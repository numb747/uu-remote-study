package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
)

const alpn = "p2pcp"

func quicConf() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:                10 * time.Second, // 两端 NAT 映射寿命最短 60 秒（docs/08），10 秒一次足够
		MaxIdleTimeout:                 30 * time.Second,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         64 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
	}
}

// header 在每条连接的流开头，告诉接收端这一段放在文件的哪里。
//
//	magic "P2PCP1" | conns u16 | idx u16 | size u64 | off u64 | len u64 | name_len u16 | name
type header struct {
	conns, idx     uint16
	size, off, len uint64
	name           string
}

func (h header) marshal() []byte {
	b := make([]byte, 0, 36+len(h.name))
	b = append(b, "P2PCP1"...)
	b = binary.BigEndian.AppendUint16(b, h.conns)
	b = binary.BigEndian.AppendUint16(b, h.idx)
	b = binary.BigEndian.AppendUint64(b, h.size)
	b = binary.BigEndian.AppendUint64(b, h.off)
	b = binary.BigEndian.AppendUint64(b, h.len)
	b = binary.BigEndian.AppendUint16(b, uint16(len(h.name)))
	return append(b, h.name...)
}

func readHeader(r io.Reader) (header, error) {
	var b [36]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return header{}, err
	}
	if string(b[:6]) != "P2PCP1" {
		return header{}, errors.New("不是 p2pcp 流")
	}
	h := header{
		conns: binary.BigEndian.Uint16(b[6:]), idx: binary.BigEndian.Uint16(b[8:]),
		size: binary.BigEndian.Uint64(b[10:]), off: binary.BigEndian.Uint64(b[18:]), len: binary.BigEndian.Uint64(b[26:]),
	}
	name := make([]byte, binary.BigEndian.Uint16(b[34:]))
	if _, err := io.ReadFull(r, name); err != nil {
		return header{}, err
	}
	h.name = filepath.Base(string(name)) // 不信任对方给的路径
	return h, nil
}

// answerPunch 在 QUIC 接管 socket 后继续回应对方迟到的打洞探测：对方可能还没收到我们的回应。
func answerPunch(ctx context.Context, tr *quic.Transport) {
	buf := make([]byte, 1500)
	for {
		n, from, err := tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if string(buf[:min(n, 3)]) == msgPunch {
			tr.WriteTo(pktAck, from)
		}
	}
}

// takeOver 让 QUIC 接管打通的 socket，后台继续回应迟到的打洞探测；release 回收 socket。
func takeOver(s *session) (tr *quic.Transport, release func()) {
	tr = &quic.Transport{Conn: s.conn}
	ctx, cancel := context.WithCancel(context.Background())
	go answerPunch(ctx, tr)
	return tr, func() { cancel(); tr.Close() }
}

// live 是所有活着的 QUIC 连接。进程收到 SIGTERM / SIGINT 时逐个主动关闭（closeLive），
// 对方马上就知道，不用等 30 秒空闲超时（隧道断了重连从 31 秒缩到 1 秒以内，docs/09 第 9 节）
var live sync.Map

// adopt 登记 conn；连接结束时注销并回收它独占的 socket。
func adopt(conn *quic.Conn, release func()) {
	live.Store(conn, struct{}{})
	go func() {
		<-conn.Context().Done()
		live.Delete(conn)
		release()
	}()
}

func closeLive() {
	live.Range(func(k, _ any) bool {
		k.(*quic.Conn).CloseWithError(0, "对方退出")
		return true
	})
}

// dialPeer 以发送端身份在 room 里打洞，然后在同一个 socket 上 QUIC Dial。
func dialPeer(o opts, room, alpn string) (*quic.Conn, *session, error) {
	s, err := connect(o, room, "send")
	if err != nil {
		return nil, nil, err
	}
	tr, release := takeOver(s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, s.peer, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}, quicConf())
	if err != nil {
		release()
		return nil, s, fmt.Errorf("打通 %s 后 QUIC 握手: %w", s.peer, err)
	}
	adopt(conn, release)
	return conn, s, nil
}

// acceptPeer 以接收端身份在 room 里打洞，然后在同一个 socket 上等对方 QUIC Dial 过来。
func acceptPeer(o opts, room string, tc *tls.Config) (*quic.Conn, *session, error) {
	s, err := connect(o, room, "recv")
	if err != nil {
		return nil, nil, err
	}
	tr, release := takeOver(s)
	ln, err := tr.Listen(tc, quicConf())
	if err != nil {
		release()
		return nil, s, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := ln.Accept(ctx)
	if err != nil {
		release()
		return nil, s, fmt.Errorf("打通 %s 后等 QUIC 连接: %w", s.peer, err)
	}
	adopt(conn, release)
	return conn, s, nil
}

// partErr：打通之后的错误带上连接序号；没打通的（connect 的分层诊断）原样返回，各条连接报的是同一件事
func partErr(i int, s *session, err error) error {
	if s == nil {
		return err
	}
	return fmt.Errorf("[%d] %w", i, err)
}

func roomOf(o opts, i int) string { return fmt.Sprintf("%s.%d", o.room, i) }

func statsLine(c *quic.Conn) string {
	st := c.ConnectionStats()
	lossPct := 0.0
	if st.PacketsSent > 0 {
		lossPct = float64(st.PacketsLost) / float64(st.PacketsSent) * 100
	}
	return fmt.Sprintf("SRTT %v  MinRTT %v  发包 %d  判定丢失 %d (%.2f%%)",
		st.SmoothedRTT.Round(100*time.Microsecond), st.MinRTT.Round(100*time.Microsecond), st.PacketsSent, st.PacketsLost, lossPct)
}

// ---------------- send ----------------

// meter 统计已传字节，每 500ms 报一次进度。
type meter struct {
	n atomic.Uint64
}

func (m *meter) run(rep *reporter, dir string, total uint64, name string) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		var last uint64
		for {
			select {
			case <-done:
				return
			case <-t.C:
				g := m.n.Load()
				if g > 0 {
					rep.progress(dir, g, total, float64(g-last)*8/0.5/1e6, name)
				}
				last = g
			}
		}
	}()
	return func() { close(done) }
}

type countingReader struct {
	r io.Reader
	m *meter
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.m.n.Add(uint64(n))
	return n, err
}

func send(o opts, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := uint64(fi.Size())
	n := max(o.conns, 1)
	if size < 1<<20 {
		n = 1 // 小文件多开连接只会多花打洞时间
	}
	o.rep.state(fmt.Sprintf("连接对方：%s（%.1fMB，%d 条连接，拥塞控制 %s）", fi.Name(), float64(size)/1e6, n, o.cc))

	chunk := (size + uint64(n) - 1) / uint64(n)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		start time.Time // 第一条连接开始发数据的时刻
		m     meter
	)
	stop := m.run(o.rep, "send", size, fi.Name())
	for i := range n {
		off := min(uint64(i)*chunk, size)
		h := header{conns: uint16(n), idx: uint16(i), size: size, off: off, len: min(chunk, size-off), name: fi.Name()}
		wg.Go(func() {
			if err := sendPart(o, f, h, &mu, &start, &m); err != nil {
				mu.Lock()
				first = cmp(first, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	stop()
	if first != nil {
		return first
	}
	o.rep.done("send", size, time.Since(start), fi.Name())
	return nil
}

func cmp(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func sendPart(o opts, f *os.File, h header, mu *sync.Mutex, start *time.Time, m *meter) error {
	conn, s, err := dialPeer(o, roomOf(o, int(h.idx)), alpn)
	if err != nil {
		return partErr(int(h.idx), s, err)
	}
	defer conn.CloseWithError(0, "")
	log.Printf("[%d] 打通 %s，用时 %v", h.idx, s.peer, s.took.Round(time.Millisecond))
	if h.idx == 0 {
		o.rep.state("已打通 " + s.peer.String())
	}
	if err := applyCC(conn, o.cc); err != nil {
		return err
	}
	ctx := context.Background()
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	mu.Lock()
	if start.IsZero() {
		*start = time.Now()
	}
	mu.Unlock()
	t0 := time.Now()
	if _, err := str.Write(h.marshal()); err != nil {
		return err
	}
	if _, err := io.Copy(str, countingReader{io.NewSectionReader(f, int64(h.off), int64(h.len)), m}); err != nil {
		return fmt.Errorf("[%d] 发送: %w", h.idx, err)
	}
	str.Close()
	// 等接收端写完盘回 "OK"，这样计时包含了最后一个字节真正到达
	ok := make([]byte, 2)
	if _, err := io.ReadFull(str, ok); err != nil || string(ok) != "OK" {
		return fmt.Errorf("[%d] 没等到确认: %v", h.idx, err)
	}
	el := time.Since(t0)
	log.Printf("[%d] %.1fMB 用时 %v（%.1f Mbps）  %s", h.idx, float64(h.len)/1e6, el.Round(time.Millisecond),
		float64(h.len)*8/el.Seconds()/1e6, statsLine(conn))
	return nil
}

// ---------------- recv ----------------

// recv 收一个文件。第 0 条连接按 o.timeout 等对方（daemon 下一直等），其余连接在读到第 0 条的头之后再去接。
func recv(o opts) error {
	tc, err := selfSigned()
	if err != nil {
		return err
	}
	var (
		out      *os.File
		outPath  string
		size     uint64
		name     string
		openOnce sync.Once
		openErr  error
		first    time.Time
		mu       sync.Mutex
		m        meter
		stop     = func() {}
	)
	var (
		wg   sync.WaitGroup
		errs = make(chan error, 256)
		part func(i int) error
	)
	spawn := func(i int) {
		wg.Go(func() {
			if err := part(i); err != nil {
				errs <- err
			}
		})
	}
	// part 接一条连接：打洞 → QUIC → 读头 → 写到文件对应位置。第 0 条连接的头里有总连接数，读到后立即去接其余几条
	// （发送端那几条正在会合服务器上等着）。
	part = func(i int) error {
		po := o
		if i > 0 {
			po.timeout, po.regEvery = 30*time.Second, 500*time.Millisecond
		}
		conn, s, err := acceptPeer(po, roomOf(o, i), tc)
		if err != nil {
			return partErr(i, s, err)
		}
		defer conn.CloseWithError(0, "")
		log.Printf("[%d] 打通 %s，用时 %v", i, s.peer, s.took.Round(time.Millisecond))
		if i == 0 {
			o.rep.state("已打通 " + s.peer.String())
		}
		str, err := conn.AcceptStream(context.Background())
		if err != nil {
			return err
		}
		h, err := readHeader(str)
		if err != nil {
			return err
		}
		if i == 0 {
			for j := 1; j < int(h.conns); j++ {
				spawn(j)
			}
		}
		openOnce.Do(func() {
			size, name = h.size, h.name
			if openErr = os.MkdirAll(o.out, 0o755); openErr != nil {
				return
			}
			outPath = uniquePath(o.out, h.name)
			if out, openErr = os.Create(outPath); openErr == nil {
				openErr = out.Truncate(int64(h.size))
			}
			o.rep.state(fmt.Sprintf("接收 %s（%.1fMB）", h.name, float64(h.size)/1e6))
			stop = m.run(o.rep, "recv", size, name)
		})
		if openErr != nil {
			return openErr
		}
		mu.Lock()
		if first.IsZero() {
			first = time.Now()
		}
		mu.Unlock()
		w := io.NewOffsetWriter(out, int64(h.off))
		buf := make([]byte, 256<<10)
		var n uint64
		for n < h.len {
			k, err := str.Read(buf)
			if k > 0 {
				if _, werr := w.Write(buf[:k]); werr != nil {
					return werr
				}
				n += uint64(k)
				m.n.Add(uint64(k))
			}
			if err != nil {
				if err == io.EOF && n == h.len {
					break
				}
				return fmt.Errorf("[%d] 接收: %w（收到 %d/%d）", i, err, n, h.len)
			}
		}
		str.Write([]byte("OK"))
		str.Close()
		log.Printf("[%d] 收完 %.1fMB  %s", i, float64(n)/1e6, statsLine(conn))
		time.Sleep(300 * time.Millisecond) // 让 OK 送达再关连接
		return nil
	}

	spawn(0)
	wg.Wait()
	stop()
	close(errs)
	if err := <-errs; err != nil {
		if out != nil {
			out.Close()
			os.Remove(outPath) // 没收全的文件不留
		}
		return err
	}
	el := time.Since(first)
	out.Close()
	o.rep.done("recv", size, el, outPath)
	return nil
}

// uniquePath 在 dir 里给 name 找一个不冲突的路径：a.mp4 → a (1).mp4 → a (2).mp4 …
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

func selfSigned() (*tls.Config, error) {
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
