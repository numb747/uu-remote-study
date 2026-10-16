package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// 与 stunprobe 相同的最小 STUN 实现：只有 Binding Request/Response 和 XOR-MAPPED-ADDRESS。
// 服务端也用它回应，这样客户端可以把自家服务器和公共 STUN 服务器一视同仁。

const magicCookie = 0x2112A442

func bindingRequest() (msg []byte, txid [12]byte) {
	rand.Read(txid[:])
	msg = make([]byte, 20)
	binary.BigEndian.PutUint16(msg[0:], 0x0001)
	binary.BigEndian.PutUint32(msg[4:], magicCookie)
	copy(msg[8:], txid[:])
	return
}

func isBindingRequest(b []byte) bool {
	return len(b) >= 20 && binary.BigEndian.Uint16(b[0:]) == 0x0001 && binary.BigEndian.Uint32(b[4:]) == magicCookie
}

// bindingResponse 回应 req，带上 XOR-MAPPED-ADDRESS = from（仅 IPv4）。
func bindingResponse(req []byte, from *net.UDPAddr) []byte {
	ip := from.IP.To4()
	if ip == nil {
		return nil
	}
	b := make([]byte, 32)
	binary.BigEndian.PutUint16(b[0:], 0x0101)
	binary.BigEndian.PutUint16(b[2:], 12)
	copy(b[4:20], req[4:20]) // cookie + txid
	binary.BigEndian.PutUint16(b[20:], 0x0020)
	binary.BigEndian.PutUint16(b[22:], 8)
	b[25] = 0x01
	binary.BigEndian.PutUint16(b[26:], uint16(from.Port)^uint16(magicCookie>>16))
	binary.BigEndian.PutUint32(b[28:], binary.BigEndian.Uint32(ip)^magicCookie)
	return b
}

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
		attrs = attrs[4+(l+3)&^3:]
	}
	if mapped == nil {
		return nil, errors.New("no mapped address attribute")
	}
	return mapped, nil
}

// query 用 conn 向 server 发 Binding Request，返回映射地址。丢包时重传 3 次。
func query(conn *net.UDPConn, server *net.UDPAddr) (*net.UDPAddr, time.Duration, error) {
	req, txid := bindingRequest()
	buf := make([]byte, 1500)
	for try := 0; try < 3; try++ {
		start := time.Now()
		conn.WriteToUDP(req, server)
		conn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if !from.IP.Equal(server.IP) {
				continue
			}
			if m, err := parseMapped(buf[:n], txid); err == nil {
				conn.SetReadDeadline(time.Time{})
				return m, time.Since(start), nil
			}
		}
	}
	conn.SetReadDeadline(time.Time{})
	return nil, 0, errors.New("timeout")
}
