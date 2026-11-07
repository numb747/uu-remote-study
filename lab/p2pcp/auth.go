package main

// 认证：房间名就是两端的共享密钥，但它不能出现在线路上，QUIC 也不能只加密不认人。
//
//   - 会合时报的是房间号 = HMAC(密钥, "rendezvous") 的前 16 字节。路上的人、会合服务器（和它的日志）都只看到房间号，
//     拿它推不回房间名；房间名本身要够随机（install.sh 生成 12 字节随机数），否则可以离线猜。
//   - QUIC 证书是自签的、不校验，所以握手完成后两端各证明一次"我知道密钥"：
//     证明 = HMAC(密钥, 角色 | EKM)。EKM（TLS 导出密钥材料，RFC 5705/8446）每次握手都不同，中间人两头各握一次手、
//     两边 EKM 不一样，转发对方的证明也对不上——这叫通道绑定，不需要 PKI 也不怕中间人。
//     证明里带角色，对方也没法把我们的证明原样反射回来。
//
// 第一条流（客户端开的双向流）专门用来交换证明，之后才是业务流。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/apernet/quic-go"
)

// errAuth：对方不知道房间密钥（或者路上有人冒充）
var errAuth = errors.New("对方没有通过房间密钥认证")

func roomSecret(room string) []byte {
	h := sha256.Sum256([]byte("p2pcp/2 room " + room))
	return h[:]
}

func mac(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// roomID 是会合服务器上用的房间号
func roomID(secret []byte) string {
	return hex.EncodeToString(mac(secret, []byte("rendezvous"))[:16])
}

// connSecret 是这条 QUIC 连接两端共有、别人不知道的随机值（EKM 派生），用作连接迁移时的会合房间号（tunnel.go）
func connSecret(conn *quic.Conn, label string) string {
	tls := conn.ConnectionState().TLS
	ekm, err := tls.ExportKeyingMaterial("p2pcp "+label, nil, 16)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(ekm)
}

// authConn 交换证明，失败就关连接。client = QUIC Dial 的一方
func authConn(conn *quic.Conn, secret []byte, client bool) error {
	tls := conn.ConnectionState().TLS
	ekm, err := tls.ExportKeyingMaterial("p2pcp auth", nil, 32)
	if err != nil {
		return err
	}
	proof := func(role string) []byte { return mac(secret, []byte(role), ekm) }
	// 服务端 5 秒：接入是串行的（keepServing），连上来却不给证明的对端最多占住这么久
	wait := 5 * time.Second
	if client {
		wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	var str *quic.Stream
	if client {
		str, err = conn.OpenStreamSync(ctx)
	} else {
		str, err = conn.AcceptStream(ctx)
	}
	if err != nil {
		return fmt.Errorf("认证：%w", err)
	}
	str.SetDeadline(time.Now().Add(wait))
	mine, want := proof("server"), proof("client")
	if client {
		mine, want = want, mine
		if _, err := str.Write(mine); err != nil {
			return fmt.Errorf("认证：%w", err)
		}
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(str, got); err != nil {
		return fmt.Errorf("认证：%w", err)
	}
	if !hmac.Equal(got, want) {
		conn.CloseWithError(0x50, "auth")
		return errAuth
	}
	if !client { // 服务端先验对方、再给自己的证明：不知道密钥的人连一个字节的证明都拿不到
		if _, err := str.Write(mine); err != nil {
			return fmt.Errorf("认证：%w", err)
		}
	}
	str.Close()
	return nil
}
