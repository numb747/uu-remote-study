// p2pcp: 打洞直连传文件。两端各执行一条命令：经会合服务器交换地址 → 打洞 → 在打通的同一个 UDP socket 上跑 QUIC。
//
//	p2pcp recv   -room demo [-o 目录]              收一次
//	p2pcp daemon -room demo [-o 目录]              常驻：收完一个接着等下一个
//	p2pcp send   -room demo [-conns 4] 文件...     发送（多个文件依次发）
//
// 会合服务器就是 punch server（REG/PEER/UNREG 协议）。-conns N 会打 N 个洞、开 N 条 QUIC 连接，各传文件的一段：
// 每条连接各自做拥塞控制，在乱序被误判成丢包的链路上（docs/09）比单条快得多。
//
// 给 GUI 用的两个开关：-progress 在 stdout 输出机器可读的 @STATE/@PROGRESS/@DONE/@ERROR 行（手机 App 解析它），
// -notify 用 notify-send 弹桌面通知并原地更新进度（电脑端常驻服务用）。
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
	server   string
	room     string
	bind     string
	public   string
	lan      string
	conns    int
	out      string
	timeout  time.Duration // 等对方的超时，0 = 一直等（daemon）
	regEvery time.Duration // 注册间隔
	rep      *reporter
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	role := os.Args[1]
	if role != "send" && role != "recv" && role != "daemon" {
		usage()
	}
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.server, "server", "8.140.18.211:7000", "会合服务器 IP:端口")
	fs.StringVar(&o.room, "room", "p2pcp", "房间名，两端要一致（当作共享密钥用：取一个别人猜不到的）")
	fs.StringVar(&o.bind, "bind", "", "绑定的本机 IP（开着 TUN 代理时用物理网卡地址）")
	fs.StringVar(&o.public, "public", "", "本机自己的公网 IP，作为额外候选（1:1 NAT 的云服务器用：会合服务器在本机时看到的是 127.0.0.1）")
	fs.StringVar(&o.lan, "lan", "", "本机内网 IP 列表（逗号分隔），代替自动探测（Android 11+ 的 App 里 Go 读不到网卡列表）")
	fs.IntVar(&o.conns, "conns", 4, "发送端：并行连接数（每条各打一个洞）")
	fs.StringVar(&o.out, "o", ".", "接收端：保存目录")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "等对方的超时（daemon 模式下无效）")
	progress := fs.Bool("progress", false, "在 stdout 输出机器可读的进度行")
	notify := fs.Bool("notify", false, "用 notify-send 弹桌面通知")
	fs.Parse(os.Args[2:])
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	o.rep = &reporter{machine: *progress, notify: *notify}
	o.regEvery = 500 * time.Millisecond

	var err error
	switch role {
	case "send":
		if fs.NArg() == 0 {
			log.Fatal("send 需要文件参数")
		}
		for _, f := range fs.Args() {
			if err = send(o, f); err != nil {
				break
			}
		}
	case "recv":
		err = recv(o)
	case "daemon":
		// 常驻：5 秒注册一次就够了（家用路由器映射寿命 60 秒以上，docs/08）。对方一注册，服务器会立刻把 PEER 推过来
		o.timeout, o.regEvery = 0, 5*time.Second
		for {
			o.rep.state("等待对方发送")
			if err := recv(o); err != nil {
				o.rep.fail(err)
				time.Sleep(time.Second)
			}
		}
	}
	if err != nil {
		o.rep.fail(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("usage: p2pcp send|recv|daemon [flags] [文件...]")
	os.Exit(2)
}

// ---------------- 会合 + 打洞 ----------------

// session 是一个打通的 UDP socket 和对方被选中的地址。
type session struct {
	conn *net.UDPConn
	peer *net.UDPAddr
	took time.Duration // 从拿到对方地址到打通
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
		c.Close()
		return nil, err
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	cands := lanCands(o, port)
	if o.public != "" {
		cands = fmt.Sprintf("%s:%d,%s", o.public, port, cands)
	}
	reg := fmt.Sprintf("REG %s %s %s", room, id, cands)
	unreg := func() {
		for range 2 {
			c.WriteToUDP([]byte("UNREG "+room+" "+id), srv)
		}
	}

	// 1. 注册，等 PEER
	var peerCands []*net.UDPAddr
	buf := make([]byte, 1500)
	start := time.Now()
	var lastReg time.Time
	for peerCands == nil {
		if o.timeout > 0 && time.Since(start) > o.timeout {
			unreg()
			c.Close()
			return nil, fmt.Errorf("等了 %v，对方没有上线", o.timeout)
		}
		if time.Since(lastReg) >= o.regEvery {
			c.WriteToUDP([]byte(reg), srv)
			lastReg = time.Now()
		}
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
	// 拿到地址就注销：否则服务器在 30 秒内还会把这次的旧地址推给下一次注册的人
	unreg()

	// 2. 打洞：每 20ms 向所有候选发探测；收到对方探测立即回应并"触发检查"；收到回应就打通了
	punchStart := time.Now()
	deadline := punchStart.Add(15 * time.Second)
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
			return &session{conn: c, peer: chosen, took: time.Since(punchStart)}, nil
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			c.Close()
			return nil, fmt.Errorf("打洞超时，对方候选 %v", peerCands)
		}
	}
}

// lanCands 返回本机内网候选 "ip:port,..."。
func lanCands(o opts, port int) string {
	var ips []string
	switch {
	case o.lan != "":
		ips = strings.Split(o.lan, ",")
	case o.bind != "":
		ips = []string{o.bind}
	default:
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
					ips = append(ips, n.IP.String())
				}
			}
		}
	}
	var out []string
	for _, ip := range ips {
		if ip = strings.TrimSpace(ip); ip != "" {
			out = append(out, fmt.Sprintf("%s:%d", ip, port))
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}
