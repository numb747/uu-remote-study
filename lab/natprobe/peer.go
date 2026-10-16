package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runPeer 是不带会合服务器的手工打洞：会合服务器唯一的作用就是把双方的映射地址转交给对方，
// 这里由人或脚本代劳（-to 或 -file）。也用来测"陌生 IP 发来的包能不能进来"：一端只收不发，另一端往它的映射地址发。
//
//	HI <seq> <unixnano>     每 -interval 发一个
//	HIACK <seq> <unixnano> <回应方 unixnano>  收到 HI 立即回，发送方据此算 RTT 和两端时钟偏差
func runPeer(args []string) {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	stun := fs.String("stun", "", "STUN 服务器 IP:端口，用来打印本方映射地址")
	bind := fs.String("bind", "", "绑定的本机 IP")
	port := fs.Int("port", 0, "本地端口（0=随机）")
	to := fs.String("to", "", "对端映射地址 IP:端口；为空则只收不发")
	file := fs.String("file", "", "从这个文件读对端地址（每 100ms 读一次，可以中途写入或修改）")
	dur := fs.Duration("dur", 30*time.Second, "运行多久")
	interval := fs.Duration("interval", 200*time.Millisecond, "发包间隔")
	ttl := fs.Int("ttl", 0, "低 TTL 预热：前 -warm 个包用这个 TTL 发（0=不预热）")
	warm := fs.Int("warm", 3, "预热包个数")
	trigger := fs.Bool("trigger", false, "触发检查：收到对方第一个 HI 时立即回发一个 HI")
	fs.Parse(args)

	var ip net.IP
	if *bind != "" {
		ip = net.ParseIP(*bind)
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	ts := func() string {
		return fmt.Sprintf("%s +%5dms", time.Now().Format("15:04:05.000"), time.Since(start).Milliseconds())
	}

	fmt.Printf("%s 本地 %s\n", ts(), c.LocalAddr())
	if *stun != "" {
		if m, _, err := query(c, mustAddr(*stun)); err != nil {
			fmt.Printf("%s STUN 失败: %v\n", ts(), err)
		} else {
			fmt.Printf("%s MAPPED %s\n", ts(), m)
		}
	}

	var (
		mu       sync.Mutex
		rtts     []time.Duration
		nHI      int
		sources  = map[string]int{}
		firstHI  time.Duration
		firstAck time.Duration
		bestRTT  time.Duration
		offset   time.Duration // 对端时钟 - 本机时钟
	)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			f := strings.Fields(string(buf[:n]))
			if len(f) < 3 {
				continue
			}
			mu.Lock()
			sources[from.String()]++
			switch f[0] {
			case "HI":
				c.WriteToUDP(fmt.Appendf(nil, "HIACK %s %s %d", f[1], f[2], time.Now().UnixNano()), from)
				if nHI++; nHI == 1 {
					firstHI = time.Since(start)
					fmt.Printf("%s ← 首个 HI 来自 %s\n", ts(), from)
					// 触发检查（ICE 的 triggered check）：对方的包能进来，说明对方 NAT 上已经为我们开了洞，
					// 此刻立即回发一个探测，而不是等下一个发包周期 —— 之前发出的探测可能在对方开洞前就被丢了
					if *trigger && len(rtts) == 0 {
						c.WriteToUDP(fmt.Appendf(nil, "HI t %d", time.Now().UnixNano()), from)
					}
				}
			case "HIACK":
				ns, _ := strconv.ParseInt(f[2], 10, 64)
				rtt := time.Since(time.Unix(0, ns))
				rtts = append(rtts, rtt)
				if len(f) >= 4 && (bestRTT == 0 || rtt < bestRTT) {
					// NTP 的算法：假设去回程对称，对端时间应该对应我方 发送 + RTT/2；取 RTT 最小的样本误差最小
					peer, _ := strconv.ParseInt(f[3], 10, 64)
					bestRTT, offset = rtt, time.Duration(peer-ns)-rtt/2
				}
				if len(rtts) == 1 {
					firstAck = time.Since(start)
					fmt.Printf("%s ← 首个 HIACK 来自 %s，RTT %v —— 双向打通\n", ts(), from, rtts[0].Round(100*time.Microsecond))
				}
			}
			mu.Unlock()
		}
	}()

	var target *net.UDPAddr
	var firstSend time.Duration
	seq := 0
	tick := time.NewTicker(min(*interval, 100*time.Millisecond))
	defer tick.Stop()
	lastSend := time.Time{}
	for now := range tick.C {
		if now.Sub(start) > *dur {
			break
		}
		want := *to
		if *file != "" {
			if b, err := os.ReadFile(*file); err == nil {
				want = strings.TrimSpace(string(b))
			}
		}
		if want != "" && (target == nil || target.String() != want) {
			if a, err := net.ResolveUDPAddr("udp4", want); err == nil {
				target = a
				fmt.Printf("%s → 目标 %s\n", ts(), a)
			}
		}
		if target == nil || now.Sub(lastSend) < *interval*9/10 {
			continue
		}
		lastSend = now
		low := *ttl > 0 && seq < *warm
		if low {
			setTTL(c, *ttl)
		}
		c.WriteToUDP(fmt.Appendf(nil, "HI %d %d", seq, time.Now().UnixNano()), target)
		if low && seq == *warm-1 {
			setTTL(c, 64)
		}
		if seq == 0 {
			firstSend = time.Since(start)
		}
		seq++
	}

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("\n== 汇总：发 HI %d 个，收 HI %d 个，收 HIACK %d 个\n", seq, nHI, len(rtts))
	for s, n := range sources {
		fmt.Printf("  来源 %s：%d 个包\n", s, n)
	}
	if seq > 0 && firstAck > 0 {
		fmt.Printf("  从开始发包到双向打通：%v\n", (firstAck - firstSend).Round(time.Millisecond))
	}
	if firstHI > 0 {
		fmt.Printf("  首个 HI 到达：+%v\n", firstHI.Round(time.Millisecond))
	}
	if len(rtts) > 0 {
		slices.Sort(rtts)
		q := func(p float64) time.Duration { return rtts[int(p*float64(len(rtts)-1))].Round(100 * time.Microsecond) }
		fmt.Printf("  RTT min %v  p50 %v  p95 %v  max %v\n", rtts[0], q(0.5), q(0.95), rtts[len(rtts)-1].Round(100*time.Microsecond))
	}
	if bestRTT > 0 {
		fmt.Printf("  OFFSET %v（对端时钟 - 本机时钟，误差 ≤ ±%v）\n", offset.Round(100*time.Microsecond), (bestRTT / 2).Round(100*time.Microsecond))
	}
}
