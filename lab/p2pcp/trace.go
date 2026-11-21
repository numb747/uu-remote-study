package main

// 连接过程记录（docs/arch/DESIGN.md 决策 24）：每次连接从开始会合到结束，一步步发生了什么——每一轮的会合地址、
// 本机 socket 和候选、会合服务器回执里的映射、对方的候选和标志、收到了哪些打洞包（来源、类型、个数）、打通 / 超时 /
// 改走中继、QUIC 握手，最后是结果记录。打不通、偶发失败时靠它看清是哪一步、哪个地址出的问题。
//
// 挂在 preflight 上（一次 connect 一份，多轮重试共用），settle 时写进 ~/.cache/p2pcp/trace/，每次连接一个文件，
// 保留最近 50 个、总共不超过 5MB。太细，不上报，只进诊断报告（diag.go）。-stun off（测试用）时没有 preflight，也不记。

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	traceKeep     = 50
	traceMaxBytes = 5 << 20
	traceMaxLines = 2000 // 一次连接最多记这么多行（常驻接收空等很久时也不会无限长）
)

type trace struct {
	mu      sync.Mutex
	t0      time.Time
	lines   []string
	dropped int
	pkts    map[string]*pktStat // 「类型 来源」→ 统计
	order   []string
}

type pktStat struct {
	first, last time.Duration
	n           int
}

func (t *trace) since() time.Duration {
	if t.t0.IsZero() {
		t.t0 = time.Now()
	}
	return time.Since(t.t0)
}

func (t *trace) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.since()
	if len(t.lines) >= traceMaxLines {
		t.dropped++
		return
	}
	t.lines = append(t.lines, fmt.Sprintf("%+8dms  %s", d.Milliseconds(), s))
}

// pkt 记一个收到的包。打洞包 20ms 一个，逐个记太多：同一来源、同一类型只在第一次记一行，其余计数
func (t *trace) pkt(kind string, from *net.UDPAddr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.since()
	k := kind + " 来自 " + from.String()
	if t.pkts == nil {
		t.pkts = map[string]*pktStat{}
	}
	if st := t.pkts[k]; st != nil {
		st.n++
		st.last = d
		return
	}
	t.pkts[k] = &pktStat{first: d, last: d, n: 1}
	t.order = append(t.order, k)
	if len(t.lines) < traceMaxLines {
		t.lines = append(t.lines, fmt.Sprintf("%+8dms  收到 %s（第一个）", d.Milliseconds(), k))
	}
}

func (t *trace) render(head, tail string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	b.WriteString(head + "\n")
	if !t.t0.IsZero() {
		fmt.Fprintf(&b, "开始 %s\n\n", t.t0.Format("2006-01-02 15:04:05.000"))
	}
	for _, l := range t.lines {
		b.WriteString(l + "\n")
	}
	if t.dropped > 0 {
		fmt.Fprintf(&b, "（后面还有 %d 行没记）\n", t.dropped)
	}
	if len(t.order) > 0 {
		b.WriteString("\n收包统计：\n")
		for _, k := range t.order {
			st := t.pkts[k]
			fmt.Fprintf(&b, "  %-44s %4d 个  %dms ~ %dms\n", k, st.n, st.first.Milliseconds(), st.last.Milliseconds())
		}
	}
	b.WriteString("\n" + tail + "\n")
	return b.String()
}

// tracef / tracePkt：preflight 为 nil（-stun off）时什么也不做
func (p *preflight) tracef(format string, a ...any) {
	if p != nil {
		p.tr.add(fmt.Sprintf(format, a...))
	}
}

func (p *preflight) tracePkt(kind string, from *net.UDPAddr) {
	if p != nil {
		p.tr.pkt(kind, from)
	}
}

func traceDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "p2pcp", "trace")
}

// writeTrace 写一个文件，再删掉多出来的旧文件
func writeTrace(role, text string) {
	dir := traceDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	name := time.Now().Format("20060102-150405.000") + "-" + role + "-" + randHex(2) + ".txt"
	if os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600) != nil {
		return
	}
	pruneDir(dir, ".txt", traceKeep, traceMaxBytes, "")
}

// pruneDir 只看名字以 prefix 开头、suffix 结尾的文件，按文件名（时间在名字里）从旧到新删，直到不超过 keep 个、总共不超过 maxBytes
func pruneDir(dir, suffix string, keep int, maxBytes int64, prefix string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		size int64
	}
	var fs []f
	var total int64
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), suffix) || !strings.HasPrefix(e.Name(), prefix) || e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			fs = append(fs, f{e.Name(), info.Size()})
			total += info.Size()
		}
	}
	slices.SortFunc(fs, func(a, b f) int { return strings.Compare(a.name, b.name) })
	for len(fs) > 0 && (len(fs) > keep || total > maxBytes) {
		os.Remove(filepath.Join(dir, fs[0].name))
		total -= fs[0].size
		fs = fs[1:]
	}
}
