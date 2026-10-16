package main

import (
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"uulab/fakeenc"
	"uulab/transport"
)

type ownSink struct{ s *transport.Sender }

func dialOwn(o opts) (frameSink, error) {
	raddr, err := net.ResolveUDPAddr("udp4", o.addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, err
	}
	conn.SetWriteBuffer(4 << 20)
	s := transport.NewSender(conn, rand.Uint32(), o.deadline)
	go s.Run()
	return &ownSink{s}, nil
}

func (k *ownSink) Send(f fakeenc.Frame) { k.s.SendFrame(f.ID, f.Key, f.Size, f.At) }

func (k *ownSink) Close(frames uint32) string {
	time.Sleep(200 * time.Millisecond) // 给最后几帧的重传留时间
	st := k.s.Stats()
	k.s.Close(frames)
	return fmt.Sprintf("[own] 发送端：包 %d  字节 %.1fMB  重传 %d  收到 NACK %d（过期 %d、重复 %d）  SRTT %v",
		st.Packets, float64(st.Bytes)/1e6, st.Retransmits, st.NacksRecv, st.NackExpired, st.NackDup, st.SRTT.Round(100*time.Microsecond))
}

func recvOwn(o opts, rep *report) (uint32, error) {
	laddr, err := net.ResolveUDPAddr("udp4", o.addr)
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.SetReadBuffer(4 << 20)
	r := transport.NewReceiver(conn, o.deadline)
	r.OnFrame = func(e transport.FrameEvent) { rep.frame(e.ID, e.Key, e.Latency) }
	total, err := r.Run(10 * time.Second)
	st := r.Stats()
	fmt.Printf("[own] 接收端：包 %d  重复 %d  发 NACK %d  反馈包 %d  RTT 估计 %v\n",
		st.Packets, st.Dups, st.NacksSent, st.Feedbacks, st.RTT.Round(100*time.Microsecond))
	return total, err
}
