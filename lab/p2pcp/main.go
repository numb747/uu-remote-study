// p2pcp: 打洞直连传文件。两端各执行一条命令：经会合服务器交换地址 → 打洞 → 在打通的同一个 UDP socket 上跑 QUIC。
//
//	p2pcp recv   -room demo [-o 目录]              收一次
//	p2pcp daemon -room demo [-o 目录]              常驻：可以同时接好几个发送端
//	p2pcp send   -room demo [-conns 2] 文件...     发送（多个文件在同一次连接里依次发；断了自动重连、断点续传）
//	p2pcp check                                   网络体检：会合端口通不通、有没有"毒端口"、NAT 映射、IPv6、中继（check.go）
//	p2pcp expose  -room demo [-to 127.0.0.1:22]       常驻：把本机一个 TCP 端口经打洞暴露给对方（tunnel.go）
//	p2pcp forward -room demo [-listen 127.0.0.1:2223] 本机端口转发到 expose 那端，比如 rsync 走 ssh -p 2223
//
// 分层：会合 + 打洞 + 中继兜底（rdv.go）→ QUIC 接管 socket（link.go）→ 用房间密钥做通道绑定认证（auth.go）
// → 传文件（transfer.go）/ 端口转发 + 连接迁移（tunnel.go）。
//
// 房间名是共享密钥：线路上只出现它的 HMAC（房间号），QUIC 握手后两端再互相证明知道它（auth.go）。
// -conns N 只打一个洞，在同一个 socket 上开 N 条 QUIC 连接，各有各的拥塞控制。
// 默认拥塞控制是 Hysteria 的 BBR（cc.go）：5G 上的乱序会被 Cubic 误判成丢包（docs/09 第 7 节）。
//
// 有全局 IPv6 时，IPv4 和 IPv6 同时打洞，先通的用（connectOnce）。打不通时走会合服务器上的中继（-relay）。
//
// 给 GUI 用的两个开关：-progress 在 stdout 输出机器可读的 @STATE/@PROGRESS/@DONE/@ERROR 行（手机 App 解析它），
// -notify 用 notify-send 弹桌面通知并原地更新进度（电脑端常驻服务用）。
//
// 打洞消息和会合 v2 报文都以 "/"（0x2f）开头：quic-go 只把首字节 0x40 位为 0 的包交给 ReadNonQUICPacket，
// 这样 QUIC 接管 socket 之后，对方迟到的打洞包、会合服务器的回执仍然能被识别。
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	msgPunch = "/PP" // 探测（按前 3 字节识别；首字节最高两位必须是 0，quic-go 才会交给 ReadNonQUICPacket）
	msgAck   = "/PA" // 回应
	msgNom   = "/PN" // 提名：发送端告诉接收端"就用这条"（两端都有 IPv4 + IPv6 两个 socket 时才需要）
)

// 默认会合地址：同一台服务器的几个端口，房间共享。7001 放前面：7000 在某台电信 CGNAT 下有毒（docs/09 第 7 节）
const defaultServers = "8.140.18.211:7001,8.140.18.211:7000"

// 实际发出的打洞包补到 48 字节，别太像异常小包。（2026-10-17 排查电信 CGNAT 下双向全丢时试过，真正的原因是会合
// 服务器用了 UDP 7000，见 docs/09 第 7 节；填充本身无害，保留）
var (
	pktPunch = pad(msgPunch)
	pktAck   = pad(msgAck)
	pktNom   = pad(msgNom)
)

func pad(m string) []byte {
	b := make([]byte, 48)
	copy(b, m)
	return b
}

type opts struct {
	server   string
	room     string
	bind     string
	public   string
	lan      string
	only4    bool // 不开 IPv6 socket
	only6    bool // 只向对方的 IPv6 候选打洞（测试用；会合仍走 IPv4）
	conns    int
	cc       string
	relay    string // auto | off | always
	out      string
	to       string        // expose：转发目标
	listen   string        // forward：本地监听地址
	timeout  time.Duration // 等对方的超时，0 = 一直等（daemon）
	regEvery time.Duration // 注册间隔
	rep      *reporter
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	role := os.Args[1]
	switch role {
	case "send", "recv", "daemon", "check", "expose", "forward":
	default:
		usage()
	}
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.server, "server", defaultServers, "会合服务器 IP:端口，逗号分隔多个时每轮换一个（服务器那边房间共享，docs/09 第 8 节）")
	fs.StringVar(&o.room, "room", "p2pcp", "房间名，两端要一致（当作共享密钥用：取一个别人猜不到的）")
	fs.StringVar(&o.bind, "bind", "", "绑定的本机 IP（开着 TUN 代理时用物理网卡地址）；auto = 每轮取 main 表默认路由那块网卡的地址")
	fs.StringVar(&o.public, "public", "", "本机自己的公网 IP，作为额外候选（1:1 NAT 的云服务器用：会合服务器在本机时看到的是 127.0.0.1）")
	fs.StringVar(&o.lan, "lan", "", "本机内网 IP 和全局 IPv6 列表（逗号分隔），代替自动探测（Android 11+ 的 App 里 Go 读不到网卡列表）")
	fs.BoolVar(&o.only4, "4", false, "只走 IPv4")
	fs.BoolVar(&o.only6, "6", false, "只走 IPv6（两端都要有全局 IPv6）")
	fs.IntVar(&o.conns, "conns", 2, "发送端：并行 QUIC 连接数（共用一个打通的洞；BBR 下 1~2 条就能跑满，docs/09 第 7 节）")
	fs.StringVar(&o.relay, "relay", "auto", "打不通时走会合服务器上的中继：auto（发送端两轮打不通才用）| off | always（测试用）")
	fs.StringVar(&o.cc, "cc", "bbr", "发送端：拥塞控制 bbr | cubic | brutal:45M（见 cc.go）")
	fs.StringVar(&o.out, "o", ".", "接收端：保存目录")
	fs.StringVar(&o.to, "to", "127.0.0.1:22", "expose：对方开的每条流都接到这个 TCP 地址")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:2223", "forward：本地监听地址")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "等对方的超时（daemon 模式下无效）")
	progress := fs.Bool("progress", false, "在 stdout 输出机器可读的进度行")
	notify := fs.Bool("notify", false, "用 notify-send 弹桌面通知")
	fs.Parse(os.Args[2:])
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	o.rep = &reporter{machine: *progress, notify: *notify}
	o.regEvery = 500 * time.Millisecond
	exitGracefully()

	var err error
	switch role {
	case "send":
		if fs.NArg() == 0 {
			log.Fatal("send 需要文件参数")
		}
		err = send(o, fs.Args())
	case "recv":
		err = recv(o)
	case "check":
		err = check(o)
	case "expose":
		err = expose(o)
	case "forward":
		err = forward(o)
	case "daemon":
		daemon(o)
	}
	if err != nil {
		o.rep.fail(err)
		os.Exit(1)
	}
}

// keepServing 常驻（daemon、expose）：在房间里等对方，每接上一个就交给 serve 在后台处理，自己马上回房间接着等。
// 所以可以同时服务好几个对端；对方换网、崩溃后重新连过来，也不用等旧连接超时。
// 5 秒注册一次就够了（家用路由器映射寿命 60 秒以上，docs/08）。对方一注册，服务器会立刻把 PEER 推过来
func keepServing(o opts, waiting string, accept func(opts) (*link, error), serve func(*link)) {
	o.timeout, o.regEvery = 0, 5*time.Second
	backoff := time.Duration(0)
	var active atomic.Int32 // 正在服务的对端数：有人在传时不报"等待"，免得盖掉界面上的进度
	for {
		if active.Load() == 0 {
			o.rep.state(waiting)
		}
		l, err := accept(o)
		switch {
		case err == nil:
			backoff = 0
			active.Add(1)
			go func() {
				serve(l)
				if active.Add(-1) == 0 {
					o.rep.state(waiting)
				}
			}()
		case errors.Is(err, errNoRendezvous):
			// 空等时连不上会合服务器（断网、换网络、服务器重启）：没人在用，不弹失败通知，只更新状态，
			// 重试间隔从 5 秒拉长到 1 分钟，网络回来后最多一分钟内自己恢复
			backoff = min(max(2*backoff, 5*time.Second), time.Minute)
			log.Print(err)
			o.rep.state(fmt.Sprintf("连不上会合服务器，%v 后重试", backoff))
			time.Sleep(backoff)
		default:
			backoff = 0
			o.rep.fail(err)
			time.Sleep(time.Second)
		}
	}
}

// exitGracefully：收到 SIGTERM / SIGINT 先主动关掉所有 QUIC 连接（closeLive），再按原信号退出，
// systemd、App（Process.destroy）看到的退出方式不变
func exitGracefully() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() {
		s := <-sig
		closeLive()
		signal.Reset()
		syscall.Kill(os.Getpid(), s.(syscall.Signal))
	}()
}

func usage() {
	fmt.Println("usage: p2pcp send|recv|daemon|check|expose|forward [flags] [文件...]")
	os.Exit(2)
}
