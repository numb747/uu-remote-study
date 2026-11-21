package main

// 体检的结论：结果行、预测记录、@CHECK（docs/arch/designs/2026-11-21-preflight.md 实现顺序第 5 步，图 26 的 report / history）。
//
// 传输会话结束时（或者打洞彻底失败时）由 preflight.settle 写：
//   - 日志里一行「体检  结果：…」：实际走的路、建连耗时拆成会合 + 打洞和 QUIC 握手 + 认证两段、预测对没对上
//   - ~/.cache/p2pcp/history.jsonl 追加一行：两端画像、预测、实际，攒真实网络数据，用来修正矩阵，也是文章素材
//   - -progress 时 stdout 一行 @CHECK <同一个 JSON>（手机 App 现在不认识它，会忽略）
//
// 打洞和握手分开计时：2026-11-21 实测有一轮打洞 118ms、握手 4.4s，原来的「用时」看不出来（docs/13 第 5 节）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// outcome 是一次连接的实际结果
type outcome struct {
	actual    string // 直连 / 中继 / 失败
	via       string // 对方地址或中继地址
	err       string
	took      time.Duration // 从开始会合到打通（含重试）
	punch     time.Duration // 成功那一轮的打洞（或进中继）用时
	handshake time.Duration // QUIC 握手 + 认证
	rounds    int
	// 走中继期间升级到了直连（upgrade.go）：走了多久中继、升级后的对方地址
	upgraded    time.Duration
	upgradedVia string
}

func (l *link) outcome() outcome {
	actual := "直连"
	if l.relayed {
		actual = "中继"
	}
	return outcome{actual: actual, via: l.peer.String(), took: l.took, punch: l.punch, handshake: l.handshake, rounds: l.rounds}
}

type histProfile struct {
	Class    string `json:"class"` // 空 = 未知
	V6       bool   `json:"v6,omitempty"`
	Filter   string `json:"filter,omitempty"`
	Mapped   string `json:"mapped,omitempty"`
	PortKept bool   `json:"port_kept,omitempty"`
	Cached   bool   `json:"cached,omitempty"`
}

func histOf(p *profile) histProfile {
	if p == nil {
		return histProfile{}
	}
	h := histProfile{Class: p.Class, V6: p.V6, Filter: p.Filter, PortKept: p.PortKept, Cached: !p.CachedAt.IsZero()}
	for _, m := range p.Mapped {
		if m != nil {
			h.Mapped = m.String()
			break
		}
	}
	return h
}

// histRec 是 history.jsonl 的一行，也是 @CHECK 的内容
type histRec struct {
	At          time.Time   `json:"at"`
	Role        string      `json:"role"`
	Me          histProfile `json:"me"`
	Peer        histProfile `json:"peer"`
	Predict     string      `json:"predict"`
	Why         string      `json:"why"`
	Actual      string      `json:"actual"`
	Wrong       bool        `json:"wrong,omitempty"`  // 预测「能直连」却走了中继 / 失败，或预测「打不通」却直连了
	Forced      bool        `json:"forced,omitempty"` // 有一端 -relay always：走中继是强制的，不算预测错
	Via         string      `json:"via,omitempty"`
	Err         string      `json:"err,omitempty"`
	Rounds      int         `json:"rounds,omitempty"`
	TookMs      int64       `json:"took_ms"`
	PunchMs     int64       `json:"punch_ms,omitempty"`
	HandshakeMs int64       `json:"handshake_ms,omitempty"`
	// actual 仍记中继（开局时的实际结果），升级不算预测错
	UpgradedAfterMs int64  `json:"upgraded_after_ms,omitempty"`
	UpgradedVia     string `json:"upgraded_via,omitempty"`
}

// wrongPrediction 预测和实际对不上（「不确定」「无法预测」不算错）
func wrongPrediction(pred, actual string) bool {
	switch pred {
	case predYes:
		return actual != "直连"
	case predNo:
		return actual == "直连"
	}
	return false
}

// resultLine 是给人看的结果行
func resultLine(r histRec) string {
	s := fmt.Sprintf("结果：%s", r.Actual)
	if r.Via != "" && r.Actual != "失败" {
		s += " " + r.Via
	}
	if r.Actual != "失败" {
		// 「从开始会合」含等对方上线（先启动的接收端、daemon 会等很久），打洞和握手单独列
		ms := func(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
		what := "打洞"
		if r.Actual == "中继" {
			what = "进中继"
		}
		s += fmt.Sprintf("，%s %v（第 %d 轮），QUIC 握手 + 认证 %v，从开始会合共 %v",
			what, ms(r.PunchMs), r.Rounds, ms(r.HandshakeMs), ms(r.TookMs+r.HandshakeMs))
		if r.UpgradedVia != "" {
			s += fmt.Sprintf("，走中继 %v 后升级到直连 %s", ms(r.UpgradedAfterMs).Round(100*time.Millisecond), r.UpgradedVia)
		}
	} else if r.Err != "" {
		s += "（" + r.Err + "）"
	}
	switch {
	case r.Predict == "":
	case r.Forced:
		s += fmt.Sprintf("；预测%s（强制走中继，不比对）", r.Predict)
	case r.Wrong:
		s += fmt.Sprintf("；预测%s，预测错了", r.Predict)
	case r.Predict == predYes || r.Predict == predNo:
		s += fmt.Sprintf("；预测%s，对上了", r.Predict)
	default:
		s += "；预测" + r.Predict
	}
	return s
}

func historyPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "p2pcp", "history.jsonl")
}

// appendHistory 追加一行（O_APPEND，一行一次 write，几个进程同时写也不会交错）
func appendHistory(r histRec) {
	path := historyPath()
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// check 输出 @CHECK 机器行
func (r *reporter) check(rec histRec) {
	if r == nil || !r.machine {
		return
	}
	b, _ := json.Marshal(rec)
	r.line("@CHECK %s", b)
}
