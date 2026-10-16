// p2pcp: 打洞直连传文件。两端各执行一条命令：经会合服务器交换地址 → 打洞 → 在打通的同一个 UDP socket 上跑 QUIC。
//
//	p2pcp recv -server 8.140.18.211:7000 -room demo [-o 目录]
//	p2pcp send -server 8.140.18.211:7000 -room demo [-conns 4] 文件
//
// 会合服务器就是 punch server（REG/PEER 协议）。-conns N 会打 N 个洞、开 N 条 QUIC 连接，各传文件的一段：
// 每条连接各自做拥塞控制，N 条并行在随机丢包、单流被限速的链路上通常更快（也更不"礼貌"）。
//
// 打洞消息以 0x01 开头：quic-go 只把首字节最高两位为 0 的包交给 ReadNonQUICPacket，
// 这样 QUIC 接管 socket 之后，对方迟到的打洞包仍然能被识别并回应。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	msgPunch = "\x01PP" // 探测
	msgAck   = "\x01PA" // 回应
)

type opts struct {
	server  string
	room    string
	bind    string
	public  string
	conns   int
	out     string
	timeout time.Duration
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "send" && os.Args[1] != "recv") {
		fmt.Println("usage: p2pcp send|recv [flags] [文件]")
		os.Exit(2)
	}
	role := os.Args[1]
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.server, "server", "8.140.18.211:7000", "会合服务器 IP:端口")
	fs.StringVar(&o.room, "room", "p2pcp", "房间名，两端要一致")
	fs.StringVar(&o.bind, "bind", "", "绑定的本机 IP（开着 TUN 代理时用物理网卡地址）")
	fs.StringVar(&o.public, "public", "", "本机自己的公网 IP，作为额外候选（1:1 NAT 的云服务器用：会合服务器在本机时看到的是 127.0.0.1）")
	fs.IntVar(&o.conns, "conns", 1, "发送端：并行连接数（每条各打一个洞）")
	fs.StringVar(&o.out, "o", ".", "接收端：保存目录")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "等对方 + 打洞的超时")
	fs.Parse(os.Args[2:])
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	var err error
	if role == "send" {
		if fs.NArg() != 1 {
			log.Fatal("send 需要一个文件参数")
		}
		err = send(o, fs.Arg(0))
	} else {
		err = recv(o)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// ---------------- 会合 + 打洞 ----------------

// session 是一个打通的 UDP socket 和对方被选中的地址。
type session struct {
	conn *net.UDPConn
	peer *net.UDPAddr
	took time.Duration // 从开始注册到打通
}

// connect 在 room 里注册为 id，拿到对方候选地址后打洞。对方的候选 = 服务器看到的公网映射 + 对方自报的内网地址，
// 哪个先回应就用哪个（同一个局域网时内网地址 RTT 小，自然先回来）。
func connect(o opts, room, id string) (*session, error) {
	var ip net.IP
	if o.bind != "" {
		ip = net.ParseIP(o.bind)
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, err
	}
	c.SetReadBuffer(8 << 20)
	c.SetWriteBuffer(8 << 20)
	srv, err := net.ResolveUDPAddr("udp4", o.server)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	deadline := start.Add(o.timeout)
	cands := lanCands(c, o.bind)
	if o.public != "" {
		cands = fmt.Sprintf("%s:%d,%s", o.public, c.LocalAddr().(*net.UDPAddr).Port, cands)
	}
	reg := fmt.Sprintf("REG %s %s %s", room, id, cands)

	// 1. 注册，等 PEER
	var peerCands []*net.UDPAddr
	buf := make([]byte, 1500)
	for peerCands == nil {
		if time.Now().After(deadline) {
			c.Close()
			return nil, fmt.Errorf("%s: 等不到对方", room)
		}
		c.WriteToUDP([]byte(reg), srv)
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			f := strings.Fields(string(buf[:n]))
			if !from.IP.Equal(srv.IP) || len(f) != 4 || f[0] != "PEER" {
				continue
			}
			for _, s := range append([]string{f[2]}, strings.Split(f[3], ",")...) {
				if a, err := net.ResolveUDPAddr("udp4", s); err == nil && a.IP != nil {
					peerCands = append(peerCands, a)
				}
			}
			break
		}
	}

	// 2. 打洞：每 20ms 向所有候选发探测；收到对方探测立即回应并"触发检查"；收到回应就打通了
	var (
		mu     sync.Mutex
		chosen *net.UDPAddr
		done   = make(chan struct{})
	)
	go func() {
		triggered := map[string]bool{}
		for {
			c.SetReadDeadline(deadline)
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if os.Getenv("P2PCP_DEBUG") != "" {
				log.Printf("%s: 收到 %q 来自 %s", room, buf[:min(n, 16)], from)
			}
			switch string(buf[:min(n, 3)]) {
			case msgPunch:
				c.WriteToUDP([]byte(msgAck), from)
				if !triggered[from.String()] {
					triggered[from.String()] = true
					c.WriteToUDP([]byte(msgPunch), from)
				}
			case msgAck:
				mu.Lock()
				if chosen == nil {
					chosen = from
					close(done)
				}
				mu.Unlock()
				return
			}
		}
	}()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, a := range peerCands {
			c.WriteToUDP([]byte(msgPunch), a)
		}
		select {
		case <-done:
			c.SetReadDeadline(time.Time{})
			return &session{conn: c, peer: chosen, took: time.Since(start)}, nil
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			c.Close()
			return nil, fmt.Errorf("%s: 打洞超时，候选 %v", room, peerCands)
		}
	}
}

// lanCands 返回本机内网候选 "ip:port,..."，绑定了 IP 时只报这一个。
func lanCands(c *net.UDPConn, bind string) string {
	port := c.LocalAddr().(*net.UDPAddr).Port
	var out []string
	if bind != "" {
		out = append(out, fmt.Sprintf("%s:%d", bind, port))
	} else if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
				out = append(out, fmt.Sprintf("%s:%d", n.IP, port))
			}
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}
