// punch: 最小化的 UDP 打洞实验 —— 一个会合(rendezvous)服务器 + 两个对等端。
// 这就是 ICE/UU远程/RustDesk/Tailscale 打洞的核心骨架，去掉了加密、认证与 TURN 中继。
//
//	服务器 (需要公网 IP，例如一台 VPS):
//	  go run . server -listen :7000
//	两个对等端 (分别在两个不同 NAT 后面):
//	  go run . peer -server VPS:7000 -room demo -id A
//	  go run . peer -server VPS:7000 -room demo -id B
//	对称 NAT 可加 -spray 50 尝试向预测端口附近喷洒。
//
// 协议 (纯文本 UDP):
//
//	peer→server  REG <room> <id> <host1,host2,...>
//	server→peer  PEER <id> <pubaddr> <host1,host2,...>
//	peer↔peer    PUNCH <id> / ACK <id> / PING <unixnano> / PONG <unixnano> / MSG <text> / KA
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
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

type member struct {
	pub, lan string
	seen     time.Time
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", ":7000", "UDP 监听地址")
	fs.Parse(args)

	conn, err := net.ListenUDP("udp", mustResolve(*listen))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("rendezvous listening on %s", conn.LocalAddr())
	rooms := map[string]map[string]*member{}
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		f := strings.Fields(string(buf[:n]))
		if len(f) != 4 || f[0] != "REG" {
			continue
		}
		room, id, lan := f[1], f[2], f[3]
		if rooms[room] == nil {
			rooms[room] = map[string]*member{}
		}
		m := rooms[room]
		if old, ok := m[id]; !ok || old.pub != from.String() {
			// 服务器看到的源地址 = 对方 NAT 分配的公网映射，这正是 STUN 做的事
			log.Printf("room=%s id=%s pub=%s lan=%s", room, id, from, lan)
		}
		m[id] = &member{pub: from.String(), lan: lan, seen: time.Now()}
		for oid, o := range m {
			if oid == id || time.Since(o.seen) > 30*time.Second {
				continue
			}
			// 双向交换：两边几乎同时拿到对方地址，同时开始往外发包 —— "同时打开"
			conn.WriteToUDP([]byte(fmt.Sprintf("PEER %s %s %s", oid, o.pub, o.lan)), from)
			conn.WriteToUDP([]byte(fmt.Sprintf("PEER %s %s %s", id, from, lan)), mustResolve(o.pub))
		}
	}
}

// ---------------- peer ----------------

func runPeer(args []string) {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	server := fs.String("server", "127.0.0.1:7000", "会合服务器地址")
	room := fs.String("room", "demo", "房间名")
	id := fs.String("id", "A", "本端 ID")
	port := fs.Int("port", 0, "本地 UDP 端口 (0=随机)")
	spray := fs.Int("spray", 0, "对称 NAT: 向对端公网端口 ±N 范围喷洒")
	timeout := fs.Duration("timeout", 15*time.Second, "打洞超时")
	fs.Parse(args)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	srv := mustResolve(*server)
	lan := hostCandidates(conn.LocalAddr().(*net.UDPAddr).Port)
	log.Printf("local %s, registering to %s", lan, srv)

	var (
		mu         sync.Mutex
		candidates []*net.UDPAddr
		selected   *net.UDPAddr
		peerID     string
		connected  = make(chan struct{})
		once       sync.Once
		pongs      = make(chan time.Duration, 64)
	)
	choose := func(a *net.UDPAddr, why string) {
		once.Do(func() {
			mu.Lock()
			selected = a
			mu.Unlock()
			kind := "公网 (打洞成功)"
			if a.IP.IsPrivate() || a.IP.IsLoopback() {
				kind = "局域网直连"
			}
			log.Printf("✔ connected to %s via %s [%s, %s]", peerID, a, kind, why)
			close(connected)
		})
	}

	// 接收循环：一个 socket 同时处理服务器与对端的报文 —— 打洞必须复用同一个端口
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
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
				if len(f) != 4 {
					continue
				}
				mu.Lock()
				if peerID == "" {
					peerID = f[1]
					pub := mustResolve(f[2])
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
				}
				mu.Unlock()
			case "PUNCH":
				conn.WriteToUDP([]byte("ACK "+*id), from)
				choose(from, "收到对方 PUNCH")
			case "ACK":
				choose(from, "收到 ACK")
			case "PING":
				conn.WriteToUDP([]byte("PONG "+f[1]), from)
			case "PONG":
				if t, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					pongs <- time.Since(time.Unix(0, t))
				}
			case "MSG":
				fmt.Printf("\r<%s> %s\n> ", peerID, strings.TrimPrefix(msg, "MSG "))
			}
		}
	}()

	// 注册 + 打洞循环
	deadline := time.Now().Add(*timeout)
	tick := time.NewTicker(200 * time.Millisecond)
loop:
	for {
		select {
		case <-connected:
			break loop
		case <-tick.C:
			if time.Now().After(deadline) {
				log.Fatal("✘ 打洞超时：双方可能都是对称 NAT 或 UDP 被封，现实中这里会降级到 TURN 中继")
			}
			conn.WriteToUDP([]byte(fmt.Sprintf("REG %s %s %s", *room, *id, lan)), srv)
			mu.Lock()
			for _, c := range candidates {
				conn.WriteToUDP([]byte("PUNCH "+*id), c)
			}
			mu.Unlock()
		}
	}
	tick.Stop()

	mu.Lock()
	peer := selected
	mu.Unlock()

	// 测 RTT
	var rtts []time.Duration
	for i := 0; i < 20; i++ {
		conn.WriteToUDP([]byte(fmt.Sprintf("PING %d", time.Now().UnixNano())), peer)
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
			conn.WriteToUDP([]byte("KA"), peer)
		}
	}()
	fmt.Print("聊天模式，输入文字回车发送 (Ctrl-C 退出)\n> ")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		conn.WriteToUDP([]byte("MSG "+sc.Text()), peer)
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
