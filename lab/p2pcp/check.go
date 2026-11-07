package main

// p2pcp check：网络体检。"昨天能用、今天不能用"时先跑它，看是哪一层变了。
//
//	p2pcp check [-server a,b] [-bind auto]
//
// 对每个会合地址 P：开一个新 socket，先问 P，再用同一个 socket 问其余地址。
//   - P 自己没回执：这个端口到不了（服务器挂了、端口被拦、安全组没放行）
//   - P 有回执，但同一个 socket 再问别的地址时，要么没回执、要么公网映射变了：和 P 通过信之后，这个 socket 往别处发包
//     走的不是服务器告诉对方的那个映射，经 P 会合打洞必然失败（docs/09 第 7 节的 7000 事件：先问 7000 的 socket，
//     再问 7001 时映射从 24728 变成 24828）
//   - 每个地址都这样：映射随目的地变化（对称型 NAT），打洞很难
//   - 映射端口 < 1024：电信 CGNAT 的低端口段，外面进不来（docs/09 第 6 节）
//
// 最后测中继：两个 socket 分别以两端的身份报到，凑齐后从一端发个包，看另一端能不能经中继收到。

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"
)

func check(o opts) error {
	servers := strings.Split(o.server, ",")
	var bindIP net.IP
	switch o.bind {
	case "auto":
		bindIP = defaultRouteIP()
		fmt.Printf("本机   绑定 %v（auto：main 表默认路由的网卡）\n", bindIP)
	case "":
		fmt.Println("本机   不绑定（开着 TUN 代理时测到的可能是代理出口，加 -bind auto）")
	default:
		bindIP = net.ParseIP(o.bind)
		fmt.Printf("本机   绑定 %v\n", bindIP)
	}
	if ip6 := global6(o); ip6 != nil {
		fmt.Printf("IPv6   有全局地址 %v，会和 IPv4 同时打洞\n", ip6)
	} else {
		fmt.Println("IPv6   没有全局地址，只走 IPv4")
	}
	fmt.Println()

	var (
		bad      []string // 自己就到不了的
		poisoned []string // 自己通、但通过信之后这个 socket 往别处发包就不对了的
		lowPort  bool
	)
	// 第一遍：每个地址用自己的新 socket 问，先弄清哪些本来就通
	type row struct {
		c      *net.UDPConn
		mapped *net.UDPAddr
		line   string
	}
	rows := make([]*row, len(servers))
	for i, p := range servers {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP})
		if err != nil {
			return err
		}
		defer c.Close()
		mapped, rtt, err := probeServer(c, p)
		if err != nil {
			fmt.Printf("✘ %-22s 没有回执（%v）\n", p, err)
			bad = append(bad, p)
			continue
		}
		lowPort = lowPort || mapped.Port < 1024
		rows[i] = &row{c, mapped, fmt.Sprintf("✔ %-22s 回执 %v，公网映射 %v", p, rtt.Round(time.Millisecond), mapped)}
	}
	// 第二遍：同一个 socket 再问其余通的地址。别的都通、偏偏跟 P 通过信之后问不通 = P 有毒
	for i, r := range rows {
		if r == nil {
			continue
		}
		var after []string
		others, broken := 0, 0
		for j, q := range servers {
			if j == i || rows[j] == nil {
				continue
			}
			others++
			m2, _, err := probeServer(r.c, q)
			switch {
			case err != nil:
				broken++
				after = append(after, q+" ✘")
			case !m2.IP.Equal(r.mapped.IP) || m2.Port != r.mapped.Port:
				broken++
				after = append(after, fmt.Sprintf("%s ✔ 但映射变成 %v", q, m2))
			default:
				after = append(after, q+" ✔")
			}
		}
		if len(after) > 0 {
			r.line += "；同一个 socket 再问 " + strings.Join(after, "，")
		}
		fmt.Println(r.line)
		if others > 0 && broken == others {
			poisoned = append(poisoned, servers[i])
		}
	}

	fmt.Println()
	ok := true
	reachable := len(servers) - len(bad)
	symmetric := reachable > 1 && len(poisoned) == reachable
	if !symmetric {
		for _, p := range poisoned {
			ok = false
			fmt.Printf("⚠ %s：和它通过信之后，这个 socket 往别处发包用的不是服务器看到的映射，经它会合必然打不通。"+
				"p2pcp 会自动换到别的端口，但最好从 -server 里去掉它\n", p)
		}
	}
	if len(bad) == len(servers) {
		ok = false
		fmt.Println("⚠ 所有会合端口都没有回执：本机断网、服务器挂了，或者 UDP 被整体拦了")
	} else if len(bad) > 0 {
		fmt.Printf("· %d 个会合端口到不了，p2pcp 会自动跳过（多花一轮，约 2 秒）\n", len(bad))
	}
	if symmetric {
		ok = false
		fmt.Println("⚠ 不管先问哪个地址，同一个 socket 再问别处映射都会变：映射随目的地变化（对称型 NAT），对方也是这样时基本打不通")
	}
	if lowPort {
		fmt.Println("· 被映射到了 1024 以下的低端口：这种端口外面进不来，p2pcp 会自动换 socket")
	}
	if reachable > 0 {
		k := 0
		for k < len(servers) && rows[k] == nil {
			k++
		}
		srv, _ := net.ResolveUDPAddr("udp4", servers[k])
		relay := &net.UDPAddr{IP: srv.IP, Port: relayPort}
		if rtt, err := probeRelay(bindIP, relay); err != nil {
			fmt.Printf("⚠ 中继 %s 不通（%v）：打不通直连时就没有兜底了\n", relay, err)
		} else {
			fmt.Printf("✔ 中继 %s 可用，经中继往返 %v（服务器出方向只有 3M，只够 ssh 这类小流量）\n", relay, rtt.Round(time.Millisecond))
		}
	}
	if ok {
		fmt.Println("结论：本机这一侧正常。还打不通的话，在对方那台设备上也跑一次 p2pcp check")
	}
	return nil
}

// probeRelay 开两个 socket 分别当中继的两端，凑齐后从 d 端发一个包、等 l 端收到
func probeRelay(bindIP net.IP, relay *net.UDPAddr) (time.Duration, error) {
	d, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP})
	if err != nil {
		return 0, err
	}
	defer d.Close()
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP})
	if err != nil {
		return 0, err
	}
	defer l.Close()
	key := "check-" + randHex(4)
	errc := make(chan error, 1)
	go func() { errc <- joinRelay(l, relay, key, "l") }()
	if err := joinRelay(d, relay, key, "d"); err != nil {
		return 0, err
	}
	if err := <-errc; err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for range 3 {
		sent := time.Now()
		d.WriteToUDP([]byte("/check"), relay)
		l.SetReadDeadline(sent.Add(time.Second))
		for {
			n, _, err := l.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if string(buf[:n]) == "/check" {
				return time.Since(sent), nil
			}
		}
	}
	return 0, fmt.Errorf("凑齐了，但包没转过来")
}

// probeServer 用 c 向会合服务器发一个体检用的 REG，等它的 YOU 回执，返回本方公网映射和往返时间。
func probeServer(c *net.UDPConn, server string) (*net.UDPAddr, time.Duration, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, 0, err
	}
	room := fmt.Sprintf("check-%08x", rand.Uint32())
	defer c.WriteToUDP([]byte("/U2 "+room+" check.0"), srv)
	buf := make([]byte, 1500)
	for range 3 { // 丢一两个包不算到不了
		sent := time.Now()
		c.WriteToUDP([]byte("/R2 "+room+" check.0 - -"), srv)
		c.SetReadDeadline(sent.Add(700 * time.Millisecond))
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			f := strings.Fields(string(buf[:n]))
			if from.IP.Equal(srv.IP) && from.Port == srv.Port && len(f) == 2 && f[0] == "/Y" {
				a, err := net.ResolveUDPAddr("udp4", f[1])
				return a, time.Since(sent), err
			}
		}
	}
	return nil, 0, fmt.Errorf("2 秒内 3 次都没回")
}
