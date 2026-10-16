// natprobe: 在真实网络上逐项测 NAT 行为，为打洞策略提供参数。比 stunprobe 多了需要自家服务器配合的几项。
//
//	natprobe server -ports 3478,3479                      自家服务器：多端口 STUN + 下面几项测试的配合逻辑
//	natprobe stun -servers ip:port,... [-alloc 12]        映射行为（EIM/EDM）+ 端口分配规律（保持/递增/随机）
//	natprobe filter -server ip:3478 -alt 3479             过滤行为：从没发过包的端口回包能不能进来
//	natprobe ttl -server ip:3478 -alt 3479 -max 10        低 TTL 预热该用几跳：TTL 至少多大才能在本方 NAT 上开洞
//	natprobe life -server ip:3478 [-delays 10s,30s,...]   映射寿命：静默多久后外面的包就进不来了（决定保活间隔）
//	natprobe peer -stun ip:port [-to ip:port | -file f]   手工打洞：问一次 STUN 打印映射，然后向目标持续发包并打印收到的包
//	                                                      （不需要会合服务器：两端的映射地址由人/脚本转交，-file 每 100ms 读一次目标）
//
// 所有客户端命令都支持 -bind <本机 IP>：开着 sing-box 之类的 TUN 时，绑定物理网卡的地址才能测到真实 NAT。
// 只用 IP，不做 DNS 解析（Android 的 adb shell 里没有 /etc/resolv.conf）。
//
// 服务端除 STUN 外用纯文本 UDP 消息：
//
//	FILT <id>        服务端从主端口和备用端口各回一个 "FILT <id> same|alt"
//	LIFE <id> <ms>   立即回 "LIFE <id> ack"，ms 毫秒后再从同一端口发 "LIFE <id> alive"
//	TTLA <id>        记下来源地址并回 ack；TTLB <id> <k> 发到备用端口，到了就记一笔
//	TTLC <id>        回 "TTLC <id> ack <到达服务器的 k|->"，并在 100ms 后从备用端口向 TTLA 记下的地址发 "TTLP <id>"
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	log.SetFlags(log.Ltime)
	args := os.Args[2:]
	switch os.Args[1] {
	case "server":
		runServer(args)
	case "stun":
		runStun(args)
	case "filter":
		runFilter(args)
	case "ttl":
		runTTL(args)
	case "life":
		runLife(args)
	case "peer":
		runPeer(args)
	default:
		usage()
	}
}

func usage() {
	fmt.Println("usage: natprobe server|stun|filter|ttl|life|peer [flags]")
	os.Exit(2)
}

// ---------------- server ----------------

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	ports := fs.String("ports", "3478,3479", "监听的 UDP 端口，第一个是主端口，第二个是备用端口")
	fs.Parse(args)

	var conns []*net.UDPConn
	for _, p := range strings.Split(*ports, ",") {
		port, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			log.Fatal(err)
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			log.Fatal(err)
		}
		conns = append(conns, c)
		log.Printf("listening on %s", c.LocalAddr())
	}
	if len(conns) < 2 {
		log.Fatal("至少需要两个端口")
	}
	alt := conns[1]

	var mu sync.Mutex
	ttlAddr := map[string]*net.UDPAddr{} // TTLA 记下的地址
	ttlReach := map[string]string{}      // TTLB 到达服务器时带的 k

	serve := func(c *net.UDPConn) {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			msg := buf[:n]
			if isBindingRequest(msg) {
				if r := bindingResponse(msg, from); r != nil {
					c.WriteToUDP(r, from)
				}
				continue
			}
			f := strings.Fields(string(msg))
			if len(f) < 2 {
				continue
			}
			id := f[1]
			switch f[0] {
			case "FILT":
				log.Printf("FILT %s from %s", id, from)
				c.WriteToUDP([]byte("FILT "+id+" same"), from)
				alt.WriteToUDP([]byte("FILT "+id+" alt"), from)
			case "LIFE":
				if len(f) < 3 {
					continue
				}
				ms, _ := strconv.Atoi(f[2])
				log.Printf("LIFE %s from %s, alive in %dms", id, from, ms)
				c.WriteToUDP([]byte("LIFE "+id+" ack"), from)
				time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
					c.WriteToUDP([]byte("LIFE "+id+" alive"), from)
				})
			case "TTLA":
				mu.Lock()
				ttlAddr[id] = from
				mu.Unlock()
				c.WriteToUDP([]byte("TTLA "+id+" ack"), from)
			case "TTLB":
				if len(f) >= 3 {
					mu.Lock()
					ttlReach[id] = f[2]
					mu.Unlock()
				}
			case "TTLC":
				mu.Lock()
				a, reach := ttlAddr[id], ttlReach[id]
				delete(ttlAddr, id)
				delete(ttlReach, id)
				mu.Unlock()
				if reach == "" {
					reach = "-"
				}
				c.WriteToUDP([]byte("TTLC "+id+" ack "+reach), from)
				if a != nil {
					log.Printf("TTLC %s: A=%s reach=%s", id, a, reach)
					time.AfterFunc(100*time.Millisecond, func() {
						alt.WriteToUDP([]byte("TTLP "+id), a)
					})
				}
			}
		}
	}
	for _, c := range conns[1:] {
		go serve(c)
	}
	serve(conns[0])
}

// ---------------- 客户端公共部分 ----------------

func listen(bind string) *net.UDPConn {
	var ip net.IP
	if bind != "" {
		if ip = net.ParseIP(bind); ip == nil {
			log.Fatalf("-bind 需要 IP：%q", bind)
		}
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		log.Fatal(err)
	}
	return c
}

func mustAddr(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp4", s)
	if err != nil || a.IP == nil {
		log.Fatalf("地址 %q 无效（只接受 IP:端口）", s)
	}
	return a
}

// withPort 返回 a 的同 IP、另一个端口的地址。
func withPort(a *net.UDPAddr, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: a.IP, Port: port}
}

func newID() string { return strconv.FormatUint(rand.Uint64()%1e9, 36) }

// request 发 msg 并等第一个以 want 开头的回包，丢包重传 3 次。
func request(c *net.UDPConn, to *net.UDPAddr, msg, want string) (string, error) {
	buf := make([]byte, 1500)
	defer c.SetReadDeadline(time.Time{})
	for try := 0; try < 3; try++ {
		c.WriteToUDP([]byte(msg), to)
		c.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		for {
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if s := string(buf[:n]); strings.HasPrefix(s, want) {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("%s: 没有回应", strings.Fields(msg)[0])
}

// collect 在 d 时间内收集所有回包。
func collect(c *net.UDPConn, d time.Duration) []string {
	var got []string
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return got
		}
		got = append(got, string(buf[:n]))
	}
}

// ---------------- stun：映射行为 + 端口分配规律 ----------------

func runStun(args []string) {
	fs := flag.NewFlagSet("stun", flag.ExitOnError)
	servers := fs.String("servers", "", "逗号分隔的 STUN 服务器 IP:端口（至少两个，最好不同 IP）")
	bind := fs.String("bind", "", "绑定的本机 IP")
	alloc := fs.Int("alloc", 12, "端口分配测试：依次新开多少个 socket")
	fs.Parse(args)
	var srv []*net.UDPAddr
	for _, s := range strings.Split(*servers, ",") {
		if s = strings.TrimSpace(s); s != "" {
			srv = append(srv, mustAddr(s))
		}
	}
	if len(srv) == 0 {
		log.Fatal("需要 -servers")
	}

	// 1. 同一个 socket 问所有服务器
	c := listen(*bind)
	local := c.LocalAddr().(*net.UDPAddr).Port
	fmt.Printf("== 映射行为：同一个本地端口 %d 问 %d 个服务器\n", local, len(srv))
	var maps []*net.UDPAddr
	for _, s := range srv {
		m, rtt, err := query(c, s)
		if err != nil {
			fmt.Printf("  %-22s 失败: %v\n", s, err)
			continue
		}
		fmt.Printf("  %-22s → %-22s RTT %v\n", s, m, rtt.Round(time.Millisecond))
		maps = append(maps, m)
	}
	c.Close()
	switch {
	case len(maps) < 2:
		fmt.Println("  有效结果不足 2 个")
	case !allSame(maps, func(a *net.UDPAddr) string { return a.IP.String() }):
		fmt.Println("  ⚠ 公网 IP 不一致：多出口 NAT，或者部分流量走了代理")
	case allSame(maps, func(a *net.UDPAddr) string { return a.String() }):
		fmt.Print("  ✔ EIM（端点无关映射）")
		if maps[0].Port == local {
			fmt.Print("，且端口保持")
		}
		fmt.Println()
	default:
		fmt.Print("  ✘ EDM（端点相关映射，对称型）。依次的端口增量:")
		for i := 1; i < len(maps); i++ {
			fmt.Printf(" %+d", maps[i].Port-maps[i-1].Port)
		}
		fmt.Println()
	}

	// 2. 依次新开 socket，都问第一个服务器：看 NAT 怎么给新映射分配端口
	if *alloc <= 1 {
		return
	}
	fmt.Printf("\n== 端口分配：依次新开 %d 个 socket，都问 %s\n", *alloc, srv[0])
	var locals, mapped []int
	for i := 0; i < *alloc; i++ {
		c := listen(*bind)
		m, _, err := query(c, srv[0])
		lp := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		if err != nil {
			fmt.Printf("  本地 %5d → 失败\n", lp)
			continue
		}
		locals = append(locals, lp)
		mapped = append(mapped, m.Port)
	}
	preserved := 0
	var deltas []int
	for i := range mapped {
		if mapped[i] == locals[i] {
			preserved++
		}
		if i > 0 {
			deltas = append(deltas, mapped[i]-mapped[i-1])
		}
	}
	fmt.Printf("  本地端口: %v\n  映射端口: %v\n  映射增量: %v\n", locals, mapped, deltas)
	switch {
	case len(mapped) == 0:
	case preserved == len(mapped):
		fmt.Println("  ✔ 端口保持：映射端口 = 本地端口，对方能直接猜到")
	case len(deltas) > 0 && smallDeltas(deltas, 16) >= len(deltas)*3/4:
		fmt.Printf("  ◐ 基本递增（%d/%d 个增量在 ±16 以内）：可以做端口预测，被其他用户插队的部分靠多预测几个端口弥补\n",
			smallDeltas(deltas, 16), len(deltas))
	default:
		lo, hi := slices.Min(mapped), slices.Max(mapped)
		fmt.Printf("  ✘ 无明显规律（范围 %d~%d）：只能靠生日攻击\n", lo, hi)
	}
}

func allSame(as []*net.UDPAddr, key func(*net.UDPAddr) string) bool {
	for _, a := range as[1:] {
		if key(a) != key(as[0]) {
			return false
		}
	}
	return true
}

func smallDeltas(ds []int, lim int) int {
	n := 0
	for _, d := range ds {
		if d != 0 && d >= -lim && d <= lim {
			n++
		}
	}
	return n
}

// ---------------- filter：过滤行为 ----------------

func runFilter(args []string) {
	fs := flag.NewFlagSet("filter", flag.ExitOnError)
	server := fs.String("server", "", "natprobe 服务端主端口 IP:端口")
	alt := fs.Int("alt", 3479, "服务端备用端口")
	bind := fs.String("bind", "", "绑定的本机 IP")
	fs.Parse(args)
	srv := mustAddr(*server)
	_ = alt // 备用端口由服务端自己决定，这里只用来提示

	c := listen(*bind)
	defer c.Close()
	id := newID()
	c.WriteToUDP([]byte("FILT "+id), srv)
	got := collect(c, 1500*time.Millisecond)
	same := slices.Contains(got, "FILT "+id+" same")
	other := slices.Contains(got, "FILT "+id+" alt")
	fmt.Printf("== 过滤行为：只向 %s 发过包，服务端从同端口和 %d 端口各回一个\n", srv, *alt)
	fmt.Printf("  同端口回包: %v\n  另一端口回包: %v\n", yes(same), yes(other))
	switch {
	case !same:
		fmt.Println("  ✘ 连同端口的回包都没收到：服务端口没放行或网络不通，结论无效")
	case other:
		fmt.Println("  ✔ 端口无关过滤（至少对同一个 IP）：对端只要知道映射地址就能直接进来")
	default:
		fmt.Println("  ✘ 端口相关过滤（APDF，端口受限）：必须本方先向对方的确切端口发过包，才能收到对方的包 —— 打洞要双方同时发")
	}
}

func yes(b bool) string {
	if b {
		return "收到"
	}
	return "没收到"
}

// ---------------- ttl：本方 NAT（过滤设备）离本机几跳 ----------------

func runTTL(args []string) {
	fs := flag.NewFlagSet("ttl", flag.ExitOnError)
	server := fs.String("server", "", "natprobe 服务端主端口 IP:端口")
	alt := fs.Int("alt", 3479, "服务端备用端口")
	bind := fs.String("bind", "", "绑定的本机 IP")
	maxTTL := fs.Int("max", 10, "最大测到几跳")
	fs.Parse(args)
	srv := mustAddr(*server)
	altAddr := withPort(srv, *alt)

	fmt.Printf("== 低 TTL 开洞测试：每个 TTL 用一个新 socket\n")
	fmt.Println("   ① 正常 TTL 向主端口发 TTLA（服务端记下映射地址）")
	fmt.Println("   ② 用 TTL=k 向备用端口发 TTLB（只要能走出本方 NAT，就会在 NAT 上为备用端口开洞）")
	fmt.Println("   ③ 让服务端从备用端口向①的映射地址发包，收到 = TTL=k 已经开出了洞")
	fmt.Println("   k=0 是对照：不发 TTLB。k=0 也能收到说明过滤是端口无关的，这个测试就没有意义了")
	ks := []int{0}
	for k := 1; k <= *maxTTL; k++ {
		ks = append(ks, k)
	}
	ks = append(ks, 64)
	firstOpen, firstReach := 0, 0
	for _, k := range ks {
		c := listen(*bind)
		id := newID()
		if _, err := request(c, srv, "TTLA "+id, "TTLA "+id); err != nil {
			fmt.Printf("  k=%-2d %v\n", k, err)
			c.Close()
			continue
		}
		if k > 0 {
			if err := setTTL(c, k); err != nil {
				log.Fatal(err)
			}
			for i := 0; i < 3; i++ { // 低 TTL 包没有回应，没法重传，多发几个
				c.WriteToUDP([]byte(fmt.Sprintf("TTLB %s %d", id, k)), altAddr)
			}
			setTTL(c, 64)
			time.Sleep(50 * time.Millisecond)
		}
		ack, err := request(c, srv, "TTLC "+id, "TTLC "+id)
		if err != nil {
			fmt.Printf("  k=%-2d %v\n", k, err)
			c.Close()
			continue
		}
		opened := slices.Contains(collect(c, 600*time.Millisecond), "TTLP "+id)
		reach := strings.Fields(ack)[3] != "-"
		c.Close()
		fmt.Printf("  k=%-2d 开洞 %-4s  TTLB 到达服务器 %s\n", k, yes(opened), yes(reach))
		if k > 0 && opened && firstOpen == 0 {
			firstOpen = k
		}
		if k > 0 && reach && firstReach == 0 {
			firstReach = k
		}
	}
	fmt.Println()
	if firstOpen > 0 {
		fmt.Printf("  → TTL ≥ %d 就能在本方 NAT 上开洞", firstOpen)
		switch {
		case firstReach == 64:
			fmt.Printf("；到服务器超过 %d 跳", *maxTTL)
		case firstReach > 0:
			fmt.Printf("；到服务器要 %d 跳", firstReach)
		}
		fmt.Println("\n    预热用的 TTL 取开洞的最小值即可；它必须小于到对方 NAT 的跳数，否则就不是\"预热\"而是普通打洞了")
	}
}

// ---------------- life：映射寿命 ----------------

func runLife(args []string) {
	fs := flag.NewFlagSet("life", flag.ExitOnError)
	server := fs.String("server", "", "natprobe 服务端主端口 IP:端口")
	bind := fs.String("bind", "", "绑定的本机 IP")
	delays := fs.String("delays", "10s,20s,30s,45s,60s,90s,120s,180s,240s,300s", "逗号分隔的静默时长，每个用一个 socket 并行测")
	fs.Parse(args)
	srv := mustAddr(*server)

	var ds []time.Duration
	for _, s := range strings.Split(*delays, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			log.Fatal(err)
		}
		ds = append(ds, d)
	}
	fmt.Printf("== 映射寿命：%d 个 socket 各发一次后保持静默，服务端分别在 %s 后从外面发包进来\n", len(ds), *delays)
	fmt.Printf("   预计 %v 后出结果\n", slices.Max(ds)+2*time.Second)

	type res struct {
		d     time.Duration
		alive bool
		err   error
	}
	out := make([]res, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := listen(*bind)
			defer c.Close()
			id := newID()
			ms := strconv.Itoa(int(d / time.Millisecond))
			if _, err := request(c, srv, "LIFE "+id+" "+ms, "LIFE "+id+" ack"); err != nil {
				out[i] = res{d: d, err: err}
				return
			}
			got := collect(c, d+2*time.Second)
			out[i] = res{d: d, alive: slices.Contains(got, "LIFE "+id+" alive")}
		}()
	}
	wg.Wait()
	var lastAlive, firstDead time.Duration
	for _, r := range out {
		switch {
		case r.err != nil:
			fmt.Printf("  静默 %-5v  %v\n", r.d, r.err)
		default:
			fmt.Printf("  静默 %-5v  %s\n", r.d, map[bool]string{true: "✔ 还能进来", false: "✘ 映射已失效"}[r.alive])
			if r.alive && r.d > lastAlive {
				lastAlive = r.d
			}
			if !r.alive && (firstDead == 0 || r.d < firstDead) {
				firstDead = r.d
			}
		}
	}
	fmt.Println()
	switch {
	case firstDead == 0:
		fmt.Printf("  → 至少 %v 不会失效\n", lastAlive)
	default:
		fmt.Printf("  → 映射寿命在 %v ~ %v 之间；保活间隔取下限的一半左右比较稳\n", lastAlive, firstDead)
	}
}
