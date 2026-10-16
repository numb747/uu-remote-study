// xfer: 传输层对比测试工具（M4）。发送端用假编码器产生 1080p60 量级的帧流，
// 分别走三种传输，接收端用同一套口径统计帧延迟、按时率和可播放率。
//
//	own   自研 UDP 传输（lab/transport）：分片 + NACK 重传 + 截止时间丢弃
//	quic  quic-go：每帧一个单向流，过截止时间就 CancelWrite；重传、拥塞控制、发包节奏全交给 QUIC
//	tcp   一条 TCP 连接按顺序发所有帧：用来看队头阻塞
//
// 用法:
//
//	xfer recv -mode own -listen :9000
//	xfer send -mode own -to 1.2.3.4:9000 -dur 20s -bitrate 20000000
//
// 帧延迟 = 接收端收齐时刻 - 发送端编码完成时刻，两端时钟需要同步（netns 实验室里是同一台机器，天然同步）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"uulab/fakeenc"
)

type opts struct {
	mode     string
	addr     string
	deadline time.Duration
	dur      time.Duration
	fps      int
	bitrate  int
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "send" && os.Args[1] != "recv") {
		fmt.Println("usage: xfer send|recv [flags]")
		os.Exit(2)
	}
	role := os.Args[1]
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.mode, "mode", "own", "传输方式: own | quic | tcp")
	fs.StringVar(&o.addr, "listen", ":9000", "接收端监听地址")
	to := fs.String("to", "", "发送端：接收端地址")
	fs.DurationVar(&o.deadline, "deadline", 100*time.Millisecond, "帧截止时间：超过就算迟到/放弃")
	fs.DurationVar(&o.dur, "dur", 20*time.Second, "发送端：发送时长")
	fs.IntVar(&o.fps, "fps", 60, "帧率")
	fs.IntVar(&o.bitrate, "bitrate", 20_000_000, "发送端：码率 bit/s")
	fs.Parse(os.Args[2:])
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if role == "send" {
		if *to == "" {
			log.Fatal("send 需要 -to")
		}
		o.addr = *to
		if err := send(o); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := recv(o); err != nil {
		log.Fatal(err)
	}
}

// frameSink 是一种传输方式的发送端。
type frameSink interface {
	Send(f fakeenc.Frame)
	Close(frames uint32) string // 返回发送端统计
}

func send(o opts) error {
	var sink frameSink
	var err error
	switch o.mode {
	case "own":
		sink, err = dialOwn(o)
	case "quic":
		sink, err = dialQUIC(o)
	case "tcp":
		sink, err = dialTCP(o)
	default:
		return fmt.Errorf("未知模式 %q", o.mode)
	}
	if err != nil {
		return err
	}
	enc := fakeenc.New(fakeenc.Config{FPS: o.fps, Bitrate: o.bitrate})
	tick := time.NewTicker(enc.Interval())
	defer tick.Stop()
	end := time.Now().Add(o.dur)
	var n uint32
	for now := range tick.C {
		if now.After(end) {
			break
		}
		sink.Send(enc.Next())
		n++
	}
	fmt.Println(sink.Close(n))
	return nil
}

func recv(o opts) error {
	rep := newReport(o.deadline, o.fps)
	var total uint32
	var err error
	switch o.mode {
	case "own":
		total, err = recvOwn(o, rep)
	case "quic":
		total, err = recvQUIC(o, rep)
	case "tcp":
		total, err = recvTCP(o, rep)
	default:
		return fmt.Errorf("未知模式 %q", o.mode)
	}
	if err != nil {
		return err
	}
	fmt.Print(rep.summary(o.mode, total))
	fmt.Println()
	return nil
}
