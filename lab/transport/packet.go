// Package transport 是自研的远控传输层 v1（M4）：分片 + NACK 重传 + 截止时间丢弃，明文。
// FEC、Pacer、带宽估计、加密留给 M5（见 docs/05 第 3 节）。
//
// 设计要点：
//   - 每个包有一个全局递增的传输序号 seq；重传的包拿新 seq，并在 OrigSeq 里带上被 NACK 的那个 seq。
//     这样每个 seq 只发一次，以后做基于延迟的带宽估计时，每个包的发送/到达时间都不会有歧义。
//   - 接收端按 seq 的空洞发 NACK；重传包带回 OrigSeq，接收端据此划掉对应的空洞，
//     并用"发出 NACK → 收到重传"的间隔估计 RTT，决定多久后再 NACK 一次。
//   - 帧有截止时间：发送端不再重传过期帧的分片，接收端也不再为过期的空洞发 NACK。
//
// 包格式（大端）：
//
//	公共头 16 字节: conn_id u32 | type u8 | channel u8 | seq u32 | send_ts u32 | flags u16
//	DATA 扩展 16 字节: frame_id u32 | frag_idx u16 | frag_cnt u16 | frame_ts u32 | orig_seq u32
//	FEEDBACK: echo_ts u32 | delay_us u32 | highest u32 | n u16 | n × nack_seq u32
//	BYE:      frames u32（一共发了多少帧）
//
// 时间戳都是 Unix 微秒的低 32 位（约 71 分钟回绕一次），只用来求差。
package transport

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	TypeData     = 1
	TypeFeedback = 2
	TypeBye      = 3

	ChannelVideo = 1

	FlagKey = 1 << 0 // 所属帧是关键帧
	FlagRtx = 1 << 1 // 这是一次重传

	HeaderLen   = 16
	DataExtLen  = 16
	MaxPayload  = 1200 // 留足余量，避免经过 PPPoE/隧道后 IP 分片
	MaxNacks    = 200
	maxDatagram = HeaderLen + DataExtLen + MaxPayload
)

// Now32 返回当前 Unix 微秒的低 32 位。两个时间戳相减（按 uint32 回绕）得到间隔。
func Now32() uint32 { return uint32(time.Now().UnixMicro()) }

// Since32 返回从时间戳 ts 到现在的时长。
func Since32(ts uint32) time.Duration { return time.Duration(Now32()-ts) * time.Microsecond }

type Header struct {
	ConnID  uint32
	Type    uint8
	Channel uint8
	Seq     uint32
	SendTS  uint32
	Flags   uint16
}

type DataExt struct {
	FrameID uint32
	FragIdx uint16
	FragCnt uint16
	FrameTS uint32
	OrigSeq uint32
}

type Feedback struct {
	EchoTS  uint32 // 最近收到的那个包的 send_ts
	DelayUS uint32 // 从收到那个包到发出本反馈之间隔了多久
	Highest uint32 // 收到过的最大 seq
	Nacks   []uint32
}

func putHeader(b []byte, h Header) {
	binary.BigEndian.PutUint32(b[0:], h.ConnID)
	b[4] = h.Type
	b[5] = h.Channel
	binary.BigEndian.PutUint32(b[6:], h.Seq)
	binary.BigEndian.PutUint32(b[10:], h.SendTS)
	binary.BigEndian.PutUint16(b[14:], h.Flags)
}

func parseHeader(b []byte) (Header, error) {
	if len(b) < HeaderLen {
		return Header{}, errors.New("short packet")
	}
	return Header{
		ConnID:  binary.BigEndian.Uint32(b[0:]),
		Type:    b[4],
		Channel: b[5],
		Seq:     binary.BigEndian.Uint32(b[6:]),
		SendTS:  binary.BigEndian.Uint32(b[10:]),
		Flags:   binary.BigEndian.Uint16(b[14:]),
	}, nil
}

// encodeData 把 DATA 包写进 buf，返回总长度。负载内容无关紧要（假编码器），保持为零。
func encodeData(buf []byte, h Header, x DataExt, payloadLen int) int {
	putHeader(buf, h)
	d := buf[HeaderLen:]
	binary.BigEndian.PutUint32(d[0:], x.FrameID)
	binary.BigEndian.PutUint16(d[4:], x.FragIdx)
	binary.BigEndian.PutUint16(d[6:], x.FragCnt)
	binary.BigEndian.PutUint32(d[8:], x.FrameTS)
	binary.BigEndian.PutUint32(d[12:], x.OrigSeq)
	n := HeaderLen + DataExtLen + payloadLen
	clear(buf[HeaderLen+DataExtLen : n])
	return n
}

func parseDataExt(b []byte) (DataExt, int, error) {
	if len(b) < HeaderLen+DataExtLen {
		return DataExt{}, 0, errors.New("short data packet")
	}
	d := b[HeaderLen:]
	return DataExt{
		FrameID: binary.BigEndian.Uint32(d[0:]),
		FragIdx: binary.BigEndian.Uint16(d[4:]),
		FragCnt: binary.BigEndian.Uint16(d[6:]),
		FrameTS: binary.BigEndian.Uint32(d[8:]),
		OrigSeq: binary.BigEndian.Uint32(d[12:]),
	}, len(b) - HeaderLen - DataExtLen, nil
}

func encodeFeedback(buf []byte, h Header, f Feedback) int {
	putHeader(buf, h)
	d := buf[HeaderLen:]
	binary.BigEndian.PutUint32(d[0:], f.EchoTS)
	binary.BigEndian.PutUint32(d[4:], f.DelayUS)
	binary.BigEndian.PutUint32(d[8:], f.Highest)
	n := min(len(f.Nacks), MaxNacks)
	binary.BigEndian.PutUint16(d[12:], uint16(n))
	for i, s := range f.Nacks[:n] {
		binary.BigEndian.PutUint32(d[14+4*i:], s)
	}
	return HeaderLen + 14 + 4*n
}

func parseFeedback(b []byte) (Feedback, error) {
	if len(b) < HeaderLen+14 {
		return Feedback{}, errors.New("short feedback")
	}
	d := b[HeaderLen:]
	f := Feedback{
		EchoTS:  binary.BigEndian.Uint32(d[0:]),
		DelayUS: binary.BigEndian.Uint32(d[4:]),
		Highest: binary.BigEndian.Uint32(d[8:]),
	}
	n := int(binary.BigEndian.Uint16(d[12:]))
	if len(d) < 14+4*n {
		return Feedback{}, errors.New("truncated nack list")
	}
	for i := range n {
		f.Nacks = append(f.Nacks, binary.BigEndian.Uint32(d[14+4*i:]))
	}
	return f, nil
}
