package main

// 传文件走中继时，后台升级到直连（docs/arch/DESIGN.md 决策 3、26，图 26 的 upgrade）。
//
// 发送端（upgrader）：中继会话开始 15 秒后，开新 socket 在「升级房间」里和接收端打洞，打不通就把间隔翻倍（最长 2 分钟）
// 再试。打通了就在新洞上 QUIC Dial、认证，然后关掉中继会话：正在发的块报错，sendSession 带着这条直连返回，
// send 马上在它上面重开会话。对方的 have 只报缺的块（断点续传），所以就是接着传，断在半路的块写下的部分也算数。
//
// 接收端（waitUpgrade）：中继会话期间另开一个 socket 挂在升级房间里等（像 daemon 一样 5 秒注册一次），对方来了就打洞，
// 在新洞上 Listen、接受认证过的连接，交给 recvSession：中继会话断开后在这条直连上接着收。
// 不挂在中继连接的 socket 上（tunnel.go helpMigrate 的做法）：那样新连接会落到原来的 Listener，被当成同一会话的额外
// 数据连接；另开 socket 就是一次普通的重开会话，还能走 IPv6。
//
// 升级房间号由中继连接的 EKM 派生（connSecret），只有这两端知道；daemon 同时服务几个发送端也互不干扰。
//
// 两个时候不切：发送端发出 end 到收到 ok 之间（这时切，对方可能已经校验完、改名成正式文件，ok 却没送到，
// 重发就会多出一份「a (1)」），以及最后一个文件发出 end 之后（马上就传完了）。

import (
	"errors"
	"log"
	"sync"
	"time"
)

var (
	upgradeFirst = 15 * time.Second // 中继会话开始多久后第一次试
	upgradeMax   = 2 * time.Minute  // 失败翻倍，最长隔这么久（传文件的时间短，比隧道的 10 分钟密）
)

// upgradeOpts：升级用的 connectOnce 参数：不体检、不走中继
func upgradeOpts(o opts) opts {
	o.stun, o.pre, o.preWait, o.relay = "off", nil, 0, "off"
	return o
}

// upgrader 是发送端一个中继会话的升级状态。nil 表示不升级（会话本来就是直连），方法都可以在 nil 上调
type upgrader struct {
	start time.Time
	mu    sync.Mutex // 发送端在 end → ok 之间拿着它，这期间不切
	final bool       // 最后一个文件已经发出 end
	quit  bool       // 会话结束了
	next  *link      // 打通的直连，等 sendSession 接手
	after time.Duration
}

// startUpgrade 中继会话开始时调用：后台定时去打洞
func startUpgrade(o opts, l *link) *upgrader {
	if !l.relayed {
		return nil
	}
	room := connSecret(l.conn, "upgrade")
	if room == "" {
		return nil
	}
	u := &upgrader{start: time.Now()}
	go u.run(o, l, room)
	return u
}

func (u *upgrader) run(o opts, l *link, room string) {
	wait := upgradeFirst
	for {
		time.Sleep(wait)
		if u.stopped() {
			return
		}
		nl, err := dialUpgrade(o, l, room)
		if err != nil {
			wait = min(2*wait, upgradeMax)
			if !u.stopped() {
				log.Printf("升级到直连没成（%v），%v 后再试", err, wait)
			}
			continue
		}
		if u.switchTo(nl, l) {
			log.Printf("升级到直连 %s（走中继 %v 后；打洞 %v，QUIC 握手 + 认证 %v），关掉中继会话，在直连上接着传",
				nl.via(), u.after.Round(time.Millisecond), nl.punch.Round(time.Millisecond), nl.handshake.Round(time.Millisecond))
		}
		return
	}
}

// switchTo 打通之后切过去：会话还在、不在 end → ok 之间、不是最后一个文件收尾时，记下直连、关掉中继会话；否则扔掉直连
func (u *upgrader) switchTo(nl, relay *link) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.quit || u.final {
		nl.close()
		return false
	}
	u.next, u.after = nl, time.Since(u.start)
	relay.close() // 在锁里关：放开锁之后 sendFile 可能马上进入 end → ok
	return true
}

func (u *upgrader) stopped() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.quit || u.final
}

// enter / leave 包住 end → ok；leave(true) 表示这是最后一个文件，之后不再切
func (u *upgrader) enter() {
	if u != nil {
		u.mu.Lock()
	}
}

func (u *upgrader) leave(last bool) {
	if u != nil {
		u.final = u.final || last
		u.mu.Unlock()
	}
}

// stop 会话结束时调用，返回切过去的直连（没切就是 nil）
func (u *upgrader) stop() *link {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.quit = true
	return u.next
}

// annotate 给预测记录补上「走中继多久后升级到了哪」（actual 仍是中继：升级不算预测错）
func (u *upgrader) annotate(out *outcome) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.next != nil {
		out.upgraded, out.upgradedVia = u.after, u.next.peer.String()
	}
}

// dialUpgrade（发送端）在升级房间里打一次洞，打通就 QUIC Dial、认证
func dialUpgrade(o opts, relay *link, room string) (*link, error) {
	uo := upgradeOpts(o)
	uo.timeout, uo.regEvery = 5*time.Second, 500*time.Millisecond
	s, err := connectOnce(uo, relay.server.String(), room, "send."+randHex(4), false)
	if err != nil {
		return nil, err
	}
	s.punch, s.rounds = s.took, 1
	nl := newLink(s, relay.secret, relay.alpn)
	t0 := time.Now()
	if nl.conn, err = nl.dial(); err != nil {
		nl.close()
		return nil, err
	}
	nl.handshake = time.Since(t0)
	return nl, nil
}

// waitUpgrade（接收端）：中继会话期间挂在升级房间里等对方来打洞，接上认证过的连接后从返回的 channel 交出去（最多一条）。
// channel 不带缓冲：recvSession 只在中继会话断开后才来取；stop 关闭后还没交出去的直连就关掉
func waitUpgrade(o opts, relay *link, stop <-chan struct{}) <-chan *link {
	out := make(chan *link)
	room := connSecret(relay.conn, "upgrade")
	if !relay.relayed || room == "" {
		return out
	}
	go func() {
		tc, err := selfSigned(relay.alpn) // 先生成好证书：手机上要一两百毫秒，别让对方的握手包干等（决策 24）
		if err != nil {
			return
		}
		uo := upgradeOpts(o)
		uo.timeout, uo.regEvery, uo.quit = 0, 5*time.Second, stop
		for {
			s, err := connectOnce(uo, relay.server.String(), room, "recv."+randHex(4), false)
			if errors.Is(err, errStopped) {
				return
			}
			if err != nil {
				select {
				case <-stop:
					return
				default:
				}
				log.Printf("等对方升级到直连：%v", err)
				if !errors.Is(err, errPunchTimeout) { // 断网之类：别空转
					select {
					case <-stop:
						return
					case <-time.After(2 * time.Second):
					}
				}
				continue
			}
			s.punch, s.rounds = s.took, 1
			nl := newLink(s, relay.secret, relay.alpn)
			if nl.ln, err = nl.tr.Listen(tc, quicConf(relay.alpn)); err != nil {
				nl.close()
				continue
			}
			ln := nl.ln
			nl.onClose(func() { ln.Close() })
			t0 := time.Now()
			if nl.conn, err = nl.accept(5 * time.Second); err != nil {
				log.Printf("等对方升级到直连：%v", err)
				nl.close()
				continue
			}
			nl.handshake = time.Since(t0)
			select {
			case out <- nl:
			case <-stop:
				nl.close()
			}
			return
		}
	}()
	return out
}
