package main

// 最小 STUN（同 natprobe/stun.go）：只有 Binding Request/Response 和 XOR-MAPPED-ADDRESS，体检测映射行为用。

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

const magicCookie = 0x2112A442

func bindingRequest() (msg []byte, txid [12]byte) {
	rand.Read(txid[:])
	msg = make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:], 0x0001)
	binary.BigEndian.PutUint32(msg[4:], magicCookie)
	copy(msg[8:], txid[:])
	return
}

// bindingResponse 回应 req，XOR-MAPPED-ADDRESS = mapped（仅 IPv4）。测试里的假 STUN 服务器用
func bindingResponse(req []byte, mapped *net.UDPAddr) []byte {
	ip := mapped.IP.To4()
	if ip == nil || len(req) < 20 {
		return nil
	}
	b := make([]byte, 32)
	binary.BigEndian.PutUint16(b[0:], 0x0101)
	binary.BigEndian.PutUint16(b[2:], 12)
	copy(b[4:20], req[4:20]) // cookie + txid
	binary.BigEndian.PutUint16(b[20:], 0x0020)
	binary.BigEndian.PutUint16(b[22:], 8)
	b[25] = 0x01
	binary.BigEndian.PutUint16(b[26:], uint16(mapped.Port)^uint16(magicCookie>>16))
	binary.BigEndian.PutUint32(b[28:], binary.BigEndian.Uint32(ip)^magicCookie)
	return b
}

// parseMapped 解析 Binding 成功响应，返回事务号和映射地址
func parseMapped(b []byte) (txid [12]byte, mapped *net.UDPAddr, err error) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != 0x0101 || binary.BigEndian.Uint32(b[4:]) != magicCookie {
		return txid, nil, errors.New("不是 Binding 成功响应")
	}
	copy(txid[:], b[8:20])
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
				return txid, &net.UDPAddr{IP: ip, Port: int(port)}, nil
			}
		case 0x0001: // MAPPED-ADDRESS（老服务器）
			if len(v) >= 8 && v[1] == 0x01 {
				mapped = &net.UDPAddr{IP: net.IP(append([]byte(nil), v[4:8]...)), Port: int(binary.BigEndian.Uint16(v[2:]))}
			}
		}
		attrs = attrs[4+(l+3)&^3:]
	}
	if mapped == nil {
		return txid, nil, errors.New("没有映射地址")
	}
	return txid, mapped, nil
}

// stunAll 用同一个 socket 同时问所有服务器，返回各自看到的映射（没回的是 nil），顺序同 servers。
// 每 300ms 给还没回的重发一次，最多等 wait；已经有两台回了、又过了 600ms，就不再等剩下的（公开 STUN 挂一台很常见，
// 不能让每次体检都白等）
func stunAll(c *net.UDPConn, servers []*net.UDPAddr, wait time.Duration) []*net.UDPAddr {
	out := make([]*net.UDPAddr, len(servers))
	reqs := make([][]byte, len(servers))
	idx := map[[12]byte]int{}
	for i := range servers {
		var txid [12]byte
		reqs[i], txid = bindingRequest()
		idx[txid] = i
	}
	deadline := time.Now().Add(wait)
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 1500)
	start := time.Now()
	for left := len(servers); left > 0 && time.Now().Before(deadline); {
		if len(servers)-left >= 2 && time.Since(start) >= 600*time.Millisecond {
			break
		}
		for i, s := range servers {
			if out[i] == nil {
				c.WriteToUDP(reqs[i], s)
			}
		}
		next := time.Now().Add(300 * time.Millisecond)
		if next.After(deadline) {
			next = deadline
		}
		c.SetReadDeadline(next)
		for left > 0 {
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			txid, m, err := parseMapped(buf[:n])
			if err != nil {
				continue
			}
			if i, ok := idx[txid]; ok && out[i] == nil {
				out[i] = m
				left--
			}
		}
	}
	return out
}
