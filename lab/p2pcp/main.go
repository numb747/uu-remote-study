// p2pcp: 打洞直连传文件。两端各执行一条命令：经会合服务器交换地址 → 打洞 → 在打通的同一个 UDP socket 上跑 QUIC。
//
//	p2pcp recv   -room demo [-o 目录]              收一次
//	p2pcp daemon -room demo [-o 目录]              常驻：可以同时接好几个发送端
//	p2pcp send   -room demo [-conns 2] 文件...     发送（多个文件在同一次连接里依次发；断了自动重连、断点续传）
//	p2pcp check                                   网络体检：会合端口通不通、有没有"毒端口"、NAT 映射、IPv6、中继（check.go）
//	p2pcp check -room demo                        两端各跑一次：双端体检，画像、预测、实际打洞、往返和丢包、中继（check.go）
//	p2pcp expose  -room demo [-to 127.0.0.1:22]       常驻：把本机一个 TCP 端口经打洞暴露给对方（tunnel.go）
//
// 通讯录模式（pair.go、contacts.go）：每台设备有自己的设备密钥，好友之间的房间密钥由两边的密钥算出来，不用 -room：
//
//	p2pcp id [-name 设备名]                        看 / 改本机的设备名和设备 ID
//	p2pcp invite [-qr 图片.png | -qr -]            生成邀请码（24 小时有效，用一次作废），发给朋友
//	p2pcp join <邀请码>                            用朋友给的邀请码加好友
//	p2pcp contacts [rm 名字 | rename 旧名 新名]     好友列表
//	p2pcp send -to 好友 文件...                    发给好友
//	p2pcp serve [-o 目录]                          常驻接收所有好友发来的文件（按好友分目录），也替 invite 等对方来加好友
//	p2pcp app [-no-browser] [-o 目录]            桌面版：常驻接收 + 本机网页界面（app.go），已经在跑时只打开浏览器
//	p2pcp report [文件]                           诊断报告：现测一次网络 + 最近的连接记录、连接过程、日志、崩溃现场（diag.go），
//	                                             好友名和文件名换成代号，可以直接发微信
//
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
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// version 随会合标志（v=）、预测记录、上报、诊断报告一起报：服务器那边按版本统计，旧版的问题能区分开
const version = "0.6.0"

const (
	msgPunch = "/PP" // 探测（按前 3 字节识别；首字节最高两位必须是 0，quic-go 才会交给 ReadNonQUICPacket）
	msgAck   = "/PA" // 回应
	msgNom   = "/PN" // 提名：发送端告诉接收端"就用这条"（两端都有 IPv4 + IPv6 两个 socket 时才需要）
)

// serverConf：没给 -server 时依次看环境变量 P2PCP_SERVER、~/.config/p2pcp/server（apps/pc/install.sh 写的；
// Windows 是 %AppData%\p2pcp\server）、打包时写进去的 defaultServer。
// 可以写同一台服务器的几个端口，房间共享，每轮换一个；别用 7000 开头，它在某台电信 CGNAT 下有毒（docs/09 第 7 节）
func serverConf() string {
	if s := os.Getenv("P2PCP_SERVER"); s != "" {
		return s
	}
	if d, err := os.UserConfigDir(); err == nil { // Linux ~/.config，Windows %AppData%，macOS ~/Library/Application Support
		if b, err := os.ReadFile(filepath.Join(d, "p2pcp", "server")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return defaultServer
}

// defaultServer：打包时写进去的会合服务器（apps/win/build.sh 用 -ldflags -X main.defaultServer=…），
// 朋友的 Windows / macOS 版没有 install.sh 写的配置文件，邀请好友时用它
var defaultServer string

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
	to       string          // expose：转发目标
	listen   string          // forward：本地监听地址
	timeout  time.Duration   // 等对方的超时，0 = 一直等（daemon）
	regEvery time.Duration   // 注册间隔
	stun     string          // 体检用的公开 STUN，逗号分隔；off = 不体检
	pre      *preflight      // 这次 connect 的体检（connect 里填）
	fresh    bool            // 画像不用缓存、现测（check -room）
	preWait  time.Duration   // 第一次注册前最多等本机画像多久（0 = 50ms；check -room 要等它测完）
	roomSet  bool            // 命令行给了 -room（check 带 -room 时做双端体检）
	quit     <-chan struct{} // 关闭时 connectOnce 不再等对方（升级房间里的接收端，upgrade.go）
	// 通讯录模式（pair.go）
	config    string        // 身份和通讯录的目录（默认 ~/.config/p2pcp）
	friend    string        // send / check -to 的好友名
	secret    []byte        // 房间密钥：配对密钥或邀请密钥；nil 时由 -room 派生（roomKey）
	name      string        // id -name：改设备名；join -name：给新好友起的名字
	qr        string        // invite -qr：二维码写到这个 PNG（- = 画在终端里）
	ttl       time.Duration // invite：邀请码有效期
	serverSet bool          // 命令行给了 -server（send -to 时就不用好友记录里的会合地址）
	regIdle   time.Duration // 常驻接收空等时的注册间隔（-reg；手机后台拉长省电）
	rep       *reporter
	// 手动取消（cancel.go）
	x   *xfer  // 这次传输，取消时由它叫停；send 没给就自己建一个
	sid string // 发送号：一次 send 一个，重连续传时不变
}

func main() {
	if len(os.Args) < 2 && runtime.GOOS == "windows" {
		os.Args = append(os.Args, "launch") // 双击 P2P直传.exe：装好、打开网页（desktop_windows.go）
	}
	if len(os.Args) < 2 {
		usage()
	}
	role := os.Args[1]
	switch role {
	case "send", "recv", "daemon", "check", "expose", "forward", "serve", "id", "invite", "join", "contacts", "report", "app",
		"sendto", "launch", "uninstall", "window":
	default:
		usage()
	}
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.server, "server", "", "会合服务器 IP:端口，逗号分隔多个时每轮换一个（服务器那边房间共享，docs/09 第 8 节）；"+
		"不给就读环境变量 P2PCP_SERVER 或 ~/.config/p2pcp/server")
	fs.StringVar(&o.room, "room", "p2pcp", "房间名，两端要一致（当作共享密钥用：取一个别人猜不到的）")
	fs.StringVar(&o.bind, "bind", "", "绑定的本机 IP（开着 TUN 代理时用物理网卡地址）；auto = 每轮取 main 表默认路由那块网卡的地址")
	fs.StringVar(&o.public, "public", "", "本机自己的公网 IP，作为额外候选（1:1 NAT 的云服务器用：会合服务器在本机时看到的是 127.0.0.1）")
	fs.StringVar(&o.lan, "lan", "", "本机内网 IP 和全局 IPv6 列表（逗号分隔），代替自动探测（Android 11+ 的 App 里 Go 读不到网卡列表）")
	fs.BoolVar(&o.only4, "4", false, "只走 IPv4")
	fs.BoolVar(&o.only6, "6", false, "只走 IPv6（两端都要有全局 IPv6）")
	fs.IntVar(&o.conns, "conns", 2, "发送端：并行 QUIC 连接数（共用一个打通的洞；BBR 下 1~2 条就能跑满，docs/09 第 7 节）")
	fs.StringVar(&o.relay, "relay", "auto", "打不通时走会合服务器上的中继：auto（发送端两轮打不通才用）| off | always（测试用）")
	stunDef := defaultSTUN
	if s := os.Getenv("P2PCP_STUN"); s != "" {
		stunDef = s
	}
	fs.StringVar(&o.stun, "stun", stunDef, "体检用的公开 STUN（IP:端口，逗号分隔，至少两台、IP 不同）；off = 不体检。也认环境变量 P2PCP_STUN")
	fs.StringVar(&o.cc, "cc", "bbr", "发送端：拥塞控制 bbr | cubic | brutal:45M（见 cc.go）")
	fs.StringVar(&o.out, "o", ".", "接收端：保存目录")
	if role == "expose" {
		fs.StringVar(&o.to, "to", "127.0.0.1:22", "expose：对方开的每条流都接到这个 TCP 地址")
	} else {
		fs.StringVar(&o.friend, "to", "", "send / check：发给通讯录里的这个好友（不用 -room）")
	}
	fs.StringVar(&o.config, "config", "", "身份和通讯录的目录（默认 ~/.config/p2pcp；安卓 App 给自己的私有目录）")
	fs.StringVar(&o.name, "name", "", "id：改本机的设备名；join：给新好友起名字（默认用对方的设备名）")
	fs.StringVar(&o.qr, "qr", "", "invite：二维码写成这个 PNG；- = 画在终端里")
	fs.DurationVar(&o.ttl, "ttl", inviteTTL, "invite：邀请码有效期")
	fs.DurationVar(&o.regIdle, "reg", 5*time.Second, "serve / daemon / expose：空等时多久注册一次。会合服务器只配 30 秒内注册过的，"+
		"手机后台可以拉长到 20s 省电（对方来时最多晚一个间隔被配上）")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:2223", "forward：本地监听地址")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "等对方的超时（daemon 模式下无效）")
	noBrowser := fs.Bool("no-browser", false, "app：不打开浏览器（开机自启用）")
	upload := fs.String("upload", "", "诊断数据上报：on | off，记进配置目录的 settings.json，以后一直有效（默认开，见 upload.go）")
	progress := fs.Bool("progress", false, "在 stdout 输出机器可读的进度行")
	notify := fs.Bool("notify", false, "用 notify-send 弹桌面通知")
	fs.Parse(os.Args[2:])
	timeoutSet, outSet, notifySet := false, false, false
	fs.Visit(func(f *flag.Flag) {
		o.roomSet = o.roomSet || f.Name == "room"
		o.serverSet = o.serverSet || f.Name == "server"
		timeoutSet = timeoutSet || f.Name == "timeout"
		outSet = outSet || f.Name == "o"
		notifySet = notifySet || f.Name == "notify"
	})
	if role == "app" || role == "launch" {
		// 桌面版没人给参数（Windows 双击、开机自启、「发送到」拉起来的）：收到下载\P2P直传，Windows 上弹通知
		if !outSet {
			o.out = defaultOut()
		}
		if !notifySet && runtime.GOOS == "windows" {
			*notify = true
		}
	}
	if role == "join" && !timeoutSet {
		// 邀请方在线时 1 秒内就配上；邀请码用过 / 过期后邀请方不再听那个房间，别让朋友干等 1 分钟
		o.timeout = 20 * time.Second
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if o.server == "" {
		o.server = serverConf()
	}
	// 不碰网络的命令、会合地址另有来处的命令（join 听邀请码的，send -to 听好友记录的，serve 每个好友各有地址）不要求 -server
	needServer := !(role == "id" || role == "contacts" || role == "join" || role == "serve" || role == "report" || role == "app" ||
		role == "sendto" || role == "launch" || role == "uninstall" || role == "window" || o.friend != "")
	if o.server == "" && needServer {
		log.Fatal("没有会合服务器：用 -server IP:7001 指定，或者写进 ~/.config/p2pcp/server（也认环境变量 P2PCP_SERVER）。" +
			"在一台有公网 IP 的机器上跑 punch server -listen :7001 -relay :3478，放行 UDP 7001 和 3478")
	}
	o.rep = &reporter{machine: *progress, notify: *notify}
	o.regEvery = 500 * time.Millisecond
	if role != "id" && role != "contacts" && os.Getenv("P2PCP_NO_LOG") == "" {
		setupLog(role) // 日志落盘（diag.go），诊断报告要用
	}
	setupCrash(o.rep)
	exitGracefully()
	if *progress && (role == "send" || role == "serve" || role == "recv" || role == "daemon") {
		readCommands(role) // App 往 stdin 写 cancel（cancel.go）
	}
	switch *upload {
	case "":
	case "on", "off":
		cfg := configDir(o)
		st := loadSettings(cfg)
		on := *upload == "on"
		st.Upload = &on
		if err := saveSettings(cfg, st); err != nil {
			log.Fatal(err)
		}
		log.Printf("诊断数据上报：%s（记在 %s）", map[bool]string{true: "开", false: "关"}[on], settingsPath(cfg))
	default:
		log.Fatal("-upload 只能是 on 或 off")
	}

	var err error
	if o.friend != "" && (role == "send" || role == "check") {
		if err := resolveFriend(&o, role); err != nil {
			o.rep.fail(err)
			os.Exit(1)
		}
		o.roomSet = true // check -to 好友 = 和这个好友的双端体检
	}
	switch role {
	case "send":
		if fs.NArg() == 0 {
			log.Fatal("send 需要文件参数")
		}
		err = send(o, fs.Args())
	case "serve":
		err = serve(o)
	case "id":
		err = cmdID(o)
	case "invite":
		err = cmdInvite(o)
	case "join":
		if fs.NArg() != 1 {
			log.Fatal("用法：p2pcp join <邀请码>")
		}
		err = cmdJoin(o, fs.Arg(0))
	case "contacts":
		err = cmdContacts(o, fs.Args())
	case "report":
		err = cmdReport(o, fs.Args())
	case "app":
		err = cmdApp(o, *noBrowser)
	case "launch":
		err = cmdLaunch(o)
	case "uninstall":
		err = cmdUninstall(o)
	case "sendto":
		err = cmdSendto(o, fs.Args())
	case "window":
		if fs.NArg() != 1 {
			log.Fatal("用法：p2pcp window <地址>")
		}
		err = cmdWindow(fs.Arg(0))
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
// 5 秒注册一次就够了（家用路由器映射寿命 60 秒以上，docs/08）。对方一注册，服务器会立刻把 PEER 推过来。
// o.quit 关闭时退出（serve 删了好友、邀请用掉了）；waiting 为空时不报状态（serve 同时听很多房间，由它统一报）
func keepServing(o opts, waiting string, accept func(opts) (*link, error), serve func(*link)) {
	o.timeout, o.regEvery = 0, min(max(o.regIdle, time.Second), 25*time.Second)
	backoff := time.Duration(0)
	var active atomic.Int32 // 正在服务的对端数：有人在传时不报"等待"，免得盖掉界面上的进度
	pause := func(d time.Duration) {
		select {
		case <-o.quit:
		case <-time.After(d):
		}
	}
	for {
		select {
		case <-o.quit:
			return
		default:
		}
		if active.Load() == 0 && waiting != "" {
			o.rep.state(waiting)
		}
		l, err := accept(o)
		switch {
		case err == nil:
			backoff = 0
			active.Add(1)
			go func() {
				defer func() {
					// 一个连接里出的 panic 不该带走整个常驻接收：留下现场（diag.go），关掉这个连接，接着等下一个
					if r := recover(); r != nil {
						path := saveCrash(r, debug.Stack())
						l.close()
						o.rep.fail(fmt.Errorf("内部错误（%v），现场存在 %s，请生成诊断报告发给我", r, path))
					}
					if active.Add(-1) == 0 && waiting != "" {
						o.rep.state(waiting)
					}
				}()
				serve(l)
			}()
		case errors.Is(err, errStopped):
			return
		case errors.Is(err, errNoRendezvous):
			// 空等时连不上会合服务器（断网、换网络、服务器重启）：没人在用，不弹失败通知，只更新状态，
			// 重试间隔从 5 秒拉长到 1 分钟，网络回来后最多一分钟内自己恢复
			backoff = min(max(2*backoff, 5*time.Second), time.Minute)
			log.Print(err)
			if waiting != "" {
				o.rep.state(fmt.Sprintf("连不上会合服务器，%v 后重试", backoff))
			}
			pause(backoff)
		default:
			backoff = 0
			o.rep.fail(err)
			pause(time.Second)
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
		reraise(s)
	}()
}

func usage() {
	fmt.Println("usage: p2pcp send|recv|daemon|check|expose|forward [flags] [文件...]")
	fmt.Println("       p2pcp id | invite | join <邀请码> | contacts | send -to 好友 文件... | serve   通讯录模式")
	fmt.Println("       p2pcp check              本机这一侧的网络体检")
	fmt.Println("       p2pcp app                桌面版：常驻接收 + 本机网页界面")
	fmt.Println("       p2pcp sendto 文件...      交给正在跑的桌面版、在网页上选发给谁（右键「发送到」）")
	fmt.Println("       p2pcp report             生成诊断报告（-upload on|off 开关诊断数据上报）")
	fmt.Println("       p2pcp check -room 名字    两端各跑一次：双端体检（画像、预测、实际打洞、往返和丢包、中继）")
	os.Exit(2)
}
