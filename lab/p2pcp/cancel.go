package main

// 手动取消（docs/arch/DESIGN.md 决策 33~36）。
//
// 「取消」和「中断」是两回事：中断（断网、换网、Ctrl-C、进程被杀）之后发送端会重连续传，半成品留着；
// 取消是用户点了按钮，两边都停、不重连，接收端删掉半成品。
//
// 怎么让对方知道：取消的一方用 QUIC 应用错误码 codeCanceled 关掉这个会话的所有连接（CONNECTION_CLOSE），
// 对方在任何一个流操作上都会拿到 Remote 的 ApplicationError，据此认出是「对方取消了」。
// CONNECTION_CLOSE 只发一次、可能丢：发送端丢了就等空闲超时、当成中断（接收端留着半成品，一周后清掉）；
// 接收端的丢了，发送端会当成中断重连，所以接收端记住被取消的发送号（offer 的 sid，每次 send 随机一个，
// 重连续传时不变），对方再拿同一个发送号来就回 {"t":"cancel"}。重新发同一个文件是新的发送号，照收。
//
// 谁来叫取消：桌面版网页（app.go，同一个进程）、手机 App（往 p2pcp 的 stdin 写一行 cancel，见 readCommands）。

import (
	"bufio"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apernet/quic-go"
)

const codeCanceled quic.ApplicationErrorCode = 0x43 // 'C'

// canceledErr：本机取消（peer = false）或对方取消（peer = true）。不是失败，界面上显示「已取消」
type canceledErr struct{ peer bool }

func (e canceledErr) Error() string {
	if e.peer {
		return "对方取消了"
	}
	return "已取消"
}

func isCanceled(err error) bool { return errors.As(err, new(canceledErr)) }

// peerCanceled：err 是不是对方用 codeCanceled 关的连接
func peerCanceled(err error) bool {
	var ae *quic.ApplicationError
	return errors.As(err, &ae) && ae.Remote && ae.ErrorCode == codeCanceled
}

// xfer 是一次能取消的传输：发送端一次 send（几个文件、重连几次都算一次），接收端一个好友的一次会话（含升级后的直连）
type xfer struct {
	id, dir, friend string

	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	links map[*link]bool
	sid   string // 接收端：对方这次发送的发送号，取消时记进 canceledSIDs
}

var (
	xfersMu sync.Mutex
	xfers   = map[*xfer]bool{}
	// earlyCancel：send 进程还没开始发（还在读文件、算指纹）就收到了 cancel。App 进程一起来就点取消时会这样：
	// 记下来，send 登记时马上取消。只对发送：serve 空闲时的「cancel recv」不能连累以后的接收
	earlyCancel bool
)

// newXfer 登记一次传输；结束时调 end
func newXfer(id, dir, friend string) *xfer {
	x := &xfer{id: id, dir: dir, friend: friend, done: make(chan struct{}), links: map[*link]bool{}}
	xfersMu.Lock()
	xfers[x] = true
	early := earlyCancel && dir == "send"
	xfersMu.Unlock()
	if early {
		x.cancel()
	}
	return x
}

func (x *xfer) end() {
	xfersMu.Lock()
	delete(xfers, x)
	xfersMu.Unlock()
}

// cancel 叫停：关掉手上所有链接（对方马上收到 codeCanceled）。可以重复调
func (x *xfer) cancel() {
	if x == nil {
		return
	}
	x.once.Do(func() {
		log.Printf("取消%s（%s）", verb(x.dir), x.friend)
		close(x.done)
		x.mu.Lock()
		ls := x.links
		x.links = map[*link]bool{}
		if x.sid != "" {
			rememberCanceled(x.sid)
		}
		x.mu.Unlock()
		for l := range ls {
			l.closeWith(codeCanceled, "canceled")
		}
	})
}

func (x *xfer) canceled() bool {
	if x == nil {
		return false
	}
	select {
	case <-x.done:
		return true
	default:
		return false
	}
}

// watch 把链接交给 x 管：取消时一起关。已经取消了就马上关。返回的函数在链接用完时调
func (x *xfer) watch(l *link) (unwatch func()) {
	if x == nil || l == nil {
		return func() {}
	}
	x.mu.Lock()
	if x.canceled() {
		x.mu.Unlock()
		l.closeWith(codeCanceled, "canceled")
		return func() {}
	}
	x.links[l] = true
	x.mu.Unlock()
	return func() {
		x.mu.Lock()
		delete(x.links, l)
		x.mu.Unlock()
	}
}

func (x *xfer) setSID(sid string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	x.sid = sid
	x.mu.Unlock()
}

// cancelXfers 取消符合条件的传输，返回取消了几个
func cancelXfers(match func(*xfer) bool) int {
	xfersMu.Lock()
	var hit []*xfer
	for x := range xfers {
		if match(x) {
			hit = append(hit, x)
		}
	}
	xfersMu.Unlock()
	for _, x := range hit {
		x.cancel()
	}
	return len(hit)
}

// 接收端取消过的发送号：对方没收到 CONNECTION_CLOSE、当成中断重连过来时，回它 cancel
var (
	canceledMu   sync.Mutex
	canceledSIDs = map[string]time.Time{}
)

const canceledTTL = 10 * time.Minute // 发送端连续 5 次重连没进展就放弃，最长也就几分钟

func rememberCanceled(sid string) {
	canceledMu.Lock()
	defer canceledMu.Unlock()
	for k, t := range canceledSIDs {
		if time.Since(t) > canceledTTL {
			delete(canceledSIDs, k)
		}
	}
	canceledSIDs[sid] = time.Now()
}

func wasCanceled(sid string) bool {
	if sid == "" {
		return false
	}
	canceledMu.Lock()
	defer canceledMu.Unlock()
	t, ok := canceledSIDs[sid]
	return ok && time.Since(t) <= canceledTTL
}

// readCommands：-progress（被 App 驱动）时从 stdin 读命令，一行一条：
//
//	cancel                 取消所有传输
//	cancel send|recv       只取消发送 / 接收
//	cancel recv <好友名>    只取消收这个好友的
//
// stdin 读到头（终端里 Ctrl-D、systemd 给的 /dev/null）就不再读，不影响别的
func readCommands(role string) {
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 || f[0] != "cancel" {
				continue
			}
			dir, friend := "", ""
			if len(f) > 1 {
				dir = f[1]
			}
			if len(f) > 2 {
				friend = strings.Join(f[2:], " ")
			}
			n := cancelXfers(func(x *xfer) bool {
				return (dir == "" || x.dir == dir) && (friend == "" || x.friend == friend)
			})
			if n == 0 && role == "send" && dir != "recv" {
				xfersMu.Lock()
				earlyCancel = true
				xfersMu.Unlock()
				log.Printf("还没开始发就取消了")
			} else if n == 0 {
				log.Printf("没有正在进行的传输可取消（%s）", sc.Text())
			}
		}
	}()
}
