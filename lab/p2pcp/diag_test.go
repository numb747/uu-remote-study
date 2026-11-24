package main

import (
	"github.com/apernet/quic-go"

	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReasonOf(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errStopped, ""},
		{canceledErr{}, rsnCanceled},
		{fmt.Errorf("收「小B」的文件：%w", canceledErr{peer: true}), rsnPeerCanceled},
		{interrupted{fmt.Errorf("等对方回应: %w", &quic.ApplicationError{Remote: true, ErrorCode: codeCanceled})}, rsnPeerCanceled},
		{interrupted{&quic.ApplicationError{Remote: true, ErrorCode: 0}}, rsnLost}, // 对方退出（Ctrl-C）不算取消
		{fmt.Errorf("%w 1.2.3.4:7001：本机断网：%w", errNoRendezvous, errBadPort), rsnRdv},
		{fmt.Errorf("%w：2 秒没收到会合服务器的回执", errBadPort), rsnRdv},
		{fmt.Errorf("%w：5s 没收到回执", errLost), rsnRdv},
		{fmt.Errorf("%w：等了 60s，「小B」没有上线", errNoPeer), rsnOffline},
		{fmt.Errorf("连不上邀请方：%w", fmt.Errorf("%w：等了 20s", errNoPeer)), rsnOffline},
		{fmt.Errorf("能会合，但 6 轮都没打通：%w", fmt.Errorf("%w，对方候选 []", errPunchTimeout)), rsnPunch},
		{fmt.Errorf("%w（1.2.3.4:3478，6 秒没凑齐两端）", errRelay), rsnRelay},
		{interrupted{errors.New("等对方确认: timeout")}, rsnLost},
		{withReason(rsnLost, errors.New("连上 x 后 QUIC 握手: timeout")), rsnLost},
		{fmt.Errorf("%w（对方验不过本端的证明）", errAuth), rsnAuth},
		{&fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}, rsnLocal},
		{fmt.Errorf("/tmp %w", errNotRegular), rsnLocal},
		{fmt.Errorf("写入：%w", syscall.ENOSPC), rsnLocal},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}, rsnLocal},
		{interrupted{fmt.Errorf("写块：%w", syscall.ENOSPC)}, rsnLocal},
		{errors.New("对方校验 a.bin 失败"), rsnUnknown},
	}
	for _, c := range cases {
		if got := reasonOf(c.err); got != c.want {
			t.Errorf("reasonOf(%v) = %q，要 %q", c.err, got, c.want)
		}
	}
	for code := range reasonText {
		if l := reasonLine(code); !strings.HasPrefix(l, code+" ") || !strings.Contains(l, "。") {
			t.Errorf("reasonLine(%s) = %q", code, l)
		}
	}
}

// 对方验不过本端的证明时，本端看到的是读失败（对方以 0x50 关连接）：要归成认证失败，而不是断线
func TestAuthRejectedReason(t *testing.T) {
	c, s := quicPair(t)
	errc := make(chan error, 1)
	go func() { errc <- authConn(s, []byte("对的密钥"), false) }()
	err := authConn(c, []byte("错的密钥"), true)
	if got := reasonOf(err); got != rsnAuth {
		t.Fatalf("发起方：%v → %q，要 auth_failed", err, got)
	}
	if got := reasonOf(<-errc); got != rsnAuth {
		t.Fatalf("接收方 → %q，要 auth_failed", got)
	}
}

func TestTraceRender(t *testing.T) {
	var tr trace
	tr.add("第 1 轮：会合 1.2.3.4:7001")
	from := &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 9}
	for range 30 {
		tr.pkt("/PP", from)
	}
	tr.pkt("/PA", from)
	out := tr.render("头", "尾")
	for _, want := range []string{"第 1 轮：会合", "收到 /PP 来自 5.6.7.8:9（第一个）", "  30 个", "/PA 来自 5.6.7.8:9", "\n尾\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q：\n%s", want, out)
		}
	}
	if strings.Count(out, "收到 /PP") != 1 {
		t.Errorf("同一来源同一类型只该记一行：\n%s", out)
	}
	var nilPre *preflight
	nilPre.tracef("不该崩 %d", 1) // -stun off
	nilPre.tracePkt("/PP", from)
}

func TestPruneDir(t *testing.T) {
	dir := t.TempDir()
	for i := range 8 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("2026%02d.txt", i)), make([]byte, 100), 0o600)
	}
	os.WriteFile(filepath.Join(dir, "keep.log"), nil, 0o600)
	pruneDir(dir, ".txt", 5, 1<<20, "")
	ents, _ := os.ReadDir(dir)
	if len(ents) != 6 || ents[0].Name() != "202603.txt" {
		t.Fatalf("留 5 个最新的 + 别的后缀不动：%v", ents)
	}
	pruneDir(dir, ".txt", 50, 250, "")
	ents, _ = os.ReadDir(dir)
	if len(ents) != 3 || ents[0].Name() != "202606.txt" {
		t.Fatalf("总大小不超过 250 字节：%v", ents)
	}
}

func TestRedactor(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := t.TempDir()
	id, err := loadIdentity(cfg) // 第一次会生成
	if err != nil {
		t.Fatal(err)
	}
	id.Name = "我的电脑"
	saveIdentity(cfg, id)
	updateBook(cfg, func(b *book) error {
		b.addContact(contact{Name: "张三的手机", Pub: make([]byte, 32)})
		return nil
	})
	noteFile("/home/me/视频/婚礼.mp4")
	noteName("f", "合同.pdf")
	noteName("p", "已删的好友")
	noteName("p", "A") // 太短，不打码（会误伤）
	r := newRedactor(cfg)
	in := "发送完成：/home/me/视频/婚礼.mp4；接收 合同.pdf 来自「张三的手机」；收「已删的好友」的文件；我的电脑 → 好友 A；婚礼.mp4"
	out := r.apply(in)
	for _, bad := range []string{"婚礼", "合同", "张三", "已删的好友", "我的电脑", "/home/me"} {
		if strings.Contains(out, bad) {
			t.Errorf("没打掉 %q：%s", bad, out)
		}
	}
	for _, want := range []string{"<文件1.mp4>", ".pdf>", "好友 A", "本机"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q：%s", want, out)
		}
	}
	if strings.Count(out, "<文件1.mp4>") != 2 {
		t.Errorf("完整路径和文件名应对到同一个代号：%s", out)
	}
}

func TestLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p2pcp.log")
	w := &logFile{path: path, tag: "[send 1]"}
	line := []byte(strings.Repeat("x", 1000) + "\n")
	for range 1200 { // 1.2MB：超过 1MB 换文件
		w.Write(line)
	}
	a, _ := os.Stat(path)
	b, err := os.Stat(path + ".1")
	if err != nil || b.Size() <= logMaxBytes || a.Size() >= logMaxBytes {
		t.Fatalf("没换文件：%v %v", a, b)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), time.Now().Format("01-02 ")+"[send 1] xxx") {
		t.Fatalf("每行要带日期和进程：%.60s", data)
	}
}

func TestSettingsUpload(t *testing.T) {
	cfg := t.TempDir()
	if !loadSettings(cfg).uploadOn() {
		t.Fatal("默认要开")
	}
	off := false
	saveSettings(cfg, settings{Upload: &off})
	if loadSettings(cfg).uploadOn() {
		t.Fatal("关了还是开")
	}
}

// fakeCollector 是只回 /HK 的会合服务器；got 收到的每条记录
func fakeCollector(t *testing.T) (addr string, got chan string, stop func()) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan string, 100)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			body := buf[3:n]
			sum := sha256.Sum256(body)
			got <- string(body)
			c.WriteToUDP([]byte("/HK "+hex.EncodeToString(sum[:4])), from)
		}
	}()
	return c.LocalAddr().String(), got, func() { c.Close() }
}

func TestUploadQueue(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := t.TempDir()
	addr, got, stop := fakeCollector(t)
	rec := histRec{Role: "send", Actual: "失败", Reason: rsnOffline, Err: "「张三」没有上线", Ver: version}
	upload(cfg, addr+",1.2.3.4:7001", rec)
	select {
	case s := <-got:
		if strings.Contains(s, "张三") || !strings.Contains(s, `"reason":"peer_offline"`) {
			t.Fatalf("上报内容不对：%s", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务器没收到")
	}
	stop() // 服务器挂了：进队列，记下失败时间
	upload(cfg, addr, rec)
	t0 := time.Now()
	upload(cfg, addr, rec) // 5 分钟内不再试，直接进队列
	if time.Since(t0) > 100*time.Millisecond {
		t.Fatal("刚失败过还在试")
	}
	if n := len(readQueue(uploadQueuePath())); n != 2 {
		t.Fatalf("队列应有 2 条，有 %d", n)
	}
	// 服务器回来了（失败标记过期）：这条 + 队列里的 2 条都发出去
	os.Remove(filepath.Join(diagDir(), "upload-failed"))
	addr2, got2, stop2 := fakeCollector(t)
	defer stop2()
	upload(cfg, addr2, rec)
	if n := len(readQueue(uploadQueuePath())); n != 0 || len(got2) != 3 {
		t.Fatalf("补发后队列还有 %d 条，服务器收到 %d 条", n, len(got2))
	}
	// 关掉上报：什么都不发
	off := false
	saveSettings(cfg, settings{Upload: &off})
	upload(cfg, addr2, rec)
	time.Sleep(100 * time.Millisecond)
	if len(got2) != 3 {
		t.Fatal("关了上报还在发")
	}
}

func TestConnHash(t *testing.T) {
	if connHash("send.a", "recv.b") != connHash("recv.b", "send.a") || connHash("a", "b") == connHash("a", "c") {
		t.Fatal("两端要算出同一个连接号，不同的连接要不同")
	}
}
