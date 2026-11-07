package main

// 端口转发：打洞后的 QUIC 连接上，每条流对应一个 TCP 连接。用法见 main.go 开头。
//
// 典型用途是让 rsync/ssh 走打洞直连（docs/09 第 9 节）：RustDesk 的端口转发打不通时退回自建中继，被 99-server 出方向
// 3M 带宽卡死；这里打洞直连，rsync 的增量和断点续传照旧由它自己负责。
//
// 转发目标由 expose 那端的 -to 决定，forward 端选不了别的端口；对方要先通过房间密钥认证（auth.go）才能开流。
//
// 健壮性：
//   - expose 接上一个对端就回房间接着等（keepServing）：可以同时服务好几个 forward，forward 重启、换网后重新连过来，
//     不用等旧连接超时。
//   - 连接迁移（keepPath）：forward 端（QUIC 客户端）发现路径断了（5 秒没收到对方任何包）、本机换了网络、或者当前走的是
//     中继，就开一个新 socket，经会合服务器和 expose 端在"迁移房间"里再打一个洞，用 QUIC 连接迁移（PATH_CHALLENGE 验证
//     新路径后切过去）把整条连接搬到新路径上——上面的 ssh 会话不断。
//     迁移房间号由这条连接的 EKM 派生，只有两端知道；expose 端在连接所在的 socket 上每 5 秒注册一次（helpMigrate），
//     会合报文以 "/" 开头，QUIC 接管的 socket 也收得到。
//     只支持 IPv4 上的连接（会合服务器只有 IPv4；走 IPv6 直连的连接断了就重新打洞）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
)

// expose 常驻：在房间里等对方，打通后把对方开的每条流接到 o.to。
func expose(o opts) error {
	keepServing(o, fmt.Sprintf("等待对方连接（转发到 %s）", o.to),
		func(o opts) (*link, error) { return acceptPeer(o, tunALPN) },
		func(l *link) { serveTunnel(o, l) })
	return nil
}

func serveTunnel(o opts, l *link) {
	defer l.close()
	conn := l.conn
	logLinked(l)
	if err := applyCC(conn, o.cc); err != nil {
		log.Print(err)
		return
	}
	if l.session.conn.LocalAddr().(*net.UDPAddr).IP.To4() != nil {
		go helpMigrate(l)
	}
	go followMigration(conn)
	go func() { // 心跳原样弹回去
		for {
			b, err := conn.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			conn.SendDatagram(b)
		}
	}()
	for {
		str, err := conn.AcceptStream(context.Background())
		if err != nil {
			log.Printf("和 %s 的连接断开：%v  %s", conn.RemoteAddr(), err, statsLine(conn))
			return
		}
		go func() {
			c, err := net.DialTimeout("tcp", o.to, 5*time.Second)
			if err != nil {
				log.Printf("连 %s 失败: %v", o.to, err)
				str.CancelRead(1)
				str.CancelWrite(1)
				return
			}
			splice(c.(*net.TCPConn), str)
		}()
	}
}

// helpMigrate（expose 端）：在连接所在的 socket 上挂在迁移房间里。forward 端换了 socket 来注册时，
// 会合服务器把它的新地址推过来，这边从连接的 socket 往那里打洞（开自家 NAT 的过滤），对方的探测由 readNonQUIC 回应
func helpMigrate(l *link) {
	mid := connSecret(l.conn, "migrate")
	if mid == "" {
		return
	}
	id := "recv." + randHex(4)
	reg := []byte(fmt.Sprintf("/R2 %s %s - -", mid, id))
	l.tr.WriteTo(reg, l.server)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	seen := map[string]time.Time{} // 对方打洞期间每 500ms 注册一次，同一个请求服务器会推好几遍，只打一次
	for {
		select {
		case <-l.conn.Context().Done():
			l.tr.WriteTo([]byte("/U2 "+mid+" "+id), l.server)
			return
		case <-t.C:
			l.tr.WriteTo(reg, l.server)
			for k, at := range seen {
				if time.Since(at) > time.Minute {
					delete(seen, k)
				}
			}
		case f := <-l.sig:
			if len(f) != 5 || f[0] != "/P" || time.Since(seen[f[1]]) < 4*time.Second {
				continue
			}
			seen[f[1]] = time.Now()
			var cands []*net.UDPAddr
			for _, s := range append([]string{f[2]}, strings.Split(f[3], ",")...) {
				if a, err := net.ResolveUDPAddr("udp4", s); err == nil && a.IP != nil {
					cands = append(cands, a)
				}
			}
			log.Printf("对方要迁移连接，往 %v 打洞", cands)
			go func() {
				end := time.Now().Add(4 * time.Second)
				for time.Now().Before(end) && l.conn.Context().Err() == nil {
					for _, a := range cands {
						l.tr.WriteTo(pktPunch, a)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
	}
}

// followMigration（expose 端）：对方换了地址时记一笔（quic-go 关了路径验证后直接跟过去，拥塞控制不受影响）
func followMigration(conn *quic.Conn) {
	last := conn.RemoteAddr().String()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-conn.Context().Done():
			return
		case <-t.C:
		}
		if now := conn.RemoteAddr().String(); now != last {
			log.Printf("对方迁移：%s → %s", last, now)
			last = now
		}
	}
}

// forward 在 o.listen 上收 TCP 连接，每个开一条流发给对方。QUIC 连接按需建立，断了下一个 TCP 连接进来时重新打洞。
func forward(o opts) error {
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	log.Printf("监听 %s，经打洞转发给房间里的 expose 端", ln.Addr())
	var (
		mu  sync.Mutex
		cur *link
	)
	// open 只在连接确实断了时重新打洞：OpenStreamSync 因为别的原因失败（比如流数到了上限）就报错，
	// 不能把还活着、上面还有别的 ssh 的连接扔掉。路径断了但连接还在时，keepPath 会把它迁到新路径上
	open := func() (*quic.Stream, error) {
		mu.Lock()
		if cur == nil || cur.conn.Context().Err() != nil {
			l, err := dialTunnel(o)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			cur = l
		}
		c := cur.conn
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return c.OpenStreamSync(ctx)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			str, err := open()
			if err != nil {
				log.Printf("连不上对方: %v", err)
				c.Close()
				return
			}
			splice(c.(*net.TCPConn), str)
		}()
	}
}

func dialTunnel(o opts) (*link, error) {
	l, err := dialPeer(o, tunALPN)
	if err != nil {
		return nil, err
	}
	logLinked(l)
	if err := applyCC(l.conn, o.cc); err != nil {
		l.close()
		return nil, err
	}
	l.closeWhenDone(l.conn)
	go keepPath(o, l)
	go func() {
		<-l.conn.Context().Done()
		log.Printf("连接断开：%v  %s", context.Cause(l.conn.Context()), statsLine(l.conn))
	}()
	return l, nil
}

// keepPath（forward 端）：盯着连接的路径，坏了、换网了、或者在走中继，就迁移到新打的洞上。
//
// 路径好坏看心跳：每秒发一个 QUIC datagram，expose 端原样弹回来，4 秒没弹回来就是断了。不能只看"收没收到包"：
// 只有本端发不出去时，对方的重传照样能进来（实验室里就是这样，晚了 15 秒才察觉）。datagram 不重传、不排队，测的就是此刻
func keepPath(o opts, l *link) {
	conn := l.conn
	if l.peer.IP.To4() == nil {
		return // IPv6 直连：会合服务器只有 IPv4，没法给迁移牵线
	}
	var pong atomic.Int64 // 最近一次收到心跳回应的时间
	pong.Store(time.Now().UnixNano())
	go func() {
		for {
			if _, err := conn.ReceiveDatagram(context.Background()); err != nil {
				return
			}
			pong.Store(time.Now().UnixNano())
		}
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var (
		lastSeen    = time.Now()
		bound       = l.session.conn.LocalAddr().(*net.UDPAddr).IP
		relayed     = l.relayed
		nextTry     time.Time
		upgradeWait = 15 * time.Second
		nextUpgrade = time.Now().Add(upgradeWait)
	)
	for {
		select {
		case <-conn.Context().Done():
			return
		case <-t.C:
		}
		conn.SendDatagram([]byte("/hb"))
		if p := time.Unix(0, pong.Load()); p.After(lastSeen) {
			lastSeen = p
		}
		var why string
		if silent := time.Since(lastSeen); silent > 4*time.Second {
			why = fmt.Sprintf("%v 没收到心跳回应", silent.Round(time.Second))
		} else if ip := defaultRouteIP(); o.bind == "auto" && ip != nil && !ip.Equal(bound) {
			why = fmt.Sprintf("本机换了网络（%v → %v）", bound, ip)
		} else if relayed && time.Now().After(nextUpgrade) {
			why = "正在走中继，试试能不能打通直连"
		}
		if why == "" || time.Now().Before(nextTry) {
			continue
		}
		if l.stranded >= 8 {
			log.Printf("迁移已失败 %d 次，不再尝试：等这条连接超时，下一个 TCP 连接进来时重新打洞", l.stranded)
			return
		}
		log.Printf("迁移连接：%s", why)
		t0 := time.Now()
		s, err := migrate(o, l, &pong)
		if err != nil {
			log.Printf("迁移失败：%v", err)
			nextTry = time.Now().Add(2 * time.Second)
			if relayed && time.Since(lastSeen) <= 4*time.Second { // 中继还好好的，只是打不通直连：拉长间隔，别一直打
				upgradeWait = min(2*upgradeWait, 10*time.Minute)
				nextUpgrade = time.Now().Add(upgradeWait)
			}
			continue
		}
		relayed, bound, lastSeen = false, s.conn.LocalAddr().(*net.UDPAddr).IP, time.Now()
		log.Printf("连接已迁移到 %s（本端 %s），用时 %v；上面的 TCP 连接不受影响", s.peer, s.conn.LocalAddr(), time.Since(t0).Round(time.Millisecond))
	}
}

// migrate 开新 socket，在迁移房间里和 expose 端打洞，再把 QUIC 连接搬到新 socket 上。
//
// 没用 quic-go 标准的迁移流程（AddPath → Probe 验证 → Switch），因为它在这里有两个坑（实验室和真实网络都踩到了）：
//   - 服务端看到新地址只发一次 PATH_CHALLENGE，客户端的 PATH_RESPONSE 却走"当前路径"——迁移就是因为它坏了，回应必丢，
//     又不重传，服务端就一直验证不了新地址；
//   - 服务端切到新路径后，不释放那条路径占的连接 ID，每迁移一次漏一个，第三次就没有 ID 可用了。
//
// 现在的做法：expose 端关掉路径验证（quicConf 的 DisablePathManager，Hysteria 2 端口跳跃用的就是它），收到新地址来的、
// 能解密的新包就直接改发往那里；forward 端把所有旧 socket 的发送改道到新 socket（quietConn.redirect），
// 于是 quic-go 以为还在走老路，包其实都从新地址出去。AddPath + 发一次探测只是为了让 quic-go 在新 socket 上登记这条连接，
// 回来的包才收得到。成没成看心跳：新路径上弹回来了就算迁过去了
func migrate(o opts, l *link, pong *atomic.Int64) (*session, error) {
	mid := connSecret(l.conn, "migrate")
	mo := o
	mo.only4, mo.relay, mo.timeout, mo.regEvery = true, "off", 5*time.Second, 500*time.Millisecond
	s, err := connectOnce(mo, l.server.String(), mid, "send."+randHex(4), false)
	if err != nil {
		return nil, err
	}
	// 新 socket 包一层地址翻译：quic-go 眼里对方地址始终是建连时那个（中继 → 直连、内网 → 公网时对方地址其实变了）
	q := &quietConn{UDPConn: s.conn, quicSees: l.peer, real: s.peer}
	q.ok.Store(true) // 这个 socket 刚打过洞，发得出去；之后的发送错误一律当丢包
	tr := &quic.Transport{Conn: q}
	ctx, cancel := context.WithCancel(context.Background())
	go readNonQUIC(ctx, tr, nil)
	drop := func() {
		cancel()
		tr.Close()
		s.conn.Close()
	}
	path, err := l.conn.AddPath(tr)
	if err != nil {
		drop()
		return nil, err
	}
	// 探测发出去之后，新 Transport 上就登记了这条连接，不能再单独关（关掉会连带销毁连接），成不成都等连接结束时一起回收。
	// 迁移失败时这就是一个暂时收不回的 socket，keepPath 限制每条连接最多攒 8 个
	l.onClose(drop)
	go func() { // 服务端不回 PATH_RESPONSE，探测必然超时；关掉路径，把它占的连接 ID 还给对方
		pctx, pcancel := context.WithTimeout(context.Background(), time.Second)
		path.Probe(pctx)
		pcancel()
		path.Close()
	}()
	prev := l.socks[len(l.socks)-1]
	redirect := func(to *quietConn) {
		for _, old := range l.socks {
			if old == to {
				old.redirect.Store(nil)
			} else {
				old.redirect.Store(to)
			}
		}
	}
	redirect(q)
	t0 := time.Now()
	for time.Since(t0) < 3*time.Second {
		l.conn.SendDatagram([]byte("/hb"))
		time.Sleep(100 * time.Millisecond)
		if time.Unix(0, pong.Load()).After(t0) {
			l.socks = append(l.socks, q)
			return s, nil
		}
	}
	redirect(prev)
	l.stranded++
	return nil, errors.New("新路径上 3 秒没收到心跳回应")
}

// splice 双向对拷，各方向读到 EOF 后只关写端（半关闭），两个方向都结束再全部关掉。
func splice(c *net.TCPConn, str *quic.Stream) {
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := io.Copy(str, c); err != nil {
			str.CancelWrite(1)
		} else {
			str.Close()
		}
	})
	wg.Go(func() {
		if _, err := io.Copy(c, str); err != nil {
			str.CancelRead(1)
		}
		c.CloseWrite()
	})
	wg.Wait()
	c.Close()
}
