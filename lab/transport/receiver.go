package transport

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"time"
)

type RecvStats struct {
	Packets   int // 收到的 DATA 包
	Dups      int // 重复分片（重传了但其实原包已到，或者多次重传）
	NacksSent int // 发出的 NACK 条目
	Feedbacks int // 发出的反馈包
	RTT       time.Duration
}

// FrameEvent 在一帧的所有分片收齐时回调。Latency = 收齐时刻 - 发送端的帧时间戳。
type FrameEvent struct {
	ID      uint32
	Key     bool
	Latency time.Duration
}

type rframe struct {
	got     []bool
	n       int
	done    bool
	firstAt time.Time
}

type gap struct {
	detected, lastNack time.Time
	nacks              int
}

// Receiver 收包、拼帧、发 NACK。单 goroutine 运行，不需要锁。
type Receiver struct {
	conn     *net.UDPConn
	deadline time.Duration
	OnFrame  func(FrameEvent)

	peer     *net.UDPAddr
	connID   uint32
	highest  uint32
	frames   map[uint32]*rframe
	prunedTo uint32 // 小于它的帧已经清理掉，迟到的包直接忽略
	missing  map[uint32]*gap
	lastTS   uint32 // 最近收到的包的 send_ts，反馈里回显给发送端测 RTT
	lastAt   time.Time
	fresh    bool // 上次反馈之后有没有收到新包
	stats    RecvStats
	buf      [256 + 4*MaxNacks]byte
}

func NewReceiver(conn *net.UDPConn, deadline time.Duration) *Receiver {
	return &Receiver{
		conn: conn, deadline: deadline,
		frames: map[uint32]*rframe{}, missing: map[uint32]*gap{},
		stats: RecvStats{RTT: 50 * time.Millisecond}, // 没有样本前的初始猜测
	}
}

const feedbackEvery = 10 * time.Millisecond

// Run 一直收到 BYE（返回发送端报告的总帧数）或空闲超过 idle。
func (r *Receiver) Run(idle time.Duration) (frames uint32, err error) {
	buf := make([]byte, 2048)
	nextFB := time.Now().Add(feedbackEvery)
	lastPkt := time.Now()
	for {
		r.conn.SetReadDeadline(nextFB)
		n, from, err := r.conn.ReadFromUDP(buf)
		now := time.Now()
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return 0, err
		}
		if err == nil {
			lastPkt = now
			if t, total := r.onPacket(buf[:n], from, now); t == TypeBye {
				return total, nil
			}
		}
		if now.Sub(lastPkt) > idle {
			return 0, errors.New("idle timeout")
		}
		if !now.Before(nextFB) {
			r.sendFeedback(now)
			nextFB = now.Add(feedbackEvery)
		}
	}
}

func (r *Receiver) onPacket(b []byte, from *net.UDPAddr, now time.Time) (typ uint8, byeFrames uint32) {
	h, err := parseHeader(b)
	if err != nil {
		return 0, 0
	}
	if r.peer == nil {
		r.peer, r.connID = from, h.ConnID
	}
	if h.ConnID != r.connID {
		return 0, 0
	}
	switch h.Type {
	case TypeBye:
		if len(b) >= HeaderLen+4 {
			return TypeBye, binary.BigEndian.Uint32(b[HeaderLen:])
		}
	case TypeData:
		r.peer = from // NAT 映射变了也能跟上
		r.onData(b, h, now)
	}
	return h.Type, 0
}

func (r *Receiver) onData(b []byte, h Header, now time.Time) {
	x, _, err := parseDataExt(b)
	if err != nil {
		return
	}
	r.stats.Packets++
	r.lastTS, r.lastAt, r.fresh = h.SendTS, now, true

	// 1. 序号空洞 → 登记缺失，立即发一次反馈
	newGap := false
	// 发送端的 seq 从 1 开始，所以 highest 初始为 0：开头就丢的包也会被发现、被 NACK
	switch {
	case h.Seq > r.highest:
		for s := r.highest + 1; s < h.Seq; s++ {
			r.missing[s] = &gap{detected: now}
			newGap = true
		}
		r.highest = h.Seq
	default:
		delete(r.missing, h.Seq) // 乱序晚到
	}
	// 2. 重传包带回被 NACK 的 seq：划掉空洞；只 NACK 过一次的，用它估计 RTT（多次 NACK 分不清对应哪一次）
	if h.Flags&FlagRtx != 0 {
		if g := r.missing[x.OrigSeq]; g != nil && g.nacks == 1 {
			r.stats.RTT += (now.Sub(g.lastNack) - r.stats.RTT) / 8
		}
		delete(r.missing, x.OrigSeq)
	}
	// 3. 拼帧
	if x.FrameID < r.prunedTo || x.FragCnt == 0 || x.FragIdx >= x.FragCnt {
		return
	}
	f := r.frames[x.FrameID]
	if f == nil {
		f = &rframe{got: make([]bool, x.FragCnt), firstAt: now}
		r.frames[x.FrameID] = f
	}
	if f.got[x.FragIdx] {
		r.stats.Dups++
	} else {
		f.got[x.FragIdx] = true
		f.n++
		if f.n == len(f.got) && !f.done {
			f.done = true
			if r.OnFrame != nil {
				r.OnFrame(FrameEvent{ID: x.FrameID, Key: h.Flags&FlagKey != 0, Latency: Since32(x.FrameTS)})
			}
		}
	}
	if newGap {
		r.sendFeedback(now)
	}
}

func (r *Receiver) sendFeedback(now time.Time) {
	if r.peer == nil {
		return
	}
	// 再次 NACK 的间隔：1.5 个 RTT，至少 10ms
	renack := max(r.stats.RTT*3/2, 10*time.Millisecond)
	var nacks []uint32
	for s, g := range r.missing {
		switch {
		case now.Sub(g.detected) > r.deadline: // 它所属的帧肯定已经过期了
			delete(r.missing, s)
		case g.nacks == 0 || now.Sub(g.lastNack) > renack:
			if len(nacks) < MaxNacks {
				nacks = append(nacks, s)
				g.lastNack = now
				g.nacks++
			}
		}
	}
	r.pruneFrames(now)
	if len(nacks) == 0 && !r.fresh {
		return
	}
	fb := Feedback{EchoTS: r.lastTS, DelayUS: uint32(now.Sub(r.lastAt).Microseconds()), Highest: r.highest, Nacks: nacks}
	n := encodeFeedback(r.buf[:], Header{ConnID: r.connID, Type: TypeFeedback, SendTS: Now32()}, fb)
	r.conn.WriteToUDP(r.buf[:n], r.peer)
	r.fresh = false
	r.stats.Feedbacks++
	r.stats.NacksSent += len(nacks)
}

// pruneFrames 清理很久以前的帧状态。
func (r *Receiver) pruneFrames(now time.Time) {
	for id, f := range r.frames {
		if now.Sub(f.firstAt) > 3*r.deadline {
			delete(r.frames, id)
			r.prunedTo = max(r.prunedTo, id+1)
		}
	}
}

func (r *Receiver) Stats() RecvStats { return r.stats }
