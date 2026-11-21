package main

// preflight 是一次 connect 的体检：本机画像、对方经会合报来的画像、组合预测（docs/arch/designs/2026-11-21-preflight.md，图 26-flow-preflight）。
//
// 和会合并行：connect 一开始就在后台测本机画像（缓存命中时几乎立刻就有），不等它；画像出来之后的每次注册都带上 nat=。
// 对方的画像也可能来得晚（它的画像还没测完时注册的那几次不带 nat=），所以打洞期间服务器重推的 /P 也要看（rdv.go listen）。
// 两边都齐了就预测、打一行。
//
// 打洞常常比体检快（真实网络几十毫秒就通，没命中缓存的画像要 600ms）：connect 结束后就不再注册，对方后测好的画像
// 再也收不到。所以第一次注册前最多等 50ms（命中缓存时画像不到 1ms 就有，稳定使用时第一次注册就带上了），
// 传输会话结束时再 settle：最多等 1.5 秒让本机画像测完，对方的没到就如实说「没赶上」，不冒充成旧版。
// 打洞失败要重试的时候（第 4 步的重试策略要用预测），每 500ms 的重新注册会让服务器重推 /P，两边的画像都来得及。
// 多轮重试共用一个 preflight，预测只报一次。

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type preflight struct {
	role     string    // send / recv
	rep      *reporter // -progress 时输出 @CHECK
	mu       sync.Mutex
	me       *profile // nil = 还在测
	peer     profile
	havePeer bool // 收到过对方带 nat= 的标志
	verdict  verdict
	reported bool
	ready    chan struct{} // 本机画像测完时关闭
	actual   string        // 实际结果：直连 / 中继；空 = 还没连上
	checked  bool          // 预测和实际已经对过
	settled  bool          // 结果已经报过、记过
	forced   bool          // 有一端用了 -relay always：走中继是强制的，不拿预测去比
	tr       trace         // 这次连接的过程记录（trace.go），settle 时落盘
	cfg      string        // 配置目录（上报开关、设备哈希）
	servers  string        // 会合地址（上报发给它）
	conn     string        // 连接号（connHash）
	peerVer  string        // 对方会合标志里的 v=
}

// setConn 记下连接号（拿到对方 id 时）
func (p *preflight) setConn(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn = id
}

// setForced：强制走中继（本端 -relay always，或对方的标志里有 forced）
func (p *preflight) setForced() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forced = true
}

// snapshot 给 check -room 的报告用：本机画像、对方画像（没收到时 ok2 为 false）、预测
func (p *preflight) snapshot() (me *profile, peer profile, havePeer bool, v verdict) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.me, p.peer, p.havePeer, p.verdict
}

// predictedNo：已经预测出「打不通」（connect 的重试策略用：一轮不通就请求中继）
func (p *preflight) predictedNo() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reported && p.verdict.Result == predNo
}

// setActual 记下实际结果，和预测对一下：对不上的要说出来（第 5 步写进预测记录，攒数据修矩阵）
func (p *preflight) setActual(relayed bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actual = "直连"
	if relayed {
		p.actual = "中继"
	}
	p.compare()
}

// compare 预测和实际都有了就对一次（调用方持有锁）
func (p *preflight) compare() {
	if p.checked || p.actual == "" || !p.reported || p.forced {
		return
	}
	p.checked = true
	switch {
	case p.verdict.Result == predNo && p.actual == "直连":
		log.Printf("体检  预测打不通，实际直连了：预测错了（%s）", p.verdict.Why)
	case p.verdict.Result == predYes && p.actual == "中继":
		log.Printf("体检  预测能直连，实际走了中继：预测错了（%s）", p.verdict.Why)
	}
}

// startPreflight 在后台测本机画像（先查缓存），立刻返回
func startPreflight(o opts, role string) *preflight {
	p := &preflight{role: role, rep: o.rep, ready: make(chan struct{}), cfg: configDir(o), servers: o.server}
	go func() { p.setMe(cachedProfile(o)) }() // 别写成 go p.setMe(cachedProfile(o))：参数在当前 goroutine 里求值，就不并行了
	return p
}

func (p *preflight) setMe(me profile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.me = &me
	if p.ready != nil {
		close(p.ready)
	}
	log.Printf("体检  本机 %s", me)
	p.tryReport()
}

// wait 等本机画像测完，最多 d；p 为 nil 或已测完时立刻返回
func (p *preflight) wait(d time.Duration) {
	if p == nil || p.ready == nil {
		return
	}
	select {
	case <-p.ready:
	case <-time.After(d):
	}
}

// flag 是要随 /R2 发的 nat= 标志；画像还没好时为空
func (p *preflight) flag() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.me == nil {
		return ""
	}
	return p.me.flag()
}

// sawPeer 收到对方的会合标志（/P 的最后一段）。不带 nat= 的先不管：可能只是对方的画像还没测完
func (p *preflight) sawPeer(flags string) {
	if p == nil {
		return
	}
	if slices.Contains(strings.Split(flags, ","), "forced") {
		p.setForced()
	}
	for _, f := range strings.Split(flags, ",") {
		if v, ok := strings.CutPrefix(f, "v="); ok && len(v) <= 16 {
			p.mu.Lock()
			p.peerVer = v
			p.mu.Unlock()
		}
	}
	pp := parseNat(flags)
	if pp.Class == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.havePeer {
		p.peer, p.havePeer = pp, true
		p.tryReport()
	}
}

// tryReport 两边画像都齐了就预测（调用方持有锁）
func (p *preflight) tryReport() {
	if p.reported || p.me == nil || !p.havePeer {
		return
	}
	p.reported = true
	p.verdict = predict(*p.me, p.peer)
	log.Printf("体检  对方 %s → 预测%s", p.peer, p.verdict)
	p.compare()
}

// settle 在传输会话结束（或打洞彻底失败）时调用：等本机画像测完（最多 1.5 秒），还没预测过就把缺的那一半说清楚，
// 然后打结果行、写预测记录、输出 @CHECK（checkreport.go）。只做一次
func (p *preflight) settle(out outcome) {
	if p == nil {
		return
	}
	p.wait(1500 * time.Millisecond)
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.settled = true
	if !p.reported && p.me != nil {
		p.reported = true
		p.verdict = verdict{predUnknown, "对方的画像没赶上（打洞比体检快，或对方是旧版 p2pcp）"}
		log.Printf("体检  %s", p.verdict)
	}
	rec := histRec{
		At: time.Now(), Role: p.role, Me: histOf(p.me), Predict: p.verdict.Result, Why: p.verdict.Why,
		Actual: out.actual, Via: out.via, Err: out.err, Rounds: out.rounds,
		TookMs: out.took.Milliseconds(), PunchMs: out.punch.Milliseconds(), HandshakeMs: out.handshake.Milliseconds(),
		UpgradedAfterMs: out.upgraded.Milliseconds(), UpgradedVia: out.upgradedVia,
	}
	if p.havePeer {
		rec.Peer = histOf(&p.peer)
	}
	rec.Forced = p.forced
	rec.Wrong = !p.forced && wrongPrediction(rec.Predict, rec.Actual)
	rec.Reason, rec.Ver, rec.OS = out.reason, version, runtime.GOOS+"/"+runtime.GOARCH
	rec.PeerVer, rec.Conn = p.peerVer, p.conn
	line := resultLine(rec)
	log.Printf("体检  %s", line)
	appendHistory(rec)
	p.rep.check(rec)
	b, _ := json.Marshal(rec)
	writeTrace(p.role, p.tr.render(fmt.Sprintf("p2pcp %s %s，角色 %s", version, rec.OS, p.role), line+"\n"+string(b)))
	p.mu.Unlock()
	upload(p.cfg, p.servers, rec) // 诊断数据上报（upload.go），发不出去就进队列；有网络往返，别拿着锁
}
