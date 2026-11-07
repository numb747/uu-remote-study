package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

// quicPair 在本机回环上建一对 QUIC 连接
func quicPair(t *testing.T) (client, server *quic.Conn) {
	t.Helper()
	tc, err := selfSigned(alpn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", tc, quicConf(alpn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			t.Error(err)
		}
		accepted <- c
	}()
	client, err = quic.DialAddr(ctx, ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}, quicConf(alpn))
	if err != nil {
		t.Fatal(err)
	}
	return client, <-accepted
}

func TestAuth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		client, server string
		ok             bool
	}{
		{"同一个房间", "room-a", "room-a", true},
		{"房间密钥不同", "room-a", "room-b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := quicPair(t)
			errc := make(chan error, 1)
			go func() { errc <- authConn(s, roomSecret(tc.server), false) }()
			cerr := authConn(c, roomSecret(tc.client), true)
			serr := <-errc
			if tc.ok && (cerr != nil || serr != nil) {
				t.Fatalf("应当通过：client %v, server %v", cerr, serr)
			}
			if !tc.ok && !errors.Is(serr, errAuth) {
				t.Fatalf("服务端应当拒绝，得到 %v", serr)
			}
			if !tc.ok && cerr == nil {
				t.Fatal("客户端不应当通过")
			}
		})
	}
}

// 反射：把对方发来的证明原样送回去，不能通过
func TestAuthReflection(t *testing.T) {
	c, s := quicPair(t)
	secret := roomSecret("room-a")
	go func() {
		str, err := s.AcceptStream(context.Background())
		if err != nil {
			return
		}
		b := make([]byte, 32)
		if _, err := str.Read(b); err == nil {
			str.Write(b) // 不知道密钥的"服务端"只能把客户端的证明弹回去
		}
	}()
	if err := authConn(c, secret, true); !errors.Is(err, errAuth) {
		t.Fatalf("反射回来的证明不应当通过，得到 %v", err)
	}
}

func TestRoomIDHidesRoom(t *testing.T) {
	id := roomID(roomSecret("abc"))
	if len(id) != 32 || id == roomID(roomSecret("abd")) {
		t.Fatalf("房间号不对：%s", id)
	}
}

func TestComplement(t *testing.T) {
	for _, tc := range []struct {
		have [][2]uint64
		size uint64
		want [][2]uint64
	}{
		{nil, 10, [][2]uint64{{0, 10}}},
		{[][2]uint64{{0, 10}}, 10, nil},
		{[][2]uint64{{2, 3}, {4, 2}}, 10, [][2]uint64{{0, 2}, {6, 4}}}, // 重叠的合并
		{[][2]uint64{{8, 100}, {0, 1}}, 10, [][2]uint64{{1, 7}}},       // 乱序、超出文件的裁掉
		{[][2]uint64{{0, 4}, {4, 4}}, 10, [][2]uint64{{8, 2}}},         // 首尾相接
		{[][2]uint64{{20, 5}}, 10, [][2]uint64{{0, 10}}},               // 完全在文件外
		{[][2]uint64{{3, 0}}, 10, [][2]uint64{{0, 10}}},                // 空区间
		{[][2]uint64{{0, 3}, {5, 1}, {9, 1}}, 10, [][2]uint64{{3, 2}, {6, 3}}},
	} {
		if got := complement(tc.have, tc.size); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("complement(%v, %d) = %v，应为 %v", tc.have, tc.size, got, tc.want)
		}
	}
}

func TestQuietConnTranslates(t *testing.T) {
	a, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer a.Close()
	real, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer real.Close()
	fake := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} // quic-go 以为的对方地址
	q := &quietConn{UDPConn: a, quicSees: fake, real: real.LocalAddr().(*net.UDPAddr)}
	if _, _, err := q.WriteMsgUDP([]byte("hi"), nil, fake); err != nil {
		t.Fatal(err)
	}
	real.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 10)
	n, _, err := real.ReadFromUDP(b)
	if err != nil || string(b[:n]) != "hi" {
		t.Fatalf("发往旧地址的包应当到新地址：%q %v", b[:n], err)
	}
}
