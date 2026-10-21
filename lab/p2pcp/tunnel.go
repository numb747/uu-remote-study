package main

// 端口转发：打洞后的 QUIC 连接上，每条流对应一个 TCP 连接。用法见 main.go 开头。
//
// 典型用途是让 rsync/ssh 走打洞直连（docs/09 第 9 节）：RustDesk 的端口转发打不通时退回自建中继，被 99-server 出方向
// 3M 带宽卡死；这里打洞直连，rsync 的增量和断点续传照旧由它自己负责。
//
// 转发目标由 expose 那端的 -to 决定，forward 端选不了别的端口。房间名仍是唯一的门槛（QUIC 不校验证书，见 docs/arch/24），
// 所以只适合转发本身有认证和加密的服务，比如 ssh。

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/apernet/quic-go"
)

const tunALPN = "p2pcp-tun"

// expose 常驻：在房间里等对方，打通后把对方开的每条流接到 o.to；连接断了重新会合。
func expose(o opts) error {
	tc, err := selfSigned()
	if err != nil {
		return err
	}
	tc.NextProtos = []string{tunALPN}
	keepServing(o, fmt.Sprintf("等待对方连接（转发到 %s）", o.to), func(o opts) error {
		conn, s, err := acceptPeer(o, o.room, tc)
		if err != nil {
			return err
		}
		defer conn.CloseWithError(0, "")
		log.Printf("打通 %s，用时 %v", s.peer, s.took.Round(time.Millisecond))
		if err := applyCC(conn, o.cc); err != nil {
			return err
		}
		for {
			str, err := conn.AcceptStream(context.Background())
			if err != nil {
				log.Printf("连接断开：%v  %s", err, statsLine(conn))
				return nil
			}
			go func() {
				c, err := net.DialTimeout("tcp", o.to, 5*time.Second)
				if err != nil {
					log.Printf("连 %s 失败: %v", o.to, err)
					str.CancelRead(1)
					str.CancelWrite(1)
					return
				}
				splice(c.(*net.TCPConn), str)
			}()
		}
	})
	return nil
}

// forward 在 o.listen 上收 TCP 连接，每个开一条流发给对方。QUIC 连接按需建立，断了下一个 TCP 连接进来时重新打洞。
func forward(o opts) error {
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	log.Printf("监听 %s，经打洞转发给房间 %s 里的 expose 端", ln.Addr(), o.room)
	var (
		mu   sync.Mutex
		conn *quic.Conn
	)
	// open 只在连接确实断了时重新打洞：OpenStreamSync 因为别的原因失败（比如流数到了上限）就报错，
	// 不能把还活着、上面还有别的 ssh 的连接扔掉
	open := func() (*quic.Stream, error) {
		mu.Lock()
		if conn == nil || conn.Context().Err() != nil {
			c, err := dialTunnel(o)
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			conn = c
		}
		c := conn
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return c.OpenStreamSync(ctx)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			str, err := open()
			if err != nil {
				log.Printf("连不上对方: %v", err)
				c.Close()
				return
			}
			splice(c.(*net.TCPConn), str)
		}()
	}
}

func dialTunnel(o opts) (*quic.Conn, error) {
	conn, s, err := dialPeer(o, o.room, tunALPN)
	if err != nil {
		return nil, err
	}
	log.Printf("打通 %s，用时 %v", s.peer, s.took.Round(time.Millisecond))
	if err := applyCC(conn, o.cc); err != nil {
		conn.CloseWithError(0, "")
		return nil, err
	}
	go func() {
		<-conn.Context().Done()
		log.Printf("连接断开：%v  %s", context.Cause(conn.Context()), statsLine(conn))
	}()
	return conn, nil
}

// splice 双向对拷，各方向读到 EOF 后只关写端（半关闭），两个方向都结束再全部关掉。
func splice(c *net.TCPConn, str *quic.Stream) {
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := io.Copy(str, c); err != nil {
			str.CancelWrite(1)
		} else {
			str.Close()
		}
	})
	wg.Go(func() {
		if _, err := io.Copy(c, str); err != nil {
			str.CancelRead(1)
		}
		c.CloseWrite()
	})
	wg.Wait()
	c.Close()
}
