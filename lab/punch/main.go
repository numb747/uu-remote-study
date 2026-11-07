// punch: 最小化的 UDP 打洞实验 —— 一个会合(rendezvous)服务器 + 两个对等端。
// 这就是 ICE/UU远程/RustDesk/Tailscale 打洞的核心骨架，去掉了加密和认证；server 带一个最简单的中继（-relay）。
//
//	服务器 (需要公网 IP，例如一台 VPS):
//	  go run . server -listen :7000          （可以听多个端口，房间共享：-listen :7000,:7001；-relay :3478 开中继）
//	两个对等端 (分别在两个不同 NAT 后面):
//	  go run . peer -server VPS:7000 -room demo -id A
//	  go run . peer -server VPS:7000 -room demo -id B
//	对称 NAT 可加 -spray 50 尝试向预测端口附近喷洒。
//
// 两个增强手段（见 docs/06）:
//
//	-ttl 2            低 TTL 预热：前几轮 PUNCH 用小 TTL 发，只在本方 NAT 上建映射、到不了对方 NAT，
//	                  避免对方 NAT 因先收到我们的包而记下冲突的 conntrack、给对方换端口。
//	-bday-sockets 256 生日攻击·对称端：额外开 N 个 socket 各自向对端发包，在本方 NAT 上开出 N 个随机端口。
//	-bday-probes 1024 生日攻击·锥形端：向对端公网 IP 的 N 个随机端口探测，命中其中任意一个即可。
//
// 协议 (纯文本 UDP):
//
//	peer→server  REG <room> <id> <host1,host2,...>   UNREG <room> <id>
//	server→peer  PEER <id> <pubaddr> <host1,host2,...>   YOU <pubaddr>（对每个 REG 的回执）
//	peer↔peer    PUNCH <id> / ACK <id> / PING <unixnano> / PONG <unixnano> / MSG <text> / KA
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: punch server|peer [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "peer":
		runPeer(os.Args[2:])
	default:
		fmt.Println("usage: punch server|peer [flags]")
		os.Exit(2)
	}
}

// ---------------- rendezvous server ----------------
//
// 两版协议同时支持，房间互不相通：
//
//	v1（punch peer、旧版 p2pcp）纯文本：REG/UNREG → YOU/PEER，同一房间里所有人两两配对
//	v2（p2pcp）每条报文以 "/" 开头，这样 QUIC 接管 socket 之后，客户端还能经 ReadNonQUICPacket 收到（连接迁移要用）：
//	  → /R2 <房间号> <角色.随机数> <候选|-> <标志|->     /U2 <房间号> <id>
//	  ← /Y <公网映射>     /P <对方 id> <对方公网映射> <对方候选|-> <对方标志|->
//	  只给角色（id 里 "." 前面那段）不同的两人配对：同一房间可以有多个发送端，它们之间不互相打洞
//
// 中继（-relay，默认 UDP 3478）：两端在打洞失败后各发 /L <中继键> <d|l>，凑齐两端后回 /LK，此后从一端来的包原样转给另一端。
// 中继只是转发 UDP，QUIC 的加密和认证仍在两端之间（p2pcp auth.go），服务器看不到内容。

type member struct {
	pub, lan string
	flags    string // v2：对方要求的方式，比如 relay
	seen     time.Time
	conn     *net.UDPConn // 它是从哪个监听端口进来的：回包必须从同一个端口发，否则过不了对方 NAT 的过滤
}

// role：v2 的 id 是 "角色.随机数"，v1 的 id 整个就是角色（两两配对，只要 id 不同）
func role(id string) string {
	r, _, _ := strings.Cut(id, ".")
	return r
}

// limiter 按源 IP 限速：会合和中继的控制报文每秒 20 个、突发 60 个，挡住刷房间、借服务器反射流量
type limiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	warned bool
}

func (l *limiter) allow(ip net.IP) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	k := ip.String()
	b := l.m[k]
	if b == nil {
		b = &bucket{tokens: 60, last: now}
		l.m[k] = b
	}
	b.tokens = min(60, b.tokens+now.Sub(b.last).Seconds()*20)
	b.last = now
	if b.tokens < 1 {
		if !b.warned {
			b.warned = true
			log.Printf("限速 %s：控制报文超过每秒 20 个", k)
		}
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if time.Since(b.last) > time.Minute {
			delete(l.m, k)
		}
	}
}

// runServer 可以同时听多个端口（-listen :7000,:7001），房间是共享的：两端从不同端口注册也能配上对。
// 某个端口被运营商拦了（docs/09 第 7 节），客户端换个端口就行，不用两端约好同一个端口。
func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", ":7000", "UDP 监听地址，逗号分隔可以听多个")
	relay := fs.String("relay", "", "中继 UDP 监听地址，如 :3478（空 = 不开中继）")
	fs.Parse(args)

	var (
		mu    sync.Mutex
		rooms = map[string]map[string]*member{}
		lim   = &limiter{m: map[string]*bucket{}}
	)
	// 清理：会合只在头几秒有用，5 分钟没再注册的成员、空房间都删掉，常驻服务的内存才不会一直涨
	go func() {
		for range time.Tick(time.Minute) {
			mu.Lock()
			for r, m := range rooms {
				for id, o := range m {
					if time.Since(o.seen) > 5*time.Minute {
						delete(m, id)
					}
				}
				if len(m) == 0 {
					delete(rooms, r)
				}
			}
			mu.Unlock()
			lim.gc()
		}
	}()

	// register 记下成员并和房间里的其他人互换地址。peer 生成发给某一方的 PEER 报文
	register := func(conn *net.UDPConn, from *net.UDPAddr, room, id, lan, flags string, peer func(id, pub, lan, flags string) string) {
		mu.Lock()
		defer mu.Unlock()
		if rooms[room] == nil {
			rooms[room] = map[string]*member{}
		}
		m := rooms[room]
		if old, ok := m[id]; !ok || old.pub != from.String() {
			// 服务器看到的源地址 = 对方 NAT 分配的公网映射，这正是 STUN 做的事。
			// v2 的房间号是房间名的 HMAC，日志里不会出现房间名
			log.Printf("room=%s id=%s pub=%s lan=%s flags=%s via=%s", room, id, from, lan, flags, conn.LocalAddr())
		}
		m[id] = &member{pub: from.String(), lan: lan, flags: flags, seen: time.Now(), conn: conn}
		for oid, o := range m {
			if role(oid) == role(id) || time.Since(o.seen) > 30*time.Second {
				continue
			}
			// 双向交换：两边几乎同时拿到对方地址，同时开始往外发包 —— "同时打开"
			conn.WriteToUDP([]byte(peer(oid, o.pub, o.lan, o.flags)), from)
			o.conn.WriteToUDP([]byte(peer(id, from.String(), lan, flags)), mustResolve(o.pub))
		}
	}
	unregister := func(room, id string) { // 已经拿到对方地址，别再把自己推给后来者
		mu.Lock()
		delete(rooms[room], id)
		mu.Unlock()
	}

	serve := func(conn *net.UDPConn) {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			if !lim.allow(from.IP) {
				continue
			}
			f := strings.Fields(string(buf[:n]))
			switch {
			case len(f) == 3 && f[0] == "UNREG":
				unregister(f[1], f[2])
			case len(f) == 4 && f[0] == "REG":
				// 告诉对方它的公网映射：客户端据此发现"分到的端口进不来"（比如运营商 CGNAT 分了被拦截的低端口，docs/09）
				conn.WriteToUDP([]byte("YOU "+from.String()), from)
				register(conn, from, f[1], f[2], f[3], "", func(id, pub, lan, _ string) string {
					return fmt.Sprintf("PEER %s %s %s", id, pub, lan)
				})
			case len(f) == 3 && f[0] == "/U2":
				unregister("v2|"+f[1], f[2])
			case len(f) == 5 && f[0] == "/R2" && len(f[1]) <= 64 && len(f[2]) <= 40:
				conn.WriteToUDP([]byte("/Y "+from.String()), from)
				register(conn, from, "v2|"+f[1], f[2], f[3], f[4], func(id, pub, lan, flags string) string {
					return fmt.Sprintf("/P %s %s %s %s", id, pub, lan, flags)
				})
			}
		}
	}
	if *relay != "" {
		go runRelay(*relay, lim)
	}
	addrs := strings.Split(*listen, ",")
	for i, a := range addrs {
		conn, err := net.ListenUDP("udp", mustResolve(a))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("rendezvous listening on %s", conn.LocalAddr())
		if i == len(addrs)-1 {
			serve(conn)
		} else {
			go serve(conn)
		}
	}
}

// relaySess 是一对中继端点：d = 发起连接的一端，l = 等连接的一端
type relaySess struct {
	key    string
	d, l   *net.UDPAddr
	seen   time.Time
	paired bool
}

// runRelay 打洞兜底：凑齐一对就互相转发。服务器出方向只有 3M（PROGRESS 0.4），中继只保证"连得上"，
// 够 ssh 交互、不够传大文件；连上之后 p2pcp 会在后台继续尝试打洞，打通了就把连接迁移到直连上（tunnel.go）
func runRelay(addr string, lim *limiter) {
	conn, err := net.ListenUDP("udp", mustResolve(addr))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("relay listening on %s", conn.LocalAddr())
	var (
		mu       sync.Mutex
		sessions = map[string]*relaySess{}
		byAddr   = map[string]*relaySess{} // 端点地址 → 所在的一对
	)
	go func() {
		for range time.Tick(30 * time.Second) {
			mu.Lock()
			for k, s := range sessions {
				if time.Since(s.seen) > time.Minute { // 两边都没包了：连接已经关了或者迁到直连上了
					delete(sessions, k)
					for _, a := range []*net.UDPAddr{s.d, s.l} {
						if a != nil && byAddr[a.String()] == s {
							delete(byAddr, a.String())
						}
					}
					log.Printf("relay %s 结束", k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if n > 3 && string(buf[:3]) == "/L " {
			if !lim.allow(from.IP) {
				continue
			}
			f := strings.Fields(string(buf[:n]))
			if len(f) != 3 || len(f[1]) > 128 || f[2] != "d" && f[2] != "l" {
				continue
			}
			mu.Lock()
			s := sessions[f[1]]
			if s == nil {
				if len(sessions) >= 256 {
					mu.Unlock()
					continue
				}
				s = &relaySess{key: f[1]}
				sessions[f[1]] = s
			}
			end := &s.d
			if f[2] == "l" {
				end = &s.l
			}
			if *end == nil || (*end).String() != from.String() {
				if *end != nil {
					delete(byAddr, (*end).String())
				}
				*end = from
				byAddr[from.String()] = s
			}
			s.seen = time.Now()
			if s.d != nil && s.l != nil {
				if !s.paired {
					s.paired = true
					log.Printf("relay %s: %s ↔ %s", s.key, s.d, s.l)
				}
				// 两边一起通知：免得先收到 /LK 的一端发出的 QUIC 首包，被还在等 /LK 的另一端当成控制报文吞掉
				conn.WriteToUDP([]byte("/LK"), s.d)
				conn.WriteToUDP([]byte("/LK"), s.l)
			}
			mu.Unlock()
			continue
		}
		mu.Lock()
		s := byAddr[from.String()]
		var to *net.UDPAddr
		if s != nil && s.d != nil && s.l != nil {
			s.seen = time.Now()
			to = s.l
			if from.String() == s.l.String() {
				to = s.d
			}
		}
		mu.Unlock()
		if to != nil {
			conn.WriteToUDP(buf[:n], to)
		}
	}
}

// ---------------- peer ----------------

const normalTTL = 64

// sock 包装一个 UDP socket，让多个 goroutine 能安全地以不同 TTL 往里写。
// TTL 是 socket 级选项："改 TTL → 发包 → 下次再改"必须互斥，
// 否则接收循环回的 ACK 恰好落在预热窗口里，就会被带成低 TTL、到不了对方。
type sock struct {
	*net.UDPConn
	mu  sync.Mutex
	ttl int
}

func (s *sock) send(msg string, to *net.UDPAddr, ttl int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl == 0 {
		ttl = normalTTL
	}
	if ttl != s.ttl {
		if err := setTTL(s.UDPConn, ttl); err != nil {
			log.Printf("设置 TTL=%d 失败: %v", ttl, err)
		}
		s.ttl = ttl
	}
	s.WriteToUDP([]byte(msg), to)
}

func runPeer(args []string) {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	server := fs.String("server", "127.0.0.1:7000", "会合服务器地址")
	room := fs.String("room", "demo", "房间名")
	id := fs.String("id", "A", "本端 ID")
	port := fs.Int("port", 0, "本地 UDP 端口 (0=随机)")
	spray := fs.Int("spray", 0, "对称 NAT: 向对端公网端口 ±N 范围喷洒")
	timeout := fs.Duration("timeout", 15*time.Second, "打洞超时")
	ttl := fs.Int("ttl", 0, "低 TTL 预热：前 -warm 轮 PUNCH 用这个 TTL 发 (0=关闭)")
	warm := fs.Int("warm", 3, "预热轮数（每轮 200ms）")
	bdaySockets := fs.Int("bday-sockets", 0, "生日攻击·对称端：额外开 N 个 socket")
	bdayProbes := fs.Int("bday-probes", 0, "生日攻击·锥形端：向对端公网 IP 的 N 个随机端口探测")
	fs.Parse(args)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	base := &sock{UDPConn: conn}
	srv := mustResolve(*server)
	lan := hostCandidates(conn.LocalAddr().(*net.UDPAddr).Port)
	log.Printf("local %s, registering to %s", lan, srv)

	// 生日攻击·对称端：每个 socket 在本方 NAT 上占一个随机的公网端口
	var extras []*sock
	for i := 0; i < *bdaySockets; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			log.Fatal(err)
		}
		extras = append(extras, &sock{UDPConn: c})
	}

	var (
		mu         sync.Mutex
		candidates []*net.UDPAddr
		probes     []*net.UDPAddr
		peerPub    *net.UDPAddr
		selSock    *sock
		selected   *net.UDPAddr
		peerID     string
		connected  = make(chan struct{})
		once       sync.Once
		pongs      = make(chan time.Duration, 64)
	)
	choose := func(s *sock, a *net.UDPAddr, why string) {
		once.Do(func() {
			mu.Lock()
			selSock, selected = s, a
			mu.Unlock()
			kind := "公网 (打洞成功)"
			if a.IP.IsPrivate() || a.IP.IsLoopback() {
				kind = "局域网直连"
			}
			via := ""
			if s != base {
				via = fmt.Sprintf(", 经生日 socket %s", s.LocalAddr())
			}
			log.Printf("✔ connected to %s via %s [%s, %s%s]", peerID, a, kind, why, via)
			close(connected)
		})
	}

	// 接收循环：服务器与对端的报文走同一个 socket —— 打洞必须复用同一个端口
	readLoop := func(s *sock) {
		buf := make([]byte, 1500)
		for {
			n, from, err := s.ReadFromUDP(buf)
			if err != nil {
				return
			}
			msg := string(buf[:n])
			f := strings.Fields(msg)
			if len(f) == 0 {
				continue
			}
			switch f[0] {
			case "PEER":
				if len(f) != 4 || s != base {
					continue
				}
				mu.Lock()
				if peerID == "" {
					peerID = f[1]
					pub := mustResolve(f[2])
					peerPub = pub
					log.Printf("peer %s: pub=%s host=%s", f[1], pub, f[3])
					candidates = append(candidates, pub)
					for _, h := range strings.Split(f[3], ",") {
						if h != pub.String() {
							candidates = append(candidates, mustResolve(h))
						}
					}
					for d := -*spray; d <= *spray; d++ {
						if d != 0 && pub.Port+d > 0 && pub.Port+d < 65536 {
							candidates = append(candidates, &net.UDPAddr{IP: pub.IP, Port: pub.Port + d})
						}
					}
					// 生日攻击·锥形端：对称 NAT 的端口一般落在 1024~65535
					for _, p := range rand.Perm(65536 - 1024)[:min(*bdayProbes, 65536-1024)] {
						probes = append(probes, &net.UDPAddr{IP: pub.IP, Port: 1024 + p})
					}
				}
				mu.Unlock()
			case "PUNCH":
				s.send("ACK "+*id, from, 0)
				choose(s, from, "收到对方 PUNCH")
			case "ACK":
				choose(s, from, "收到 ACK")
			case "PING":
				s.send("PONG "+f[1], from, 0)
			case "PONG":
				if t, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					pongs <- time.Since(time.Unix(0, t))
				}
			case "MSG":
				fmt.Printf("\r<%s> %s\n> ", peerID, strings.TrimPrefix(msg, "MSG "))
			}
		}
	}
	go readLoop(base)
	for _, s := range extras {
		go readLoop(s)
	}

	// 注册 + 打洞循环
	const probesPerTick = 128 // 生日探测限速：每 200ms 最多 128 个，预算 1024 个约 1.6 秒发完
	deadline := time.Now().Add(*timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	round, next := 0, 0
loop:
	for {
		select {
		case <-connected:
			break loop
		case <-tick.C:
			if time.Now().After(deadline) {
				log.Fatal("✘ 打洞超时：双方可能都是对称 NAT 或 UDP 被封，现实中这里会降级到 TURN 中继")
			}
			base.send(fmt.Sprintf("REG %s %s %s", *room, *id, lan), srv, 0)
			mu.Lock()
			cands, pub, prb := candidates, peerPub, probes
			mu.Unlock()
			if pub == nil {
				continue
			}
			round++
			t := 0
			if *ttl > 0 && round <= *warm {
				t = *ttl
			}
			for _, c := range cands {
				base.send("PUNCH "+*id, c, t)
			}
			// 对称端的额外 socket 只需要把映射开出来并保持住；真正的连通靠锥形端的探测打进来，
			// 所以开着预热时它们一直用低 TTL，免得在对方 NAT 上留下记录
			for _, s := range extras {
				s.send("PUNCH "+*id, pub, *ttl)
			}
			if round > *warm || *ttl == 0 {
				// 探测数是总预算：发完就停。重复探测同一批端口不会提高命中率，只会多发包
				for i := 0; i < probesPerTick && next < len(prb); i++ {
					base.send("PUNCH "+*id, prb[next], 0)
					next++
				}
			}
		}
	}
	tick.Stop()

	mu.Lock()
	s, peer := selSock, selected
	mu.Unlock()
	for _, e := range extras {
		if e != s {
			e.Close()
		}
	}

	// 测 RTT
	var rtts []time.Duration
	for i := 0; i < 20; i++ {
		s.send(fmt.Sprintf("PING %d", time.Now().UnixNano()), peer, 0)
		select {
		case r := <-pongs:
			rtts = append(rtts, r)
		case <-time.After(time.Second):
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(rtts) > 0 {
		sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
		log.Printf("RTT: min %v  p50 %v  max %v  loss %d/20",
			rtts[0].Round(time.Microsecond*100), rtts[len(rtts)/2].Round(time.Microsecond*100),
			rtts[len(rtts)-1].Round(time.Microsecond*100), 20-len(rtts))
	}

	// NAT 映射会过期（常见 30s~5min），必须保活
	go func() {
		for range time.Tick(15 * time.Second) {
			s.send("KA", peer, 0)
		}
	}()
	fmt.Print("聊天模式，输入文字回车发送 (Ctrl-C 退出)\n> ")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		s.send("MSG "+sc.Text(), peer, 0)
		fmt.Print("> ")
	}
}

func mustResolve(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp4", s)
	if err != nil {
		log.Fatalf("resolve %s: %v", s, err)
	}
	return a
}

// hostCandidates 像 ICE 一样收集本机所有网卡的 IPv4 地址作为 host 候选。
// 不能只取"默认路由的地址"：开着 TUN 代理 (sing-box/Clash) 时那是虚拟网卡地址，对端根本不可达。
func hostCandidates(port int) string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, fmt.Sprintf("%s:%d", ipn.IP, port))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("127.0.0.1:%d", port))
	}
	return strings.Join(out, ",")
}
