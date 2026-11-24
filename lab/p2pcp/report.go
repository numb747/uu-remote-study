package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// reporter 把进度同时报给三个地方：日志（人看）、stdout 机器行（手机 App 解析）、桌面通知（电脑端常驻服务）。
//
//	@STATE <文字>
//	@PROGRESS <方向 send|recv> <已传字节> <总字节> <当前 Mbps> <文件名>
//	@DONE <方向> <字节> <秒> <平均 Mbps> <路径或文件名>
//	@REASON <原因码> <人话>。<动作>     紧跟着一行 @ERROR（reason.go；旧版 App 不认识，忽略）
//	@ERROR <文字>
//	@CANCELED <me|peer> <文字>          本机 / 对方取消了（cancel.go），不是失败（旧版 App 不认识，忽略）
type reporter struct {
	machine bool
	notify  bool
	sink    func(string) // 桌面版（app.go）：同样的机器行推给网页

	mu      sync.Mutex
	nid     string // 当前通知的 ID，用来原地更新
	lastLog time.Time
}

func (r *reporter) line(format string, a ...any) {
	if r.machine {
		fmt.Printf(format+"\n", a...)
	}
	if r.sink != nil {
		r.sink(fmt.Sprintf(format, a...))
	}
}

func (r *reporter) state(s string) {
	log.Print(s)
	r.line("@STATE %s", s)
}

func (r *reporter) progress(dir string, done, total uint64, mbps float64, name string) {
	r.line("@PROGRESS %s %d %d %.1f %s", dir, done, total, mbps, name)
	pct := 0
	if total > 0 {
		pct = int(done * 100 / total)
	}
	r.mu.Lock()
	if time.Since(r.lastLog) >= time.Second {
		r.lastLog = time.Now()
		log.Printf("  %6.1f / %.1fMB  %3d%%  %.1f Mbps", float64(done)/1e6, float64(total)/1e6, pct, mbps)
	}
	r.mu.Unlock()
	r.desktop(fmt.Sprintf("%s %s", verb(dir), name), fmt.Sprintf("%.1f / %.1f MB　%.1f Mbps", float64(done)/1e6, float64(total)/1e6, mbps), pct)
}

// done：size 是文件大小，sent 是这次真正传的字节（续传时比 size 小），速度按 sent 算
func (r *reporter) done(dir string, size, sent uint64, el time.Duration, path string) {
	mbps := float64(sent) * 8 / el.Seconds() / 1e6
	resumed := ""
	if sent < size {
		resumed = fmt.Sprintf("（续传，这次传了 %.1fMB）", float64(sent)/1e6)
	}
	log.Printf("%s完成：%s  %.1fMB%s 用时 %v，平均 %.1f Mbps（%.2f MB/s）", verb(dir), path, float64(size)/1e6, resumed, el.Round(time.Millisecond), mbps, float64(sent)/el.Seconds()/1e6)
	r.line("@DONE %s %d %.3f %.1f %s", dir, size, el.Seconds(), mbps, path)
	r.desktop(fmt.Sprintf("%s完成 %s", verb(dir), shortName(path)),
		fmt.Sprintf("%.1f MB，用时 %.1f 秒，平均 %.1f Mbps", float64(size)/1e6, el.Seconds(), mbps), -1)
	r.mu.Lock()
	r.nid = "" // 下一个文件开新通知
	r.mu.Unlock()
}

func (r *reporter) fail(err error) {
	var ce canceledErr
	if errors.As(err, &ce) {
		who := "me"
		if ce.peer {
			who = "peer"
		}
		log.Print(err)
		r.line("@CANCELED %s %s", who, strings.ReplaceAll(err.Error(), "\n", " "))
		r.desktop("P2P 直传："+ce.Error(), err.Error(), -1)
		r.mu.Lock()
		r.nid = ""
		r.mu.Unlock()
		return
	}
	code := reasonOf(err)
	if code == "" {
		code = rsnUnknown
	}
	log.Printf("%v（%s）", err, code)
	r.line("@REASON %s", reasonLine(code))
	r.line("@ERROR %s", strings.ReplaceAll(err.Error(), "\n", " "))
	t := reasonText[code]
	r.desktop("P2P 直传失败："+t[0], t[1]+"\n\n"+err.Error(), -1)
	r.mu.Lock()
	r.nid = ""
	r.mu.Unlock()
}

// desktop 弹或更新一条桌面通知；pct ≥ 0 时带进度条（只有 Linux 弹进度，见 notify_*.go）。
func (r *reporter) desktop(title, body string, pct int) {
	if !r.notify {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := osNotify(title, body, pct, r.nid); ok {
		r.nid = id
	}
}

func verb(dir string) string {
	if dir == "send" {
		return "发送"
	}
	return "接收"
}

func shortName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
