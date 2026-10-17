// p2pcp: 打洞直连传文件。两端各执行一条命令：经会合服务器交换地址 → 打洞 → 在打通的同一个 UDP socket 上跑 QUIC。
//
//	p2pcp recv   -room demo [-o 目录]              收一次
//	p2pcp daemon -room demo [-o 目录]              常驻：收完一个接着等下一个
//	p2pcp send   -room demo [-conns 2] 文件...     发送（多个文件依次发）
//	p2pcp check                                   网络体检：会合端口通不通、有没有"毒端口"、NAT 映射、IPv6（check.go）
//
// 会合服务器就是 punch server（REG/PEER/UNREG 协议）。-conns N 会打 N 个洞、开 N 条 QUIC 连接，各传文件的一段。
// 默认拥塞控制是 Hysteria 的 BBR（cc.go）：5G 上的乱序会被 Cubic 误判成丢包，Cubic 时代要靠 4 条连接才能跑满，
// BBR 1~2 条就够了（docs/09 第 7 节）。
//
// 有全局 IPv6 时，IPv4 和 IPv6 同时打洞，先通的用（connectOnce）。
//
// 给 GUI 用的两个开关：-progress 在 stdout 输出机器可读的 @STATE/@PROGRESS/@DONE/@ERROR 行（手机 App 解析它），
// -notify 用 notify-send 弹桌面通知并原地更新进度（电脑端常驻服务用）。
//
// 打洞消息以 "/"（0x2f）开头：quic-go 只把首字节最高两位为 0 的包交给 ReadNonQUICPacket，
// 这样 QUIC 接管 socket 之后，对方迟到的打洞包仍然能被识别并回应。
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	msgPunch = "/PP" // 探测（按前 3 字节识别；首字节最高两位必须是 0，quic-go 才会交给 ReadNonQUICPacket）
	msgAck   = "/PA" // 回应
	msgNom   = "/PN" // 提名：发送端告诉接收端"就用这条"（两端都有 IPv4 + IPv6 两个 socket 时才需要）
)

// 默认会合地址：同一台服务器的几个端口，房间共享。7001 放前面：7000 在某台电信 CGNAT 下有毒（docs/09 第 7 节）
const defaultServers = "8.140.18.211:7001,8.140.18.211:7000"

// 实际发出的打洞包补到 48 字节，别太像异常小包。（2026-10-17 排查电信 CGNAT 下双向全丢时试过，真正的原因是会合
// 服务器用了 UDP 7000，见 docs/09 第 7 节；填充本身无害，保留）
var (
	pktPunch = pad(msgPunch)
	pktAck   = pad(msgAck)
	pktNom   = pad(msgNom)
)

func pad(m string) []byte {
	b := make([]byte, 48)
	copy(b, m)
	return b
}

type opts struct {
	server   string
	room     string
	bind     string
	public   string
	lan      string
	only4    bool // 不开 IPv6 socket
	only6    bool // 只向对方的 IPv6 候选打洞（测试用；会合仍走 IPv4）
	conns    int
	cc       string
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
	if role != "send" && role != "recv" && role != "daemon" && role != "check" {
		usage()
	}
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.server, "server", defaultServers, "会合服务器 IP:端口，逗号分隔多个时每轮换一个（服务器那边房间共享，docs/09 第 8 节）")
	fs.StringVar(&o.room, "room", "p2pcp", "房间名，两端要一致（当作共享密钥用：取一个别人猜不到的）")
	fs.StringVar(&o.bind, "bind", "", "绑定的本机 IP（开着 TUN 代理时用物理网卡地址）；auto = 每轮取 main 表默认路由那块网卡的地址")
	fs.StringVar(&o.public, "public", "", "本机自己的公网 IP，作为额外候选（1:1 NAT 的云服务器用：会合服务器在本机时看到的是 127.0.0.1）")
	fs.StringVar(&o.lan, "lan", "", "本机内网 IP 和全局 IPv6 列表（逗号分隔），代替自动探测（Android 11+ 的 App 里 Go 读不到网卡列表）")
	fs.BoolVar(&o.only4, "4", false, "只走 IPv4")
	fs.BoolVar(&o.only6, "6", false, "只走 IPv6（两端都要有全局 IPv6）")
	fs.IntVar(&o.conns, "conns", 2, "发送端：并行连接数（每条各打一个洞；BBR 下 1~2 条就能跑满，docs/09 第 7 节）")
	fs.StringVar(&o.cc, "cc", "bbr", "发送端：拥塞控制 bbr | cubic | brutal:45M（见 cc.go）")
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
	case "check":
		err = check(o)
	case "daemon":
		// 常驻：5 秒注册一次就够了（家用路由器映射寿命 60 秒以上，docs/08）。对方一注册，服务器会立刻把 PEER 推过来
		o.timeout, o.regEvery = 0, 5*time.Second
		backoff := time.Duration(0)
		for {
			o.rep.state("等待对方发送")
			err := recv(o)
			switch {
			case err == nil:
				backoff = 0
			case errors.Is(err, errNoRendezvous):
				// 空等时连不上会合服务器（断网、换网络、服务器重启）：没人在传，不弹失败通知，只更新状态，
				// 重试间隔从 5 秒拉长到 1 分钟，网络回来后最多一分钟内自己恢复
				backoff = min(max(2*backoff, 5*time.Second), time.Minute)
				log.Print(err)
				o.rep.state(fmt.Sprintf("连不上会合服务器，%v 后重试", backoff))
				time.Sleep(backoff)
			default:
				backoff = 0
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
	fmt.Println("usage: p2pcp send|recv|daemon|check [flags] [文件...]")
	os.Exit(2)
}

// ---------------- 会合 + 打洞 ----------------

// session 是一个打通的 UDP socket 和对方被选中的地址。
type session struct {
	conn *net.UDPConn
	peer *net.UDPAddr
	took time.Duration // 从开始注册到打通（含重试）
}

// connect 在 room 里注册为 id，拿到对方候选地址后打洞；一轮打不通就换新端口重来。
//
// 偶发的"双方地址都对、两个方向却一个包都过不去"（docs/09 第 6 节）：某一侧 NAT 因为对方的包先到而留下了冲突状态，
// 给我们出去的包换了端口（docs/06 的 conntrack 冲突），这一对端口就废了。换一对新端口，冲突状态就不在了。
// 双方的单轮超时一样长、又几乎同时开始，所以会一起超时、一起重新注册，由会合服务器重新配对。
//
// 会合服务器可以给多个地址（-server a,b,c，服务器那边房间共享）：每轮换下一个，某个端口被运营商拦了（docs/09 第 7 节）
// 也只是多花一轮。哪个地址打通过就记住，下次从它开始。
func connect(o opts, room, id string) (*session, error) {
	const attempts = 6
	servers := strings.Split(o.server, ",")
	first := int(goodServer.Load())
	start := time.Now()
	var (
		err      error
		punchTOs int // 拿到了对方地址、却没打通的轮数
	)
	for i := range attempts {
		k := (first + i) % len(servers)
		var s *session
		if s, err = connectOnce(o, servers[k], room, id); err == nil {
			s.took = time.Since(start)
			goodServer.Store(int32(k))
			if i > 0 {
				log.Printf("%s: 第 %d 轮才打通（会合 %s）", room, i+1, servers[k])
			}
			return s, nil
		}
		if errors.Is(err, errPunchTimeout) {
			punchTOs++
		} else if !errors.Is(err, errBadPort) && !errors.Is(err, errLost) {
			return nil, err // 等不到对方之类的错误，重来也没用
		}
		log.Printf("%s: 第 %d 轮失败（会合 %s：%v），换端口重来", room, i+1, servers[k], err)
	}
	// 按层说清楚坏在哪：昨天能用今天不能用，多半是网络环境变了，先知道是哪一层
	switch {
	case punchTOs == attempts:
		return nil, fmt.Errorf("能会合、拿到了对方地址，但 %d 轮都没打通：两边之间的 UDP 直连被拦了（运营商换了 NAT、拦了端口，或者对称型 NAT）。"+
			"用 p2pcp check 看看是不是会合端口出了问题：%w", attempts, err)
	case punchTOs == 0:
		return nil, fmt.Errorf("%w %s：本机断网、服务器挂了，或者这些端口都被拦了（p2pcp check 逐个测）：%w", errNoRendezvous, o.server, err)
	}
	return nil, err
}

var goodServer atomic.Int32 // 上次打通用的是第几个会合地址

var (
	errPunchTimeout = errors.New("打洞超时")
	// errBadPort：会合服务器的回执没收到，或者收到了但映射端口 < 1024。电信 CGNAT 端口不够时会分 1~437 的低端口，
	// 这些端口上外面的包进不来（连会合服务器的回包都收不到），整个 socket 只能扔掉
	errBadPort = errors.New("这个端口外面进不来")
	// errLost：等对方的时候，会合服务器的回执断了。多半是网络变了（换 Wi-Fi、5G 切换、地址变了），这个 socket 的映射已经作废
	errLost = errors.New("会合服务器失联")
	// errNoRendezvous：几轮下来一次都没拿到对方地址，问题在本机到会合服务器这一段，还没轮到打洞
	errNoRendezvous = errors.New("连不上会合服务器")
)

// connectOnce 用一个新 socket 注册、拿对方候选、打洞。对方的候选 = 服务器看到的公网映射 + 对方自报的内网地址，
// 哪个先回应就用哪个（同一个局域网时内网地址 RTT 小，自然先回来）。
func connectOnce(o opts, server, room, id string) (*session, error) {
	var ip net.IP
	if o.bind == "auto" { // 每轮现取：电脑换了网络，常驻服务不用重启
		if ip = defaultRouteIP(); ip != nil {
			o.bind = ip.String()
		} else {
			o.bind = ""
		}
	} else if o.bind != "" {
		ip = net.ParseIP(o.bind)
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, err
	}
	c.SetReadBuffer(8 << 20)
	c.SetWriteBuffer(8 << 20)
	// 有全局 IPv6 就再开一个 IPv6 socket：IPv6 没有 NAT，只有运营商的状态防火墙，两边同时往外发就能打开，
	// 也不受 CGNAT 端口块、低端口这些问题影响。会合服务器只有 IPv4，IPv6 地址像内网地址一样作为自报候选交换
	var c6 *net.UDPConn
	if ip6 := global6(o); ip6 != nil && !o.only4 {
		if c6, err = net.ListenUDP("udp6", &net.UDPAddr{IP: ip6}); err == nil {
			c6.SetReadBuffer(8 << 20)
			c6.SetWriteBuffer(8 << 20)
		} else {
			log.Printf("%s: 开 IPv6 socket 失败（%v），只走 IPv4", room, err)
			c6 = nil
		}
	}
	closeAll := func() {
		c.Close()
		if c6 != nil {
			c6.Close()
		}
	}
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		closeAll()
		return nil, err
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	cands := lanCands(o, port)
	if o.public != "" {
		cands = fmt.Sprintf("%s:%d,%s", o.public, port, cands)
	}
	if c6 != nil {
		cands = c6.LocalAddr().String() + "," + cands
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
	var (
		lastReg time.Time
		lastYou time.Time
		mapped  *net.UDPAddr // 会合服务器回执里的本方公网映射
	)
	for peerCands == nil {
		if mapped == nil && time.Since(start) > 2*time.Second {
			unreg()
			closeAll()
			return nil, fmt.Errorf("%w：2 秒没收到会合服务器的回执", errBadPort)
		}
		if mapped != nil && time.Since(lastYou) > 3*o.regEvery+2*time.Second {
			unreg()
			closeAll()
			return nil, fmt.Errorf("%w：%v 没收到回执", errLost, time.Since(lastYou).Round(time.Second))
		}
		if o.timeout > 0 && time.Since(start) > o.timeout {
			unreg()
			closeAll()
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
			if !from.IP.Equal(srv.IP) {
				continue
			}
			if len(f) == 2 && f[0] == "YOU" {
				lastYou = time.Now()
				if mapped != nil {
					continue
				}
				if mapped, _ = net.ResolveUDPAddr("udp4", f[1]); mapped != nil && mapped.Port < 1024 {
					unreg()
					closeAll()
					return nil, fmt.Errorf("%w：映射到了低端口 %s", errBadPort, mapped)
				}
				continue
			}
			if len(f) != 4 || f[0] != "PEER" {
				continue
			}
			for _, s := range append([]string{f[2]}, strings.Split(f[3], ",")...) {
				a, err := net.ResolveUDPAddr("udp", s)
				if err != nil || a.IP == nil {
					continue
				}
				if v4 := a.IP.To4() != nil; v4 && o.only6 || !v4 && c6 == nil {
					continue
				}
				peerCands = append(peerCands, a)
			}
			break
		}
	}
	// 拿到地址就注销：否则服务器在 30 秒内还会把这次的旧地址推给下一次注册的人
	unreg()
	if len(peerCands) == 0 {
		closeAll()
		return nil, errors.New("没有能用的对方候选（-6 时两端都要有全局 IPv6）")
	}

	// 2. 打洞：每 20ms 向所有候选发探测；收到对方探测立即回应并"触发检查"；收到回应就打通了。
	//
	// IPv4 和 IPv6 两个 socket 同时打，两端各自"先收到回应的"未必是同一条，所以学 ICE 的提名：发送端（之后 QUIC Dial 的一方）
	// 用第一个回应的路径，并在上面发 /PN；接收端有两个 socket 时等 /PN 再定。/PN 万一全丢了，QUIC 握手包先到哪个 socket
	// 也就说明了发送端选的是哪条（那个握手包被这里吞掉，quic-go 过一个 PTO 会重发）
	punchStart := time.Now()
	deadline := punchStart.Add(4 * time.Second) // 正常 30~300ms 就通，4 秒还不通基本就是这对端口废了
	controlling := id == "send"
	waitNom := !controlling && c6 != nil
	var (
		mu       sync.Mutex
		chosen   *net.UDPAddr
		chosenOn *net.UDPConn
		done     = make(chan struct{})
	)
	choose := func(c *net.UDPConn, from *net.UDPAddr) {
		mu.Lock()
		if chosen == nil {
			chosen, chosenOn = from, c
			close(done)
		}
		mu.Unlock()
	}
	listen := func(c *net.UDPConn) {
		buf := make([]byte, 1500)
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
				c.WriteToUDP(pktAck, from)
				if !triggered[from.String()] {
					triggered[from.String()] = true
					c.WriteToUDP(pktPunch, from)
				}
			case msgAck:
				if !waitNom {
					choose(c, from)
					return
				}
			case msgNom:
				if waitNom {
					choose(c, from)
					return
				}
			default:
				if waitNom && n >= 1200 && buf[0]&0x80 != 0 { // QUIC Initial：长包头，至少补到 1200 字节
					choose(c, from)
					return
				}
			}
		}
	}
	if !o.only6 { // -6 时 IPv4 socket 只管会合，别应答打洞（否则对方的触发检查会把路径拉回 IPv4）
		go listen(c)
	}
	if c6 != nil {
		go listen(c6)
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, a := range peerCands {
			via := c
			if a.IP.To4() == nil {
				via = c6
			}
			if _, err := via.WriteToUDP(pktPunch, a); err != nil && os.Getenv("P2PCP_DEBUG") != "" {
				log.Printf("%s: 发往 %s 失败: %v", room, a, err)
			}
		}
		select {
		case <-done:
			if controlling {
				for range 3 {
					chosenOn.WriteToUDP(pktNom, chosen)
				}
			}
			if c6 != nil {
				if chosenOn == c {
					c6.Close()
				} else {
					c.Close()
				}
			}
			chosenOn.SetReadDeadline(time.Time{})
			return &session{conn: chosenOn, peer: chosen, took: time.Since(punchStart)}, nil
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			closeAll()
			return nil, fmt.Errorf("%w，对方候选 %v", errPunchTimeout, peerCands)
		}
	}
}

// global6 返回本机一个全局 IPv6 地址（不要 ULA、链路本地），没有就返回 nil。
// -lan 里给了 IPv6 就用它（Android App 由 Java 传进来）；否则问内核"去公网 IPv6 走哪个源地址"——对 UDP 做 connect
// 不发包，只查路由，拿到的就是默认出口上的地址，不会挑到 docker、虚拟网卡上的。默认 IPv6 路由被 TUN 代理接管时，
// 拿到的是 TUN 的 ULA 地址，被过滤掉，宁可不走 IPv6
func global6(o opts) net.IP {
	var ips []net.IP
	if o.lan != "" {
		for _, s := range strings.Split(o.lan, ",") {
			ips = append(ips, net.ParseIP(strings.TrimSpace(s)))
		}
	} else if c, err := net.Dial("udp6", "[2400:3200::1]:53"); err == nil { // 阿里 DNS，只用来选路
		ips = append(ips, c.LocalAddr().(*net.UDPAddr).IP)
		c.Close()
	}
	for _, ip := range ips {
		if ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return ip
		}
	}
	return nil
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
		if p := net.ParseIP(strings.TrimSpace(ip)); p != nil && p.To4() != nil { // IPv6 由 global6 单独处理
			out = append(out, fmt.Sprintf("%s:%d", p, port))
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

// defaultRouteIP 返回 main 路由表里默认路由那块网卡的 IPv4 地址（读 /proc/net/route，它只列 main 表）。
// 开着 sing-box 这类 TUN 代理时，代理的默认路由在别的策略路由表里，main 表的默认路由仍是物理网卡，正好是打洞要绑的那块
func defaultRouteIP() net.IP {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	best, bestMetric := "", 1<<30
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		var metric int
		fmt.Sscan(f[6], &metric)
		if metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	ifc, err := net.InterfaceByName(best)
	if err != nil {
		return nil
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.To4()
		}
	}
	return nil
}
