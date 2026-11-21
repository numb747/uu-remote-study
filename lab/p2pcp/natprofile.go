package main

// 本机 NAT 画像：先体检、再直传（docs/arch/DESIGN.md，图 33-nat-profile）。
//
// 映射行为：开一个 socket，同时问几台不同 IP 的公开 STUN。各台看到的映射都一样 = EIM（映射与目的地无关），
// 不一样 = EDM（对称型：每换一个目的地就换一个端口，对方从会合服务器拿到的那个端口对它没用）。
// 映射 IP 就是本机 IP = 没有 NAT（open）。
//
// 过滤行为（决定 EIM 是 cone 还是 port）：向会合服务器的一个端口发 /FT，服务器从这个端口回 /F1、从另一个监听端口回 /F2。
// 只收到 /F1 = 只放行发过包的 IP:端口（APDF）→ port；/F2 也收到 = 同 IP 换端口也能进来（ADF 或 EIF）→ cone。
// 4×4 矩阵里 full 和 restricted 的结果一样，不需要第二个 IP 去区分 EIF / ADF。测不出来（旧服务器、会合端口都不通）时 EIM 记 eim。
// STUN 和过滤测试各用各的 socket，并行做。
//
// 画像随会合报文 /R2 的标志发给对方：nat=<类别>[.v6]，见 flag / parseNat。

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// 默认的公开 STUN：都在国内，IP 各不相同（lab/real/pcphone.sh 用过的四台）。-stun 可改，off 关掉体检
const defaultSTUN = "111.206.174.2:3478,106.12.251.193:3478,162.159.207.0:3478,111.206.174.3:3478"

// NAT 类别。cone / port 要等过滤测试（第 2 步）；之前 EIM 都是 eim
const (
	natOpen = "open" // 没有 NAT：映射 IP = 本机 IP
	natCone = "cone" // EIM + 过滤宽松（ADF 或 EIF）
	natPort = "port" // EIM + 端口受限过滤（APDF）
	natEIM  = "eim"  // EIM，过滤还没测
	natSym  = "sym"  // EDM：映射随目的地变化
)

var natClasses = []string{natOpen, natCone, natPort, natEIM, natSym}

const (
	filterLoose = "loose" // 同 IP 换端口也能进来：ADF 或 EIF
	filterAPDF  = "apdf"  // 只放行发过包的 IP:端口
)

type profile struct {
	Class    string         // natOpen…natSym；空 = 未知（没测出来、对方是旧版）
	V6       bool           // 有全局 IPv6
	Local    *net.UDPAddr   // 探测用的本机地址
	Mapped   []*net.UDPAddr // 每台 STUN 看到的映射，没回的是 nil，顺序同 -stun
	PortKept bool           // 映射端口 = 本机端口
	Filter   string         // filterLoose / filterAPDF；空 = 没测出来
	Note     string         // 测不出来时的原因
	Took     time.Duration  // 现测用时；用缓存时为 0
	CachedAt time.Time      // 用的缓存是什么时候测的（零值 = 现测）
}

// natProfile 测本机画像。和打洞用同样的绑定方式（-bind / auto），开着 TUN 代理时才测得到真实出口
func natProfile(o opts) (p profile) {
	start := time.Now()
	p.V6 = !o.only4 && global6(o) != nil
	defer func() { p.Took = time.Since(start) }()
	var servers []*net.UDPAddr
	for _, s := range strings.Split(o.stun, ",") {
		if a, err := net.ResolveUDPAddr("udp4", strings.TrimSpace(s)); err == nil {
			servers = append(servers, a)
		}
	}
	if len(servers) == 0 {
		p.Note = "没有可用的 STUN 服务器（-stun）"
		return p
	}
	var ip net.IP
	switch o.bind {
	case "auto":
		ip = defaultRouteIP()
	case "":
	default:
		ip = net.ParseIP(o.bind)
	}
	filterDone := make(chan struct{})
	var filterNote string
	go func() {
		p.Filter, filterNote = probeFilter(ip, o.server)
		close(filterDone)
	}()
	defer func() {
		<-filterDone
		p.Class = refine(p.Class, p.Filter)
		if filterNote != "" && p.Class != "" && p.Class != natSym && p.Class != natOpen {
			p.Note = strings.TrimPrefix(p.Note+"；"+filterNote, "；")
		}
	}()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		p.Note = err.Error()
		return p
	}
	defer c.Close()
	p.Local = c.LocalAddr().(*net.UDPAddr)
	if ip == nil { // 没绑地址：问内核去第一台 STUN 走哪个源地址（connect 不发包）
		if d, err := net.DialUDP("udp4", nil, servers[0]); err == nil {
			p.Local = &net.UDPAddr{IP: d.LocalAddr().(*net.UDPAddr).IP, Port: p.Local.Port}
			d.Close()
		}
	}
	p.Mapped = stunAll(c, servers, 1500*time.Millisecond)
	p.Class, p.PortKept, p.Note = classify(p.Local, p.Mapped)
	return p
}

// classify 由各台 STUN 看到的映射定类别。至少两台回了才分得清 EIM / EDM
func classify(local *net.UDPAddr, mapped []*net.UDPAddr) (class string, portKept bool, note string) {
	var got []*net.UDPAddr
	for _, m := range mapped {
		if m != nil {
			got = append(got, m)
		}
	}
	if len(got) == 0 {
		return "", false, fmt.Sprintf("%d 台 STUN 都没回（UDP 可能被拦了）", len(mapped))
	}
	first := got[0]
	portKept = local != nil && first.Port == local.Port
	if len(got) < 2 {
		return "", portKept, "只有 1 台 STUN 回了，分不清映射行为"
	}
	for _, m := range got[1:] {
		if !m.IP.Equal(first.IP) || m.Port != first.Port {
			return natSym, false, ""
		}
	}
	if local != nil && first.IP.Equal(local.IP) {
		return natOpen, portKept, ""
	}
	return natEIM, portKept, ""
}

// refine 用过滤行为把 eim 细分成 cone / port；其他类别不变（sym 的过滤不影响预测，open 没有 NAT）
func refine(class, filter string) string {
	if class != natEIM {
		return class
	}
	switch filter {
	case filterLoose:
		return natCone
	case filterAPDF:
		return natPort
	}
	return class
}

// probeFilter 测过滤行为：依次试会合地址（从上次打通的那个开始），第一个有回音的说了算。
// 每个地址用新 socket：联系过 A 端口的 socket 再去测 B 端口，A 端口来的 /F2 会因为"发过包"被放行，误判成宽松
func probeFilter(ip net.IP, servers string) (filter, note string) {
	list := strings.Split(servers, ",")
	first := int(goodServer.Load())
	for i := range list {
		a := strings.TrimSpace(list[(first+i)%len(list)])
		if a == "" {
			continue
		}
		srv, err := net.ResolveUDPAddr("udp4", a)
		if err != nil {
			continue
		}
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err != nil {
			return "", err.Error()
		}
		got1, got2 := filterOnce(c, srv)
		c.Close()
		switch {
		case got2:
			return filterLoose, ""
		case got1:
			return filterAPDF, ""
		}
	}
	return "", "过滤没测出来（会合服务器没回 /FT，可能是旧版）"
}

// filterOnce 向 srv 发 /FT，每 300ms 重发；收到 /F1 后再等 500ms 的 /F2（期间照样重发，/F2 丢一个还有下一个），最多 1.2 秒
func filterOnce(c *net.UDPConn, srv *net.UDPAddr) (got1, got2 bool) {
	nonce := randHex(8)
	req := []byte("/FT " + nonce)
	start := time.Now()
	deadline := start.Add(1200 * time.Millisecond)
	var t1 time.Time
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) && !got2 {
		if got1 && time.Since(t1) >= 500*time.Millisecond {
			break
		}
		c.WriteToUDP(req, srv)
		next := time.Now().Add(300 * time.Millisecond)
		if next.After(deadline) {
			next = deadline
		}
		c.SetReadDeadline(next)
		for !got2 {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if !from.IP.Equal(srv.IP) {
				continue
			}
			switch f := strings.Fields(string(buf[:n])); {
			case len(f) == 2 && f[0] == "/F1" && f[1] == nonce && from.Port == srv.Port && !got1:
				got1, t1 = true, time.Now()
			case len(f) == 2 && f[0] == "/F2" && f[1] == nonce && from.Port != srv.Port:
				got2 = true
			}
		}
	}
	return
}

// flag 编码成会合标志 nat=<类别>[.v6]；类别未知时返回空串（不发）
func (p profile) flag() string {
	if p.Class == "" {
		return ""
	}
	if p.V6 {
		return "nat=" + p.Class + ".v6"
	}
	return "nat=" + p.Class
}

// parseNat 从对方的会合标志（逗号分隔，如 "relay,nat=sym.v6"）里取画像；没有或不认识时 Class 为空
func parseNat(flags string) profile {
	for _, f := range strings.Split(flags, ",") {
		v, ok := strings.CutPrefix(f, "nat=")
		if !ok {
			continue
		}
		parts := strings.Split(v, ".")
		var p profile
		for _, c := range natClasses {
			if parts[0] == c {
				p.Class = c
			}
		}
		for _, x := range parts[1:] {
			p.V6 = p.V6 || x == "v6"
		}
		if p.Class == "" {
			p.Note = "不认识的类别 " + parts[0]
		}
		return p
	}
	return profile{Note: "没报画像（旧版 p2pcp）"}
}

// String 给人看的一行，如「eim（4/4 台 STUN 一致 120.1.4.60:40629，端口保持）+ IPv6，用时 85ms」
func (p profile) String() string {
	var b strings.Builder
	switch p.Class {
	case "":
		b.WriteString("未知")
	case natOpen:
		b.WriteString("open 没有 NAT")
	case natCone:
		b.WriteString("cone 锥形（EIM，过滤宽松）")
	case natPort:
		b.WriteString("port 端口受限锥（EIM + APDF）")
	case natEIM:
		b.WriteString("eim 映射与目的地无关（过滤没测出来）")
	case natSym:
		b.WriteString("sym 对称型（映射随目的地变）")
	}
	var detail []string
	if n := len(p.Mapped); n > 0 {
		var got []string
		for _, m := range p.Mapped {
			if m != nil {
				got = append(got, m.String())
			}
		}
		switch {
		case p.Class == natSym:
			detail = append(detail, fmt.Sprintf("%d/%d 台 STUN 看到 %s", len(got), n, strings.Join(got, " ")))
		case len(got) > 0:
			detail = append(detail, fmt.Sprintf("%d/%d 台 STUN 一致 %s", len(got), n, got[0]))
		}
		if p.PortKept {
			detail = append(detail, "端口保持")
		}
		switch p.Filter {
		case filterLoose:
			detail = append(detail, "同 IP 换端口能进来")
		case filterAPDF:
			detail = append(detail, "只放行发过包的 IP:端口")
		}
	}
	if p.Note != "" {
		detail = append(detail, p.Note)
	}
	if len(detail) > 0 {
		b.WriteString("（" + strings.Join(detail, "，") + "）")
	}
	if p.V6 {
		b.WriteString(" + IPv6")
	}
	switch {
	case !p.CachedAt.IsZero():
		fmt.Fprintf(&b, "，缓存（%v 前测的）", time.Since(p.CachedAt).Round(time.Second))
	case p.Took >= time.Millisecond:
		fmt.Fprintf(&b, "，用时 %v", p.Took.Round(time.Millisecond))
	case p.Took > 0:
		b.WriteString("，用时 <1ms")
	}
	return b.String()
}
