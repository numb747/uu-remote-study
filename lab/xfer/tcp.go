package main

import (
	"fmt"
	"net"

	"uulab/fakeenc"
)

// tcpSink 把所有帧按顺序写进一条 TCP 连接。TCP 没法放弃过期数据：
// 一个包丢了，后面所有帧都要等它重传完才能交给应用 —— 这就是队头阻塞。
type tcpSink struct {
	conn     net.Conn
	ch       chan fakeenc.Frame
	done     chan struct{}
	maxQueue int
}

func dialTCP(o opts) (frameSink, error) {
	conn, err := net.Dial("tcp4", o.addr)
	if err != nil {
		return nil, err
	}
	k := &tcpSink{conn: conn, ch: make(chan fakeenc.Frame, 100000), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		for f := range k.ch {
			if writeFrame(conn, f) != nil {
				return
			}
		}
	}()
	return k, nil
}

func (k *tcpSink) Send(f fakeenc.Frame) {
	k.maxQueue = max(k.maxQueue, len(k.ch))
	k.ch <- f
}

func (k *tcpSink) Close(frames uint32) string {
	close(k.ch)
	<-k.done
	writeBye(k.conn, frames)
	k.conn.Close()
	return fmt.Sprintf("[tcp] 发送端：应用层排队最多 %d 帧", k.maxQueue)
}

func recvTCP(o opts, rep *report) (uint32, error) {
	ln, err := net.Listen("tcp4", o.addr)
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	conn, err := ln.Accept()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	for {
		id, key, bye, lat, err := readFrame(conn)
		if err != nil {
			return 0, err
		}
		if bye {
			return id, nil
		}
		rep.frame(id, key, lat)
	}
}
