package transport

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

type SenderStats struct {
	Packets     int           // 发出的 DATA 包（含重传）
	Bytes       int           // 发出的 DATA 字节（含包头）
	Retransmits int           // 重传的包
	NacksRecv   int           // 收到的 NACK 条目
	NackExpired int           // 因为帧已过截止时间而放弃的 NACK
	NackDup     int           // 因为刚重传过（一个 RTT 内）而忽略的 NACK
	SRTT        time.Duration // 平滑 RTT
}

// frag 是一个分片的发送记录。同一分片的首发和各次重传共享同一个 *frag。
type frag struct {
	ext     DataExt
	flags   uint16
	size    int
	frameAt time.Time
	lastRtx time.Time // 最近一次重传的时刻；首发不算
}

// Sender 把帧切片发出去，并根据接收端的反馈重传。conn 必须是已 Dial 到对端的 UDP 连接。
type Sender struct {
	conn     *net.UDPConn
	connID   uint32
	deadline time.Duration

	mu     sync.Mutex
	seq    uint32
	hist   map[uint32]*frag // seq → 分片
	oldest uint32           // hist 里最老的 seq，用来按顺序清理过期记录
	buf    [maxDatagram]byte
	stats  SenderStats
}

func NewSender(conn *net.UDPConn, connID uint32, deadline time.Duration) *Sender {
	return &Sender{conn: conn, connID: connID, deadline: deadline, hist: map[uint32]*frag{}, oldest: 1}
}

// SendFrame 把一帧切成不超过 MaxPayload 的分片，一次性全部发出（v1 没有 Pacer）。
func (s *Sender) SendFrame(id uint32, key bool, size int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cnt := max(1, (size+MaxPayload-1)/MaxPayload)
	var flags uint16
	if key {
		flags |= FlagKey
	}
	ts := uint32(at.UnixMicro())
	for i := range cnt {
		f := &frag{
			ext:     DataExt{FrameID: id, FragIdx: uint16(i), FragCnt: uint16(cnt), FrameTS: ts},
			flags:   flags,
			size:    min(MaxPayload, size-i*MaxPayload),
			frameAt: at,
		}
		s.sendLocked(f, 0)
	}
	s.pruneLocked()
}

// sendLocked 用一个新 seq 发出分片；origSeq 为 0 表示首发，否则是被 NACK 的那个 seq。
func (s *Sender) sendLocked(f *frag, origSeq uint32) {
	s.seq++
	h := Header{ConnID: s.connID, Type: TypeData, Channel: ChannelVideo, Seq: s.seq, SendTS: Now32(), Flags: f.flags}
	x := f.ext
	x.OrigSeq = s.seq
	if origSeq != 0 {
		h.Flags |= FlagRtx
		x.OrigSeq = origSeq
	}
	n := encodeData(s.buf[:], h, x, f.size)
	s.conn.Write(s.buf[:n])
	if origSeq != 0 {
		f.lastRtx = time.Now()
	}
	s.hist[s.seq] = f
	s.stats.Packets++
	s.stats.Bytes += n
}

// pruneLocked 按 seq 顺序删掉帧已过期很久的记录。重传会让少数旧帧的记录排在后面，晚一点删也无妨。
func (s *Sender) pruneLocked() {
	for s.oldest <= s.seq {
		f, ok := s.hist[s.oldest]
		if ok && time.Since(f.frameAt) < 2*s.deadline {
			return
		}
		delete(s.hist, s.oldest)
		s.oldest++
	}
}

// Run 读取反馈并处理重传，直到 conn 被关闭。
func (s *Sender) Run() {
	buf := make([]byte, 2048)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		h, err := parseHeader(buf[:n])
		if err != nil || h.ConnID != s.connID || h.Type != TypeFeedback {
			continue
		}
		fb, err := parseFeedback(buf[:n])
		if err != nil {
			continue
		}
		s.onFeedback(fb)
	}
}

func (s *Sender) onFeedback(fb Feedback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sample := Since32(fb.EchoTS) - time.Duration(fb.DelayUS)*time.Microsecond; sample > 0 {
		if s.stats.SRTT == 0 {
			s.stats.SRTT = sample
		} else {
			s.stats.SRTT += (sample - s.stats.SRTT) / 8
		}
	}
	// 一个分片在一个 RTT 内只重传一次：接收端的重复 NACK 可能是在重传到达之前发出的。
	// 只看重传时刻，不看首发：首个 NACK 离首发通常不到一个 RTT，按首发算会把它误判成重复
	guard := max(s.stats.SRTT, 5*time.Millisecond)
	for _, seq := range fb.Nacks {
		s.stats.NacksRecv++
		f, ok := s.hist[seq]
		switch {
		case !ok || time.Since(f.frameAt) > s.deadline:
			s.stats.NackExpired++
		case time.Since(f.lastRtx) < guard:
			s.stats.NackDup++
		default:
			s.sendLocked(f, seq)
			s.stats.Retransmits++
		}
	}
}

// Close 发 3 次 BYE（UDP 会丢），告诉接收端一共发了多少帧，然后关闭连接。
func (s *Sender) Close(frames uint32) {
	s.mu.Lock()
	var b [HeaderLen + 4]byte
	putHeader(b[:], Header{ConnID: s.connID, Type: TypeBye, SendTS: Now32()})
	binary.BigEndian.PutUint32(b[HeaderLen:], frames)
	s.mu.Unlock()
	for range 3 {
		s.conn.Write(b[:])
		time.Sleep(20 * time.Millisecond)
	}
	s.conn.Close()
}

func (s *Sender) Stats() SenderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
