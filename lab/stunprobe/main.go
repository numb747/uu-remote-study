// stunprobe: 用同一个本地 UDP 端口向多个 STUN 服务器发 Binding Request，
// 比较各服务器看到的公网映射地址，判断 NAT 的映射行为（RFC 4787 / 5780 的思路）。
//
//   映射地址全部相同      → 端点无关映射 (EIM，俗称 cone / NAT1~3)，打洞容易
//   不同服务器端口不同    → 端点相关映射 (EDM，俗称 symmetric / NAT4)，需要端口预测或中继
//
// 用法: go run . [-port 0] [-servers a:3478,b:3478]
package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const magicCookie = 0x2112A442

var defaultServers = []string{
	"stun.miwifi.com:3478",
	"stun.chat.bilibili.com:3478",
	"stun.l.google.com:19302",
	"stun.cloudflare.com:3478",
}

// bindingRequest 构造 20 字节 STUN 头：type=0x0001, length=0, cookie, 96bit 事务 ID。
func bindingRequest() (msg []byte, txid [12]byte) {
	rand.Read(txid[:])
	msg = make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:], 0x0001)
	binary.BigEndian.PutUint16(msg[2:], 0)
	binary.BigEndian.PutUint32(msg[4:], magicCookie)
	copy(msg[8:], txid[:])
	return
}

// parseMapped 从 Binding Success Response 中取 XOR-MAPPED-ADDRESS（优先）或 MAPPED-ADDRESS。
func parseMapped(b []byte, txid [12]byte) (*net.UDPAddr, error) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != 0x0101 {
		return nil, errors.New("not a binding success response")
	}
	if string(b[8:20]) != string(txid[:]) {
		return nil, errors.New("transaction id mismatch")
	}
	var mapped *net.UDPAddr
	attrs := b[20:]
	for len(attrs) >= 4 {
		typ := binary.BigEndian.Uint16(attrs[0:])
		l := int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+l > len(attrs) {
			break
		}
		v := attrs[4 : 4+l]
		switch typ {
		case 0x0020: // XOR-MAPPED-ADDRESS
			if len(v) >= 8 && v[1] == 0x01 {
				port := binary.BigEndian.Uint16(v[2:]) ^ uint16(magicCookie>>16)
				ip := make(net.IP, 4)
				binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(v[4:])^magicCookie)
				return &net.UDPAddr{IP: ip, Port: int(port)}, nil
			}
		case 0x0001: // MAPPED-ADDRESS（老服务器）
			if len(v) >= 8 && v[1] == 0x01 {
				mapped = &net.UDPAddr{IP: net.IP(append([]byte(nil), v[4:8]...)), Port: int(binary.BigEndian.Uint16(v[2:]))}
			}
		}
		attrs = attrs[4+(l+3)&^3:] // 属性按 4 字节对齐
	}
	if mapped == nil {
		return nil, errors.New("no mapped address attribute")
	}
	return mapped, nil
}

func query(conn *net.UDPConn, server string) (*net.UDPAddr, time.Duration, error) {
	raddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, 0, err
	}
	req, txid := bindingRequest()
	buf := make([]byte, 1500)
	for try := 0; try < 3; try++ { // UDP 会丢，简单重传
		start := time.Now()
		conn.WriteToUDP(req, raddr)
		conn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if !from.IP.Equal(raddr.IP) {
				continue
			}
			if m, err := parseMapped(buf[:n], txid); err == nil {
				return m, time.Since(start), nil
			}
		}
	}
	return nil, 0, fmt.Errorf("timeout")
}

func main() {
	port := flag.Int("port", 0, "本地 UDP 端口 (0=随机)")
	servers := flag.String("servers", strings.Join(defaultServers, ","), "逗号分隔的 STUN 服务器")
	flag.Parse()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Printf("本地地址: %s\n\n", conn.LocalAddr())

	var results []*net.UDPAddr
	for _, s := range strings.Split(*servers, ",") {
		m, rtt, err := query(conn, strings.TrimSpace(s))
		if err != nil {
			fmt.Printf("%-32s  失败: %v\n", s, err)
			continue
		}
		fmt.Printf("%-32s  映射 %-22s  RTT %v\n", s, m, rtt.Round(time.Millisecond))
		results = append(results, m)
	}
	fmt.Println()
	if len(results) < 2 {
		fmt.Println("有效结果不足 2 个，无法判断映射行为")
		return
	}

	sameIP, samePort := true, true
	for _, r := range results[1:] {
		sameIP = sameIP && r.IP.Equal(results[0].IP)
		samePort = samePort && r.Port == results[0].Port
	}
	local := conn.LocalAddr().(*net.UDPAddr).Port
	switch {
	case !sameIP:
		fmt.Println("⚠ 不同服务器看到的公网 IP 不同：多出口/负载均衡 NAT，或部分流量走了代理（检查 sing-box/TUN 分流）")
	case samePort:
		fmt.Printf("✔ 端点无关映射 (EIM / cone)：公网 %s\n", results[0])
		if results[0].Port == local {
			fmt.Println("  且端口保持 (port preserving)，对端可以直接猜到你的端口")
		}
		fmt.Println("  → 与大多数对端 UDP 打洞都能成功。过滤行为(NAT1/2/3)需要 RFC5780 服务器的 CHANGE-REQUEST 才能测。")
	default:
		fmt.Println("✘ 端点相关映射 (EDM / symmetric / NAT4)：每个目的地分配不同端口")
		for i := 1; i < len(results); i++ {
			fmt.Printf("  端口增量 %+d\n", results[i].Port-results[i-1].Port)
		}
		fmt.Println("  → 增量若稳定（如 +1/+2）可做端口预测；随机则只能靠对端是 cone + 生日攻击喷洒，或走中继")
	}
}
