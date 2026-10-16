package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// report 在接收端按帧记录结果，三种模式共用，保证指标口径一致。
type report struct {
	mu       sync.Mutex
	deadline time.Duration
	fps      int
	lat      map[uint32]time.Duration // 帧 ID → 收齐延迟（没收齐的不在表里）
	doneAt   map[uint32]time.Time     // 帧 ID → 收齐时刻
	key      map[uint32]bool
	offset   time.Duration // 接收端时钟 - 发送端时钟
}

func newReport(deadline time.Duration, fps int) *report {
	return &report{deadline: deadline, fps: fps, lat: map[uint32]time.Duration{}, doneAt: map[uint32]time.Time{}, key: map[uint32]bool{}}
}

func (r *report) frame(id uint32, key bool, lat time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// own 的时间戳是 32 位微秒（约 71 分钟回绕）：接收端时钟比发送端慢时差值为负，会回绕成接近 2^32 微秒，先还原成负数再扣偏差
	if wrap := time.Duration(1<<32) * time.Microsecond; lat > wrap/2 {
		lat -= wrap
	}
	if _, dup := r.lat[id]; !dup {
		r.lat[id] = lat - r.offset
		r.doneAt[id] = time.Now()
	}
	if key {
		r.key[id] = true
	}
}

// seenTotal 在没收到 BYE 时兜底：用收到过的最大帧号推算总帧数（末尾整段丢失的帧会被漏算）。
func (r *report) seenTotal() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n uint32
	for id := range r.lat {
		n = max(n, id+1)
	}
	return n
}

func pct(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	return xs[min(len(xs)-1, int(float64(len(xs))*p))]
}

// summary 输出两行：人读的一段，以及以 "RESULT" 开头、便于脚本汇总的一行。
//
// "可播放"按解码依赖算：P 帧要等上一帧解码完才能解码，所以它的有效显示时刻 = max(自己收齐, 上一帧有效显示时刻)；
// 有效延迟（有效显示时刻 - 编码时刻）不超过截止时间才算可播放。迟到的帧会顺延后面的帧，
// 但依赖链不断；一帧彻底没收到，依赖链就断了，画面冻结到下一个关键帧。
// v1 没有"丢帧后请求关键帧"，所以一次丢帧的代价最多是一个关键帧周期（2 秒）的冻结。
func (r *report) summary(mode string, total uint32) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lats, keyLats []time.Duration
	onTime, late := 0, 0
	for id, l := range r.lat {
		lats = append(lats, l)
		if r.key[id] {
			keyLats = append(keyLats, l)
		}
		if l <= r.deadline {
			onTime++
		} else {
			late++
		}
	}
	slices.Sort(lats)
	slices.Sort(keyLats)
	lost := int(total) - len(r.lat)

	playable, freezes := 0, 0
	prevOK, chain := true, false
	var prevEff time.Time
	for id := range total {
		l, got := r.lat[id]
		ok := false
		switch {
		case !got:
			chain = false
		case r.key[id] || chain:
			eff := r.doneAt[id]
			if !r.key[id] && prevEff.After(eff) {
				eff = prevEff
			}
			chain, prevEff = true, eff
			ok = eff.Sub(r.doneAt[id].Add(-l)) <= r.deadline
		}
		if ok {
			playable++
		} else if prevOK {
			freezes++
		}
		prevOK = ok
	}
	frozen := time.Duration(int(total)-playable) * time.Second / time.Duration(r.fps)

	var b strings.Builder
	n := max(1, int(total))
	fmt.Fprintf(&b, "[%s] 帧 %d：按时 %d (%.1f%%)  迟到 %d  丢失 %d\n", mode, total, onTime, 100*float64(onTime)/float64(n), late, lost)
	fmt.Fprintf(&b, "[%s] 帧延迟 p50 %v  p95 %v  p99 %v  max %v\n", mode,
		pct(lats, .5).Round(100*time.Microsecond), pct(lats, .95).Round(100*time.Microsecond),
		pct(lats, .99).Round(100*time.Microsecond), pct(lats, 1).Round(100*time.Microsecond))
	fmt.Fprintf(&b, "[%s] 关键帧 收齐 %d 个，延迟 p50 %v  max %v\n", mode, len(keyLats),
		pct(keyLats, .5).Round(100*time.Microsecond), pct(keyLats, 1).Round(100*time.Microsecond))
	fmt.Fprintf(&b, "[%s] 可播放 %.1f%%  冻结 %d 次，共 %v\n", mode, 100*float64(playable)/float64(n), freezes, frozen.Round(time.Millisecond))
	fmt.Fprintf(&b, "RESULT mode=%s frames=%d ontime=%.2f late=%d lost=%d p50=%.1f p95=%.1f p99=%.1f max=%.1f playable=%.2f freezes=%d frozen_ms=%d key_p50=%.1f",
		mode, total, 100*float64(onTime)/float64(n), late, lost,
		ms(pct(lats, .5)), ms(pct(lats, .95)), ms(pct(lats, .99)), ms(pct(lats, 1)),
		100*float64(playable)/float64(n), freezes, frozen.Milliseconds(), ms(pct(keyLats, .5)))
	return b.String()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
