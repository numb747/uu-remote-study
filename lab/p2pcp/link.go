package main

// link：打通的 socket 交给 QUIC 之后的那一层。一个 link = 一个打通的洞（或者一条中继）+ 上面的 quic.Transport，
// 上面可以有多条 QUIC 连接（传文件 -conns N 共用一个洞）。隧道连接迁移后还会挂上新的 Transport（tunnel.go），
// 都在 link 关闭时一起回收。
//
// 注意 quic.Transport.Close() 不会关传进去的 socket（只关它自己开的），所以 socket 要自己关；
// 而关掉 Transport 会直接销毁上面所有连接，所以要等连接都结束了再关。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apernet/quic-go"
)

const (
	alpn    = "p2pcp/2" // v2：会合协议、认证、传文件协议都和 v1 不兼容，ALPN 不同，握手阶段就会报错而不是传一半
	tunALPN = "p2pcp-tun/2"
)

func quicConf(alpn string) *quic.Config {
	c := &quic.Config{
		KeepAlivePeriod:                5 * time.Second,  // 两端 NAT 映射寿命最短 60 秒（docs/08）
		MaxIdleTimeout:                 15 * time.Second, // 对方断电、换网后 15 秒认定断开，发送端重连续传
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         64 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
		MaxIncomingStreams:             1000,
		MaxIncomingUniStreams:          1000,
		// 服务端（接收端 / expose）不做路径验证：对方换了地址（连接迁移、NAT 重新映射），收到新地址来的、能解密的
		// 新包就直接改发往那里。quic-go 自带的路径验证在这里会卡死，见 tunnel.go migrate
		DisablePathManager: true,
	}
	if alpn == tunALPN {
		// 隧道：2 秒一个保活，路径断了 forward 端最多 5 秒就能察觉、去打新洞迁移（tunnel.go）；
		// 空闲超时留够迁移的时间（打洞 0.1~4 秒 + 验证路径）
		c.KeepAlivePeriod, c.MaxIdleTimeout = 2*time.Second, 25*time.Second
		c.EnableDatagrams = true // 心跳（tunnel.go heartbeat）
	}
	return c
}

type link struct {
	*session
	secret   []byte
	alpn     string
	tr       *quic.Transport
	ln       *quic.Listener // 接收端才有
	conn     *quic.Conn     // 第一条连接
	sig      chan []string  // 经这个 socket 收到的会合报文（/Y、/P），连接迁移用
	socks    []*quietConn   // 这条连接用过的所有 socket（第一个是打洞的那个，之后每迁移一次多一个），最后一个是当前的
	stranded int            // 迁移失败、又要等连接结束才能回收的 socket 数（tunnel.go migrate）

	mu      sync.Mutex
	conns   []*quic.Conn
	cleanup []func()
	closed  bool
}

// newLink 让 QUIC 接管打通的 socket，后台继续处理非 QUIC 的包：回应对方迟到的打洞探测，会合报文转给 sig
func newLink(s *session, secret []byte, alpn string) *link {
	l := &link{session: s, secret: secret, alpn: alpn, sig: make(chan []string, 16)}
	q := &quietConn{UDPConn: s.conn}
	l.socks = []*quietConn{q}
	l.tr = &quic.Transport{Conn: q}
	ctx, cancel := context.WithCancel(context.Background())
	go readNonQUIC(ctx, l.tr, l.sig)
	l.onClose(func() {
		cancel()
		l.tr.Close()
		s.conn.Close()
	})
	return l
}

// quietConn：发包出错（网卡没了、路由没了、防火墙拒绝）时当成丢包，不报给 quic-go。quic-go 遇到任何发送错误
// （除了包太大）都会直接关掉连接，可换网的那一瞬间发包必然失败，这正是连接迁移要救的场景；传文件也一样，
// 信号抖一下不该让整个连接作废。只包写方法，其余原样转发（SyscallConn、ReadMsgUDP 等），quic-go 的 GSO、批量收包照常用
//
// 顺带做连接迁移要的地址翻译（quicSees 非空时）：quic-go 的客户端迁移只换本端 socket、不换对方地址，可我们的对方地址
// 可能变了（中继 → 直连、内网 → 公网），于是发往 quicSees 的包实际发给 real。收方向不用翻译：QUIC 客户端不看
// 来包的源地址，新路径的验证也是按 PATH_CHALLENGE 的内容对的
//
// 还有一个改道（redirect）：连接迁移后，旧 socket 要发的包都从新 socket 发出去（见 tunnel.go migrate）
type quietConn struct {
	*net.UDPConn
	ok             atomic.Bool // 成功发出过之后才吞错：第一个包的错误 quic-go 自己要用（比如判断 GSO 能不能用）
	quicSees, real *net.UDPAddr
	redirect       atomic.Pointer[quietConn]
}

func sameAddr(a, b *net.UDPAddr) bool { return a.Port == b.Port && a.IP.Equal(b.IP) }

func (c *quietConn) dst(a *net.UDPAddr) *net.UDPAddr {
	if c.quicSees != nil && a != nil && sameAddr(a, c.quicSees) {
		return c.real
	}
	return a
}

func (c *quietConn) quiet(err error) bool {
	if err == nil {
		c.ok.Store(true)
		return false
	}
	return c.ok.Load() && !errors.Is(err, syscall.EMSGSIZE) // 包太大要报上去，路径 MTU 探测靠它
}

func (c *quietConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if r := c.redirect.Load(); r != nil {
		return r.WriteMsgUDP(b, oob, addr)
	}
	n, oobn, err := c.UDPConn.WriteMsgUDP(b, oob, c.dst(addr))
	if c.quiet(err) {
		return len(b), len(oob), nil
	}
	return n, oobn, err
}

func (c *quietConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if r := c.redirect.Load(); r != nil {
		return r.WriteTo(b, addr)
	}
	if u, ok := addr.(*net.UDPAddr); ok {
		addr = c.dst(u)
	}
	n, err := c.UDPConn.WriteTo(b, addr)
	if c.quiet(err) {
		return len(b), nil
	}
	return n, err
}

func (c *quietConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if r := c.redirect.Load(); r != nil {
		return r.WriteToUDP(b, addr)
	}
	n, err := c.UDPConn.WriteToUDP(b, c.dst(addr))
	if c.quiet(err) {
		return len(b), nil
	}
	return n, err
}

func readNonQUIC(ctx context.Context, tr *quic.Transport, sig chan []string) {
	buf := make([]byte, 1500)
	for {
		n, from, err := tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		switch msg := string(buf[:n]); {
		case strings.HasPrefix(msg, msgPunch):
			tr.WriteTo(pktAck, from)
		case sig != nil && (strings.HasPrefix(msg, "/Y ") || strings.HasPrefix(msg, "/P ")):
			select {
			case sig <- strings.Fields(msg):
			default:
			}
		}
	}
}

// onClose 登记 link 关闭时要做的事，按登记的倒序执行
func (l *link) onClose(f func()) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		f()
		return
	}
	l.cleanup = append(l.cleanup, f)
	l.mu.Unlock()
}

// close 先正常关掉所有连接（对方马上知道），再回收 Transport 和 socket
func (l *link) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	conns, cleanup := l.conns, l.cleanup
	l.mu.Unlock()
	for _, c := range conns {
		c.CloseWithError(0, "")
	}
	for i := len(cleanup) - 1; i >= 0; i-- {
		cleanup[i]()
	}
}

// closeWhenDone：conn 结束时整个 link 跟着回收（隧道一个 link 只有一条连接）
func (l *link) closeWhenDone(conn *quic.Conn) {
	go func() {
		<-conn.Context().Done()
		l.close()
	}()
}

func (l *link) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *link) track(conn *quic.Conn) {
	l.mu.Lock()
	l.conns = append(l.conns, conn)
	l.mu.Unlock()
	live.Store(conn, struct{}{})
	go func() {
		<-conn.Context().Done()
		live.Delete(conn)
	}()
}

// dial 在这个洞上再开一条 QUIC 连接并认证
func (l *link) dial() (*quic.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := l.tr.Dial(ctx, l.peer, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{l.alpn}}, quicConf(l.alpn))
	if err != nil {
		return nil, fmt.Errorf("连上 %s 后 QUIC 握手: %w", l.via(), err)
	}
	if err := authConn(conn, l.secret, true); err != nil {
		conn.CloseWithError(0, "")
		return nil, err
	}
	l.track(conn)
	return conn, nil
}

// accept 等对方在这个洞上开的下一条连接并认证；timeout 为 0 时一直等到 link 关闭
func (l *link) accept(timeout time.Duration) (*quic.Conn, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("连上 %s 后等 QUIC 连接: %w", l.via(), err)
	}
	if err := authConn(conn, l.secret, false); err != nil {
		conn.CloseWithError(0, "")
		return nil, err
	}
	l.track(conn)
	return conn, nil
}

// live 是所有活着的 QUIC 连接。进程收到 SIGTERM / SIGINT 时逐个主动关闭（closeLive），
// 对方马上就知道，不用等空闲超时（docs/09 第 9 节）
var live sync.Map

func closeLive() {
	live.Range(func(k, _ any) bool {
		k.(*quic.Conn).CloseWithError(0, "对方退出")
		return true
	})
}

// dialPeer 以发送端身份在房间里打洞，然后在同一个 socket 上 QUIC Dial、认证。
func dialPeer(o opts, alpn string) (*link, error) {
	secret := roomSecret(o.room)
	s, err := connect(o, roomID(secret), "send")
	if err != nil {
		return nil, err
	}
	l := newLink(s, secret, alpn)
	if l.conn, err = l.dial(); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}

// acceptPeer 以接收端身份在房间里打洞，然后在同一个 socket 上等对方 QUIC Dial 过来、认证。
func acceptPeer(o opts, alpn string) (*link, error) {
	tc, err := selfSigned(alpn)
	if err != nil {
		return nil, err
	}
	secret := roomSecret(o.room)
	s, err := connect(o, roomID(secret), "recv")
	if err != nil {
		return nil, err
	}
	l := newLink(s, secret, alpn)
	if l.ln, err = l.tr.Listen(tc, quicConf(alpn)); err != nil {
		l.close()
		return nil, err
	}
	l.onClose(func() { l.ln.Close() })
	if l.conn, err = l.accept(15 * time.Second); err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}

func statsLine(c *quic.Conn) string {
	st := c.ConnectionStats()
	lossPct := 0.0
	if st.PacketsSent > 0 {
		lossPct = float64(st.PacketsLost) / float64(st.PacketsSent) * 100
	}
	return fmt.Sprintf("SRTT %v  MinRTT %v  发包 %d  判定丢失 %d (%.2f%%)",
		st.SmoothedRTT.Round(100*time.Microsecond), st.MinRTT.Round(100*time.Microsecond), st.PacketsSent, st.PacketsLost, lossPct)
}

func logLinked(l *link) {
	log.Printf("连上 %s，用时 %v", l.via(), l.took.Round(time.Millisecond))
}

// selfSigned：证书只是 TLS 1.3 的形式要求，不校验，身份靠 auth.go 的通道绑定
func selfSigned(alpn string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{alpn}}, nil
}
