package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"uulab/fakeenc"
)

const alpn = "uulab-xfer"

func quicConf() *quic.Config {
	return &quic.Config{
		MaxIncomingUniStreams: 1 << 20, // 每帧一个流，60fps 很快就会超过默认的 100
		MaxIdleTimeout:        10 * time.Second,
	}
}

type quicSink struct {
	conn     *quic.Conn
	deadline time.Duration
	wg       sync.WaitGroup
}

func dialQUIC(o opts) (frameSink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tc := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}} // 实验用自签证书
	conn, err := quic.DialAddr(ctx, o.addr, tc, quicConf())
	if err != nil {
		return nil, err
	}
	return &quicSink{conn: conn, deadline: o.deadline}, nil
}

// Send 为每帧开一个单向流。到取消时刻还没送达就 CancelWrite（在 Close 之后调用也有效，会放弃未确认的数据），
// 这样一帧丢了只影响它自己，不会像 TCP 那样堵住后面的帧。
//
// 取消时刻 = 截止时间 + SRTT/2，与 own 对齐：own 在截止时间之前都还会重传，最后一个重传包要再过约半个 RTT 才到，
// 接收端把它记为"迟到"而不是"丢失"。如果 QUIC 恰好在截止时间就丢弃，同样的帧会被记成丢失、断开解码链，对 QUIC 不公平。
func (k *quicSink) Send(f fakeenc.Frame) {
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		cancelAt := f.At.Add(k.deadline + k.conn.ConnectionStats().SmoothedRTT/2)
		ctx, cancel := context.WithDeadline(context.Background(), cancelAt)
		defer cancel()
		str, err := k.conn.OpenUniStreamSync(ctx)
		if err != nil {
			return
		}
		str.SetWriteDeadline(cancelAt) // 被流控/拥塞控制卡住的 Write 到点也要返回
		if writeFrame(str, f) == nil {
			str.Close()
		}
		<-ctx.Done()
		str.CancelWrite(0) // 已经全部送达确认的流上这是空操作
	}()
}

func (k *quicSink) Close(frames uint32) string {
	k.wg.Wait()
	if str, err := k.conn.OpenUniStream(); err == nil {
		writeBye(str, frames)
		str.Close()
	}
	time.Sleep(k.deadline + 500*time.Millisecond) // 等 BYE 和最后几帧送达
	st := k.conn.ConnectionStats()
	k.conn.CloseWithError(0, "")
	return fmt.Sprintf("[quic] 发送端：包 %d  字节 %.1fMB  判定丢失 %d  SRTT %v  MinRTT %v",
		st.PacketsSent, float64(st.BytesSent)/1e6, st.PacketsLost, st.SmoothedRTT.Round(100*time.Microsecond), st.MinRTT.Round(100*time.Microsecond))
}

func recvQUIC(o opts, rep *report) (uint32, error) {
	tc, err := selfSigned()
	if err != nil {
		return 0, err
	}
	ln, err := quic.ListenAddr(o.addr, tc, quicConf())
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	conn, err := ln.Accept(context.Background())
	if err != nil {
		return 0, err
	}
	bye := make(chan uint32, 1)
	go func() {
		for {
			str, err := conn.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				id, key, isBye, lat, err := readFrame(str)
				switch {
				case err != nil: // 被发送端在截止时间取消了
				case isBye:
					bye <- id
				default:
					rep.frame(id, key, lat)
				}
			}()
		}
	}()
	select {
	case total := <-bye:
		time.Sleep(o.deadline + 200*time.Millisecond) // 还在路上的帧最多再晚一个截止时间
		st := conn.ConnectionStats()
		fmt.Printf("[quic] 接收端：包 %d  SRTT %v\n", st.PacketsReceived, st.SmoothedRTT.Round(100*time.Microsecond))
		return total, nil
	case <-conn.Context().Done():
		return 0, context.Cause(conn.Context())
	}
}

func selfSigned() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{alpn},
	}, nil
}
