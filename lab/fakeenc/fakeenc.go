// Package fakeenc 是一个"假编码器"：按固定帧率产出帧大小序列，模拟硬件编码器的码率形态，
// 让传输层可以脱离真实编码器单独调试（M4/M5），到 M6 再换成 ffmpeg。
//
// 形态：每 KeyEvery 帧一个关键帧，大小是普通帧的 KeyRatio 倍；普通帧大小在均值上下随机抖动 ±Jitter。
// 码率可在运行中调整（SetBitrate），M5 的带宽估计会接到这里。
package fakeenc

import (
	"math/rand/v2"
	"sync"
	"time"
)

type Frame struct {
	ID   uint32
	Key  bool
	Size int
	At   time.Time // "编码完成"时刻，帧延迟从这里开始算
}

type Config struct {
	FPS      int     // 默认 60
	Bitrate  int     // 目标码率 bit/s，默认 20Mbps
	KeyEvery int     // 关键帧间隔（帧），默认 120 = 2 秒
	KeyRatio float64 // 关键帧 / 普通帧 大小比，默认 8
	Jitter   float64 // 普通帧大小抖动比例，默认 0.2
}

type Encoder struct {
	cfg      Config
	mu       sync.Mutex
	bitrate  int
	next     uint32
	forceKey bool
}

func New(cfg Config) *Encoder {
	if cfg.FPS == 0 {
		cfg.FPS = 60
	}
	if cfg.Bitrate == 0 {
		cfg.Bitrate = 20_000_000
	}
	if cfg.KeyEvery == 0 {
		cfg.KeyEvery = 120
	}
	if cfg.KeyRatio == 0 {
		cfg.KeyRatio = 8
	}
	if cfg.Jitter == 0 {
		cfg.Jitter = 0.2
	}
	return &Encoder{cfg: cfg, bitrate: cfg.Bitrate}
}

func (e *Encoder) SetBitrate(bps int) {
	e.mu.Lock()
	e.bitrate = bps
	e.mu.Unlock()
}

// RequestKeyframe 让下一帧成为关键帧（丢帧后接收端请求 IDR 时用）。
func (e *Encoder) RequestKeyframe() {
	e.mu.Lock()
	e.forceKey = true
	e.mu.Unlock()
}

// Interval 是帧间隔。
func (e *Encoder) Interval() time.Duration { return time.Second / time.Duration(e.cfg.FPS) }

// Next 产出下一帧。普通帧均值按"一个关键帧周期内总字节数 = 码率 × 周期时长"反推：
// 周期内有 1 个关键帧 + (KeyEvery-1) 个普通帧，所以 P = 周期字节数 / (KeyEvery-1+KeyRatio)。
func (e *Encoder) Next() Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cfg
	gopBytes := float64(e.bitrate) / 8 * float64(c.KeyEvery) / float64(c.FPS)
	p := gopBytes / (float64(c.KeyEvery-1) + c.KeyRatio)
	f := Frame{ID: e.next, At: time.Now()}
	f.Key = e.forceKey || e.next%uint32(c.KeyEvery) == 0
	e.forceKey = false
	if f.Key {
		f.Size = int(p * c.KeyRatio)
	} else {
		f.Size = int(p * (1 + c.Jitter*(2*rand.Float64()-1)))
	}
	e.next++
	return f
}
