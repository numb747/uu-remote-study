package main

// 会合 + 打洞 + 中继兜底。
//
// 会合协议 v2（punch server 那边的说明更全）：报文都以 "/" 开头，QUIC 接管 socket 之后仍能经 ReadNonQUICPacket 收发。
//
//	→ 服务器  /R2 <房间号> <角色.随机数> <候选|-> <标志|->     /U2 <房间号> <id>
//	← 服务器  /Y <本方公网映射>     /P <对方 id> <对方公网映射> <对方候选|-> <对方标志|->
//	→ 中继    /L <中继键> <d|l>     ← 中继 /LK
//
// 中继兜底：发送端（QUIC Dial 的一方）连续两轮"拿到了地址却没打通"，第三轮起在 REG 里带上 relay 标志；
// 任何一方带了这个标志，双方都改去会合服务器的中继端口（同一 IP 的 UDP 3478），QUIC 照常握手、端到端认证。

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const relayPort = 3478

// session 是一个打通的 UDP socket 和对方被选中的地址。
type session struct {
	conn    *net.UDPConn
	peer    *net.UDPAddr // 直连时是对方地址，中继时是中继地址
	server  *net.UDPAddr // 这次用的会合服务器（连接迁移时继续用它）
	relayed bool
	control bool          // 本端是 QUIC 发起方（send，或 check 里 id 大的一端）
	took    time.Duration // 从开始注册到打通（含重试）
	punch   time.Duration // 成功那一轮的打洞（或进中继）用时
	rounds  int           // 第几轮连上的
	pre     *preflight    // 这次的体检（-stun off 时为 nil）
}

func (s *session) via() string {
	if s.relayed {
		return "中继 " + s.peer.String()
	}
	return s.peer.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// connect 在房间 rid 里以 role（send | recv）注册，拿到对方候选地址后打洞；一轮打不通就换新端口重来。
//
// 偶发的"双方地址都对、两个方向却一个包都过不去"（docs/09 第 6 节）：某一侧 NAT 因为对方的包先到而留下了冲突状态，
// 给我们出去的包换了端口（docs/06 的 conntrack 冲突），这一对端口就废了。换一对新端口，冲突状态就不在了。
// 双方的单轮超时一样长、又几乎同时开始，所以会一起超时、一起重新注册，由会合服务器重新配对。
//
// 会合服务器可以给多个地址（-server a,b,c，服务器那边房间共享）：每轮换下一个，某个端口被运营商拦了（docs/09 第 7 节）
// 也只是多花一轮。哪个地址打通过就记住，下次从它开始。
func connect(o opts, rid, role string) (*session, error) {
	const attempts = 6
	if o.stun != "off" && o.stun != "" {
		o.pre = startPreflight(o, role) // 后台测，不等；传输会话结束时 settle（preflight.go）
		if o.relay == "always" {
			o.pre.setForced()
		}
	}
	servers := strings.Split(o.server, ",")
	first := int(goodServer.Load())
	start := time.Now()
	id := role + "." + randHex(4)
	if role == "check" {
		// check -room 两端跑的是同一条命令：各取一个随机角色名，会合服务器只给角色不同的两人配对，
		// 配上之后按 id 大小定谁发起（isControlling）
		id = "chk" + randHex(3) + "." + randHex(4)
	}
	var (
		err      error
		punchTOs int // 拿到了对方地址、却没打通的轮数
	)
	for i := range attempts {
		k := (first + i) % len(servers)
		// 发送端两轮打不通就请求中继（接收端看到对方的标志会跟着去，打洞中途也会跟）；
		// 体检预测打不通时，一轮不通就请求（DESIGN 决策 2：仍短打一轮，万一预测错了还能直连）
		early := punchTOs == 1 && o.pre.predictedNo()
		relay := o.relay == "always" || o.relay == "auto" && role != "recv" && (punchTOs >= 2 || early)
		if relay && early && o.relay == "auto" {
			log.Print("体检预测打不通，这一轮直接请求中继")
		}
		var s *session
		if s, err = connectOnce(o, servers[k], rid, id, relay); err == nil {
			s.punch, s.rounds = s.took, i+1 // connectOnce 填的是这一轮的打洞用时
			s.took = time.Since(start)
			s.pre = o.pre
			o.pre.setActual(s.relayed)
			goodServer.Store(int32(k))
			if i > 0 {
				log.Printf("第 %d 轮才连上（会合 %s）", i+1, servers[k])
			}
			return s, nil
		}
		if errors.Is(err, errPunchTimeout) {
			punchTOs++
		} else if !errors.Is(err, errBadPort) && !errors.Is(err, errLost) && !errors.Is(err, errRelay) {
			return nil, err // 等不到对方之类的错误，重来也没用
		}
		log.Printf("第 %d 轮失败（会合 %s：%v），换端口重来", i+1, servers[k], err)
	}
	if punchTOs > 0 || errors.Is(err, errRelay) {
		o.pre.settle(outcome{actual: "失败", err: err.Error(), took: time.Since(start), rounds: attempts})
	}
	// 按层说清楚坏在哪：昨天能用今天不能用，多半是网络环境变了，先知道是哪一层
	switch {
	case punchTOs == attempts:
		return nil, fmt.Errorf("能会合、拿到了对方地址，但 %d 轮都没打通：两边之间的 UDP 直连被拦了（运营商换了 NAT、拦了端口，或者对称型 NAT）。"+
			"用 p2pcp check 看看是不是会合端口出了问题：%w", attempts, err)
	case punchTOs == 0 && !errors.Is(err, errRelay):
		return nil, fmt.Errorf("%w %s：本机断网、服务器挂了，或者这些端口都被拦了（p2pcp check 逐个测）：%w", errNoRendezvous, o.server, err)
	}
	return nil, err
}

var goodServer atomic.Int32 // 上次打通用的是第几个会合地址

// isControlling：本端是不是发起方（QUIC Dial、发提名、中继键用它的 id）。send 是、recv 不是；
// check 两端对等，id 大的一端发起，两边算出来的结果一致
func isControlling(id, peerID string) bool {
	switch {
	case strings.HasPrefix(id, "send."):
		return true
	case strings.HasPrefix(id, "recv."):
		return false
	}
	return id > peerID
}

var (
	errPunchTimeout = errors.New("打洞超时")
	// errBadPort：会合服务器的回执没收到，或者收到了但映射端口 < 1024。电信 CGNAT 端口不够时会分 1~437 的低端口，
	// 这些端口上外面的包进不来（连会合服务器的回包都收不到），整个 socket 只能扔掉
	errBadPort = errors.New("这个端口外面进不来")
	// errLost：等对方的时候，会合服务器的回执断了。多半是网络变了（换 Wi-Fi、5G 切换、地址变了），这个 socket 的映射已经作废
	errLost = errors.New("会合服务器失联")
	// errNoRendezvous：几轮下来一次都没拿到对方地址，问题在本机到会合服务器这一段，还没轮到打洞
	errNoRendezvous = errors.New("连不上会合服务器")
	errRelay        = errors.New("中继没有回应")
	errStopped      = errors.New("不等了")
	errNoPeer       = errors.New("对方没上线")
)

// connectOnce 用一个新 socket 注册、拿对方候选、打洞（或者走中继）。对方的候选 = 服务器看到的公网映射 + 对方自报的
// 内网地址，哪个先回应就用哪个（同一个局域网时内网地址 RTT 小，自然先回来）。
func connectOnce(o opts, server, rid, id string, wantRelay bool) (*session, error) {
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
			log.Printf("开 IPv6 socket 失败（%v），只走 IPv4", err)
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
		cands = strings.TrimSuffix(fmt.Sprintf("%s:%d,%s", o.public, port, cands), ",-")
	}
	if c6 != nil {
		cands = strings.TrimSuffix(c6.LocalAddr().String()+","+cands, ",-")
	}
	// 每次注册现拼：本机画像在后台测，出来之后的注册才带得上 nat=
	reg := func() []byte {
		var fl []string
		if wantRelay {
			fl = append(fl, "relay")
			if o.relay == "always" {
				fl = append(fl, "forced") // 强制走中继（测试用）：对方据此不拿预测去比（旧版不认识，忽略）
			}
		}
		if f := o.pre.flag(); f != "" {
			fl = append(fl, f)
		}
		flags := "-"
		if len(fl) > 0 {
			flags = strings.Join(fl, ",")
		}
		return []byte(fmt.Sprintf("/R2 %s %s %s %s", rid, id, cands, flags))
	}
	unreg := func() {
		for range 2 {
			c.WriteToUDP([]byte("/U2 "+rid+" "+id), srv)
		}
	}

	// parseCands：/P 里对方的公网映射 + 自报候选，按本端开了哪些 socket 过滤
	parseCands := func(pub, cands string) []*net.UDPAddr {
		var out []*net.UDPAddr
		for _, s := range append([]string{pub}, strings.Split(cands, ",")...) {
			a, err := net.ResolveUDPAddr("udp", s)
			if err != nil || a.IP == nil {
				continue
			}
			if v4 := a.IP.To4() != nil; v4 && o.only6 || !v4 && c6 == nil {
				continue
			}
			out = append(out, a)
		}
		return out
	}

	// 1. 注册，等 PEER。第一次注册前给画像 50ms：命中缓存时这就够了，第一次注册就带得上 nat=。
	// check -room 是诊断，不在乎这一会儿，等画像测完（preWait），保证两端互相拿得到
	o.pre.wait(cmp.Or(o.preWait, 50*time.Millisecond))
	var (
		peerCands []*net.UDPAddr
		peerID    string
		useRelay  = wantRelay
	)
	buf := make([]byte, 1500)
	start := time.Now()
	var (
		lastReg   time.Time
		regHadNat bool // 上一次注册带没带 nat=
		lastYou   time.Time
		mapped    *net.UDPAddr // 会合服务器回执里的本方公网映射
	)
	for peerID == "" {
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
		select {
		case <-o.quit: // 接收端挂在升级房间里等，会话结束时叫停（upgrade.go）
			unreg()
			closeAll()
			return nil, errStopped
		default:
		}
		if o.timeout > 0 && time.Since(start) > o.timeout {
			unreg()
			closeAll()
			if o.friend != "" { // 通讯录模式：说人话，朋友看得懂
				return nil, fmt.Errorf("%w：等了 %v，「%s」没有上线。对方的 P2P 直传没开着，或者手机熄屏后被系统冻结了"+
					"（在对方手机的设置里给 P2P 直传打开「允许后台高耗电」/ 关掉电池优化）", errNoPeer, o.timeout, o.friend)
			}
			return nil, fmt.Errorf("%w：等了 %v，对方没有上线（两端的 p2pcp 版本要一致：v2 起会合协议变了）", errNoPeer, o.timeout)
		}
		// 画像一测好就补一次注册，不等下一个周期（daemon 是 5 秒）：服务器存的是最近一次注册，对方这时来就能拿到 nat=
		if f := o.pre.flag(); time.Since(lastReg) >= o.regEvery || f != "" && !regHadNat {
			c.WriteToUDP(reg(), srv)
			lastReg, regHadNat = time.Now(), f != ""
		}
		wake := 500 * time.Millisecond
		if o.pre != nil && !regHadNat {
			wake = 50 * time.Millisecond // 画像还没带上：勤看着点，测好了马上补注册
		}
		c.SetReadDeadline(time.Now().Add(wake))
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if !from.IP.Equal(srv.IP) || from.Port != srv.Port {
				continue
			}
			f := strings.Fields(string(buf[:n]))
			if len(f) == 2 && f[0] == "/Y" {
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
			if len(f) != 5 || f[0] != "/P" {
				continue
			}
			peerID = f[1]
			useRelay = useRelay || slices.Contains(strings.Split(f[4], ","), "relay")
			o.pre.sawPeer(f[4])
			peerCands = parseCands(f[2], f[3])
			break
		}
	}
	// 打洞期间继续每 500ms 注册一次，打完（通了、超时、改走中继）再注销：服务器每收到一次注册就把双方地址再推一遍，
	// 推给对方的那个 PEER 万一丢了，半秒后还有下一个；而不是像以前那样拿到地址就注销，对方漏收一个包就白等一轮。
	// 注销还是要的：否则服务器在 30 秒内还会把这次的旧地址推给下一次注册的人
	controlling := isControlling(id, peerID)

	// goRelay 去中继报到。凑齐之前不注销、继续注册：先进新一轮的一方拿到的可能是对方上一轮的旧注册，
	// 它一注销，对方新一轮注册时房间里就找不到它、看不到 relay 标志，只能干等它在中继上 6 秒超时再来一轮
	// （netns port ↔ sym 偶发 10~14 秒，2026-11-21 第 5 步抓到）
	goRelay := func(why string) (*session, error) {
		defer unreg()
		if c6 != nil {
			c6.Close()
		}
		// 中继键 = 房间号 + 发送端 id：同一个房间里可以同时有好几对在走中继
		dialer := id
		if !controlling {
			dialer = peerID
		}
		relay := &net.UDPAddr{IP: srv.IP, Port: relayPort}
		t0 := time.Now()
		keep := func() { c.WriteToUDP(reg(), srv) }
		if err := joinRelay(c, relay, rid+"."+dialer, map[bool]string{true: "d", false: "l"}[controlling], keep); err != nil {
			c.Close()
			return nil, err
		}
		log.Printf("%s，改走中继 %s（%v）", why, relay, time.Since(t0).Round(time.Millisecond))
		return &session{conn: c, peer: relay, server: srv, relayed: true, took: time.Since(t0), control: controlling}, nil
	}
	if useRelay {
		return goRelay("打洞不通")
	}
	if len(peerCands) == 0 {
		unreg()
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
	waitNom := !controlling && c6 != nil
	var (
		mu       sync.Mutex
		chosen   *net.UDPAddr
		chosenOn *net.UDPConn
		learned  []*net.UDPAddr // 从对方入包学到的地址（peer-reflexive）、对方新一轮的地址，也往这里打
		done     = make(chan struct{})
		// 对方在重推的 /P 里带了 relay：它已经改走中继了，别再等这一轮超时（以前各按各的超时推进，
		// 一端已经在中继上等、另一端还在打洞，白等 4~6 秒，DESIGN 待定「轮次错位」）
		relayNow  = make(chan struct{})
		relayOnce sync.Once
		stop      atomic.Bool // 改走中继前让 listen 都退出，免得和 joinRelay 抢包
		lwg       sync.WaitGroup
	)
	addLearned := func(a *net.UDPAddr) { // 调用方持有 mu
		if !slices.ContainsFunc(peerCands, func(b *net.UDPAddr) bool { return sameAddr(a, b) }) &&
			!slices.ContainsFunc(learned, func(b *net.UDPAddr) bool { return sameAddr(a, b) }) {
			learned = append(learned, a)
		}
	}
	choose := func(c *net.UDPConn, from *net.UDPAddr) {
		mu.Lock()
		if chosen == nil {
			chosen, chosenOn = from, c
			close(done)
		}
		mu.Unlock()
	}
	listen := func(c *net.UDPConn) {
		defer lwg.Done()
		buf := make([]byte, 1500)
		triggered := map[string]bool{}
		for !stop.Load() {
			c.SetReadDeadline(deadline)
			n, from, err := c.ReadFromUDP(buf)
			if err != nil || stop.Load() {
				return
			}
			if os.Getenv("P2PCP_DEBUG") != "" {
				log.Printf("收到 %q 来自 %s", buf[:min(n, 16)], from)
			}
			switch string(buf[:min(n, 3)]) {
			case "/P ": // 服务器重推的对方信息（双方打洞期间每 500ms 注册一次，每次都推）
				if from.IP.Equal(srv.IP) && from.Port == srv.Port {
					if f := strings.Fields(string(buf[:n])); len(f) == 5 && f[1] == peerID {
						o.pre.sawPeer(f[4]) // 对方的画像可能这时才带上
						if slices.Contains(strings.Split(f[4], ","), "relay") {
							relayOnce.Do(func() { close(relayNow) })
						}
						// 对方已经换了新 socket（它那边这一轮先超时了）：新地址也打上，别对着它关掉的旧端口打满 4 秒
						mu.Lock()
						for _, a := range parseCands(f[2], f[3]) {
							addLearned(a)
						}
						mu.Unlock()
					}
				}
			case msgPunch:
				c.WriteToUDP(pktAck, from)
				if !triggered[from.String()] {
					triggered[from.String()] = true
					c.WriteToUDP(pktPunch, from)
					// 入包的源地址不在候选里 = 对方是对称型、给我们换了新端口（peer-reflexive）。记下来，之后每 20ms
					// 都往这里打：只补发一次的话，对方收到我们的 /PA 就停止收包，紧跟着到的这一个 /PP 没人应，
					// 我们就永远等不到 /PA（cone ↔ sym 偶发打不通，2026-11-21 netns 抓到）
					mu.Lock()
					addLearned(from)
					mu.Unlock()
				}
			case msgAck:
				if !waitNom {
					choose(c, from)
					return
				}
			// 发起方选定路径后先发 3 个 /PN 再 QUIC Dial。非发起方见到 /PN（或者 /PN 全丢了、先见到 QUIC Initial）
			// 就说明这条路两个方向都通，马上停：要是还等自己那份 /PA，这个 listen 会把紧跟着到的 Initial 当成无关的包
			// 读走扔掉，发起方要等 quic-go 的 PTO 重传（约 200ms，连丢几次就是 docs/13 那次「打洞 118ms、握手 4.4s」）。
			// listen 退出后来的包留在内核缓冲区里，QUIC 接管后照常读到。netns check -room：full ↔ sym 握手 202ms → 1ms
			case msgNom:
				if !controlling {
					choose(c, from)
					return
				}
			default:
				if !controlling && n >= 1200 && buf[0]&0x80 != 0 { // QUIC Initial：长包头，至少补到 1200 字节
					choose(c, from)
					return
				}
			}
		}
	}
	if !o.only6 { // -6 时 IPv4 socket 只管会合，别应答打洞（否则对方的触发检查会把路径拉回 IPv4）
		lwg.Add(1)
		go listen(c)
	}
	if c6 != nil {
		lwg.Add(1)
		go listen(c6)
	}
	// stopListeners 让 listen 都退出：它们每轮会把读超时设回 deadline，所以要反复踢，直到都退出
	stopListeners := func() {
		stop.Store(true)
		exited := make(chan struct{})
		go func() { lwg.Wait(); close(exited) }()
		for {
			c.SetReadDeadline(time.Now())
			if c6 != nil {
				c6.SetReadDeadline(time.Now())
			}
			select {
			case <-exited:
				c.SetReadDeadline(time.Time{})
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	lastReg = time.Now()
	for {
		if time.Since(lastReg) >= 500*time.Millisecond {
			c.WriteToUDP(reg(), srv)
			lastReg = time.Now()
		}
		mu.Lock()
		targets := append(slices.Clip(peerCands), learned...)
		mu.Unlock()
		for _, a := range targets {
			via := c
			if a.IP.To4() == nil {
				via = c6
			}
			if _, err := via.WriteToUDP(pktPunch, a); err != nil && os.Getenv("P2PCP_DEBUG") != "" {
				log.Printf("发往 %s 失败: %v", a, err)
			}
		}
		select {
		case <-done:
			unreg()
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
			return &session{conn: chosenOn, peer: chosen, server: srv, took: time.Since(punchStart), control: controlling}, nil
		case <-relayNow:
			stopListeners()
			return goRelay(fmt.Sprintf("对方已改走中继（打洞 %v 时）", time.Since(punchStart).Round(time.Millisecond)))
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			unreg()
			closeAll()
			return nil, fmt.Errorf("%w，对方候选 %v", errPunchTimeout, peerCands)
		}
	}
}

// joinRelay 向中继报到，等两端凑齐（/LK）。对方可能晚到几百毫秒，最多等 6 秒。
// keep 非空时每 500ms 调一次（connectOnce 用它继续在会合服务器上注册，见 goRelay）
func joinRelay(c *net.UDPConn, relay *net.UDPAddr, key, end string, keep func()) error {
	msg := []byte("/L " + key + " " + end)
	buf := make([]byte, 1500)
	deadline := time.Now().Add(6 * time.Second)
	var lastKeep time.Time
	for time.Now().Before(deadline) {
		if keep != nil && time.Since(lastKeep) >= 500*time.Millisecond {
			keep()
			lastKeep = time.Now()
		}
		c.WriteToUDP(msg, relay)
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if from.IP.Equal(relay.IP) && from.Port == relay.Port && string(buf[:n]) == "/LK" {
				c.SetReadDeadline(time.Time{})
				return nil
			}
		}
	}
	return fmt.Errorf("%w（%s，6 秒没凑齐两端）", errRelay, relay)
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
	best, _ := defaultRoute()
	if best == "" {
		return nil
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
