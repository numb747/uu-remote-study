package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

// quicPair 在本机回环上建一对 QUIC 连接
func quicPair(t *testing.T) (client, server *quic.Conn) {
	t.Helper()
	tc, err := selfSigned(alpn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", tc, quicConf(alpn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			t.Error(err)
		}
		accepted <- c
	}()
	client, err = quic.DialAddr(ctx, ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}, quicConf(alpn))
	if err != nil {
		t.Fatal(err)
	}
	return client, <-accepted
}

func TestAuth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		client, server string
		ok             bool
	}{
		{"同一个房间", "room-a", "room-a", true},
		{"房间密钥不同", "room-a", "room-b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := quicPair(t)
			errc := make(chan error, 1)
			go func() { errc <- authConn(s, roomSecret(tc.server), false) }()
			cerr := authConn(c, roomSecret(tc.client), true)
			serr := <-errc
			if tc.ok && (cerr != nil || serr != nil) {
				t.Fatalf("应当通过：client %v, server %v", cerr, serr)
			}
			if !tc.ok && !errors.Is(serr, errAuth) {
				t.Fatalf("服务端应当拒绝，得到 %v", serr)
			}
			if !tc.ok && cerr == nil {
				t.Fatal("客户端不应当通过")
			}
		})
	}
}

// 反射：把对方发来的证明原样送回去，不能通过
func TestAuthReflection(t *testing.T) {
	c, s := quicPair(t)
	secret := roomSecret("room-a")
	go func() {
		str, err := s.AcceptStream(context.Background())
		if err != nil {
			return
		}
		b := make([]byte, 32)
		if _, err := str.Read(b); err == nil {
			str.Write(b) // 不知道密钥的"服务端"只能把客户端的证明弹回去
		}
	}()
	if err := authConn(c, secret, true); !errors.Is(err, errAuth) {
		t.Fatalf("反射回来的证明不应当通过，得到 %v", err)
	}
}

func TestRoomIDHidesRoom(t *testing.T) {
	id := roomID(roomSecret("abc"))
	if len(id) != 32 || id == roomID(roomSecret("abd")) {
		t.Fatalf("房间号不对：%s", id)
	}
}

func TestComplement(t *testing.T) {
	for _, tc := range []struct {
		have [][2]uint64
		size uint64
		want [][2]uint64
	}{
		{nil, 10, [][2]uint64{{0, 10}}},
		{[][2]uint64{{0, 10}}, 10, nil},
		{[][2]uint64{{2, 3}, {4, 2}}, 10, [][2]uint64{{0, 2}, {6, 4}}}, // 重叠的合并
		{[][2]uint64{{8, 100}, {0, 1}}, 10, [][2]uint64{{1, 7}}},       // 乱序、超出文件的裁掉
		{[][2]uint64{{0, 4}, {4, 4}}, 10, [][2]uint64{{8, 2}}},         // 首尾相接
		{[][2]uint64{{20, 5}}, 10, [][2]uint64{{0, 10}}},               // 完全在文件外
		{[][2]uint64{{3, 0}}, 10, [][2]uint64{{0, 10}}},                // 空区间
		{[][2]uint64{{0, 3}, {5, 1}, {9, 1}}, 10, [][2]uint64{{3, 2}, {6, 3}}},
	} {
		if got := complement(tc.have, tc.size); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("complement(%v, %d) = %v，应为 %v", tc.have, tc.size, got, tc.want)
		}
	}
}

func TestQuietConnTranslates(t *testing.T) {
	a, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer a.Close()
	real, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer real.Close()
	fake := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} // quic-go 以为的对方地址
	q := &quietConn{UDPConn: a, quicSees: fake, real: real.LocalAddr().(*net.UDPAddr)}
	if _, _, err := q.WriteMsgUDP([]byte("hi"), nil, fake); err != nil {
		t.Fatal(err)
	}
	real.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 10)
	n, _, err := real.ReadFromUDP(b)
	if err != nil || string(b[:n]) != "hi" {
		t.Fatalf("发往旧地址的包应当到新地址：%q %v", b[:n], err)
	}
}

// ---------------- 体检：画像、预测（natprofile.go / predict.go） ----------------

func udpAddr(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp4", s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestClassify(t *testing.T) {
	local := udpAddr("192.168.1.95:40629")
	m := udpAddr("120.1.4.60:40629")
	cases := []struct {
		name     string
		local    *net.UDPAddr
		mapped   []*net.UDPAddr
		class    string
		portKept bool
	}{
		{"四台一致、端口保持", local, []*net.UDPAddr{m, m, m, m}, natEIM, true},
		{"有一台没回也算", local, []*net.UDPAddr{m, nil, m, nil}, natEIM, true},
		{"端口变了", local, []*net.UDPAddr{m, udpAddr("120.1.4.60:40630")}, natSym, false},
		{"IP 变了（多出口）", local, []*net.UDPAddr{m, udpAddr("120.1.4.61:40629")}, natSym, false},
		{"映射就是本机", udpAddr("8.140.18.211:5000"), []*net.UDPAddr{udpAddr("8.140.18.211:5000"), udpAddr("8.140.18.211:5000")}, natOpen, true},
		{"只回了一台", local, []*net.UDPAddr{m, nil}, "", true},
		{"都没回", local, []*net.UDPAddr{nil, nil}, "", false},
	}
	for _, c := range cases {
		class, kept, note := classify(c.local, c.mapped)
		if class != c.class || kept != c.portKept {
			t.Errorf("%s：得到 %q 端口保持=%v（%s），要 %q %v", c.name, class, kept, note, c.class, c.portKept)
		}
		if class == "" && note == "" {
			t.Errorf("%s：未知时要说明原因", c.name)
		}
	}
}

func TestNatFlag(t *testing.T) {
	for _, p := range []profile{{Class: natSym, V6: true}, {Class: natEIM}, {Class: natOpen}} {
		got := parseNat("relay," + p.flag())
		if got.Class != p.Class || got.V6 != p.V6 {
			t.Errorf("%s 往返后成了 %+v", p.flag(), got)
		}
	}
	if (profile{}).flag() != "" {
		t.Error("未知画像不该发标志")
	}
	for _, flags := range []string{"-", "relay", "nat=weird"} {
		if p := parseNat(flags); p.Class != "" || p.Note == "" {
			t.Errorf("%q 应解析成未知并说明原因，得到 %+v", flags, p)
		}
	}
}

// TestPredict 照 netns 实验室的矩阵（docs/06 第 1.1 节）：full / restricted 归 cone
func TestPredict(t *testing.T) {
	cases := []struct{ me, peer, want string }{
		{natCone, natCone, predYes}, {natCone, natPort, predYes}, {natPort, natPort, predYes},
		{natCone, natSym, predYes}, {natSym, natCone, predYes}, {natOpen, natSym, predYes},
		{natPort, natSym, predNo}, {natSym, natPort, predNo}, {natSym, natSym, predNo},
		{natEIM, natSym, predMaybe}, {natSym, natEIM, predMaybe}, {natEIM, natEIM, predYes},
	}
	for _, c := range cases {
		v := predict(profile{Class: c.me}, profile{Class: c.peer})
		if v.Result != c.want || v.Why == "" {
			t.Errorf("%s ↔ %s：得到 %v，要 %s", c.me, c.peer, v, c.want)
		}
	}
	// IPv6：v4 打不通、两端都有 v6 → 不确定；只有一端有 v6 不变
	if v := predict(profile{Class: natSym, V6: true}, profile{Class: natSym, V6: true}); v.Result != predMaybe {
		t.Errorf("sym ↔ sym 都有 IPv6：得到 %v", v)
	}
	if v := predict(profile{Class: natSym, V6: true}, profile{Class: natSym}); v.Result != predNo {
		t.Errorf("sym ↔ sym 只有一端 IPv6：得到 %v", v)
	}
	if v := predict(profile{Class: natSym}, parseNat("-")); v.Result != predUnknown {
		t.Errorf("对方旧版：得到 %v", v)
	}
}

// fakeSTUN 在回环上起一个 STUN 服务器，映射由 mapTo 决定（nil = 原样回来源地址）
func fakeSTUN(t *testing.T, mapTo func(from *net.UDPAddr) *net.UDPAddr) string {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			m := from
			if mapTo != nil {
				m = mapTo(from)
			}
			c.WriteToUDP(bindingResponse(buf[:n], m), from)
		}
	}()
	return c.LocalAddr().String()
}

func TestNatProfileLoopback(t *testing.T) {
	// 原样回来源：映射 = 本机地址 → open，端口保持
	o := opts{bind: "127.0.0.1", only4: true}
	o.stun = fakeSTUN(t, nil) + "," + fakeSTUN(t, nil)
	if p := natProfile(o); p.Class != natOpen || !p.PortKept {
		t.Fatalf("原样回来源：%s", p)
	}
	// 每台服务器给不同端口 → sym；有一台不存在（不回）也照常判断
	o.stun = fakeSTUN(t, func(f *net.UDPAddr) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: f.Port + 1} }) + "," +
		fakeSTUN(t, func(f *net.UDPAddr) *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: f.Port + 2} }) +
		",127.0.0.1:1"
	p := natProfile(o)
	if p.Class != natSym || len(p.Mapped) != 3 || p.Mapped[2] != nil {
		t.Fatalf("端口随目的地变：%s", p)
	}
	if p.Took > time.Second || p.Took < 550*time.Millisecond {
		t.Errorf("两台回了、一台不回：应在约 600ms 时放弃等它，实际 %v", p.Took)
	}
}

func TestRefine(t *testing.T) {
	cases := []struct{ class, filter, want string }{
		{natEIM, filterLoose, natCone}, {natEIM, filterAPDF, natPort}, {natEIM, "", natEIM},
		{natSym, filterLoose, natSym}, {natOpen, filterAPDF, natOpen}, {"", filterLoose, ""},
	}
	for _, c := range cases {
		if got := refine(c.class, c.filter); got != c.want {
			t.Errorf("refine(%q, %q) = %q，要 %q", c.class, c.filter, got, c.want)
		}
	}
}

// fakeFT 在回环上起一个两端口的会合服务器，只答 /FT：从收包端口回 /F1，sendF2 时再从另一个端口回 /F2
func fakeFT(t *testing.T, sendF2 bool) string {
	t.Helper()
	var cs [2]*net.UDPConn
	for i := range cs {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		cs[i] = c
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := cs[0].ReadFromUDP(buf)
			if err != nil {
				return
			}
			nonce, _ := strings.CutPrefix(string(buf[:n]), "/FT ")
			cs[0].WriteToUDP([]byte("/F1 "+nonce), from)
			if sendF2 {
				cs[1].WriteToUDP([]byte("/F2 "+nonce), from)
			}
		}
	}()
	return cs[0].LocalAddr().String()
}

func TestProbeFilter(t *testing.T) {
	lo := net.IPv4(127, 0, 0, 1)
	if f, note := probeFilter(lo, fakeFT(t, true)); f != filterLoose {
		t.Errorf("两个端口都回：得到 %q（%s）", f, note)
	}
	if f, note := probeFilter(lo, fakeFT(t, false)); f != filterAPDF {
		t.Errorf("只回 /F1：得到 %q（%s）", f, note)
	}
	// 第一个地址没人听，换下一个
	if f, _ := probeFilter(lo, "127.0.0.1:1,"+fakeFT(t, true)); f != filterLoose {
		t.Errorf("跳过死地址后应为 loose，得到 %q", f)
	}
	if f, note := probeFilter(lo, ""); f != "" || note == "" {
		t.Errorf("没有会合地址：得到 %q（%s）", f, note)
	}
}

func TestProfileCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	o := opts{bind: "127.0.0.1", only4: true}
	o.stun = fakeSTUN(t, nil) + "," + fakeSTUN(t, nil)
	p1 := cachedProfile(o)
	if p1.Class != natOpen || !p1.CachedAt.IsZero() {
		t.Fatalf("第一次应现测：%s", p1)
	}
	p2 := cachedProfile(o)
	if p2.Class != natOpen || p2.CachedAt.IsZero() || p2.Took != 0 {
		t.Fatalf("第二次应命中缓存：%s", p2)
	}
	// 指纹变了（换了 STUN 清单）→ 重测
	o.stun = fakeSTUN(t, nil) + "," + fakeSTUN(t, nil)
	if p3 := cachedProfile(o); !p3.CachedAt.IsZero() {
		t.Fatalf("指纹变了还用缓存：%s", p3)
	}
	// 过期 → 重测
	path := profileCachePath()
	m := loadProfileCache(path)
	for k, e := range m {
		e.At = e.At.Add(-profileTTL)
		m[k] = e
	}
	b, _ := json.Marshal(m)
	os.WriteFile(path, b, 0o600)
	if p4 := cachedProfile(o); !p4.CachedAt.IsZero() {
		t.Fatalf("过期了还用缓存：%s", p4)
	}
	// 测不出来的不缓存
	o.stun = "127.0.0.1:1,127.0.0.1:2"
	cachedProfile(o)
	if _, ok := loadProfileCache(path)[netFingerprint(o)]; ok {
		t.Error("未知画像不该进缓存")
	}
}

func TestPreflightOrder(t *testing.T) {
	// 对方的画像先到、本机后到：齐了才预测
	p := &preflight{ready: make(chan struct{})}
	p.sawPeer("-") // 对方画像还没测完时的注册：不带 nat=，不算
	p.sawPeer("relay,nat=sym")
	if p.reported {
		t.Fatal("本机画像还没好就预测了")
	}
	if p.flag() != "" {
		t.Fatal("本机画像没好时不该带 nat=")
	}
	p.setMe(profile{Class: natPort})
	if !p.reported || p.verdict.Result != predNo || p.flag() != "nat=port" {
		t.Fatalf("齐了应预测打不通：%+v", p.verdict)
	}
	p.sawPeer("nat=cone") // 只报一次
	if p.peer.Class != natSym {
		t.Fatal("预测之后对方画像被改了")
	}
	// 对方的画像一直没到：settle 时说明
	q := &preflight{ready: make(chan struct{})}
	q.setMe(profile{Class: natCone})
	q.sawPeer("-")
	q.settle(outcome{actual: "直连"})
	if !q.reported || q.verdict.Result != predUnknown {
		t.Fatalf("对方画像没到：%+v", q.verdict)
	}
	// settle 等本机画像，最多 1.5 秒
	r := &preflight{ready: make(chan struct{})}
	go func() { time.Sleep(200 * time.Millisecond); r.setMe(profile{Class: natPort}) }()
	r.sawPeer("nat=cone")
	t0 := time.Now()
	r.settle(outcome{actual: "直连"})
	if el := time.Since(t0); !r.reported || r.verdict.Result != predYes || el < 150*time.Millisecond || el > time.Second {
		t.Fatalf("settle 应等到本机画像（%v）：%+v", el, r.verdict)
	}
	var nilp *preflight // -stun off 时 o.pre 是 nil，各方法都要能调
	nilp.sawPeer("nat=sym")
	nilp.wait(time.Second)
	nilp.settle(outcome{})
	if nilp.flag() != "" {
		t.Fatal("nil preflight 的 flag 应为空")
	}
}

func TestPreflightActual(t *testing.T) {
	// 预测打不通 → predictedNo；实际直连 → 记为预测错了
	p := &preflight{ready: make(chan struct{})}
	if p.predictedNo() {
		t.Fatal("还没预测")
	}
	p.setMe(profile{Class: natSym})
	p.sawPeer("nat=port")
	if !p.predictedNo() {
		t.Fatalf("sym ↔ port 应预测打不通：%+v", p.verdict)
	}
	p.setActual(false)
	if !p.checked || p.actual != "直连" {
		t.Fatalf("实际结果没记上：%+v", p)
	}
	// 先连上、后预测（打洞比体检快）：预测出来时再对
	q := &preflight{ready: make(chan struct{})}
	q.setActual(true)
	if q.checked {
		t.Fatal("还没预测就对上了")
	}
	q.setMe(profile{Class: natCone})
	q.sawPeer("nat=cone")
	if !q.checked || q.verdict.Result != predYes || q.actual != "中继" {
		t.Fatalf("预测出来后应和实际对一次：%+v", q)
	}
	var nilp *preflight
	nilp.setActual(true)
	if nilp.predictedNo() {
		t.Fatal("nil preflight 不该预测打不通")
	}
}

func TestWrongPrediction(t *testing.T) {
	cases := []struct {
		pred, actual string
		wrong        bool
	}{
		{predYes, "直连", false}, {predYes, "中继", true}, {predYes, "失败", true},
		{predNo, "中继", false}, {predNo, "失败", false}, {predNo, "直连", true},
		{predMaybe, "中继", false}, {predUnknown, "直连", false}, {"", "直连", false},
	}
	for _, c := range cases {
		if got := wrongPrediction(c.pred, c.actual); got != c.wrong {
			t.Errorf("预测%q 实际%q：得到 %v", c.pred, c.actual, got)
		}
	}
}

// TestSettleHistory：settle 写一行 history.jsonl，-progress 时 stdout 输出同样内容的 @CHECK；只记一次
func TestSettleHistory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	p := &preflight{role: "send", rep: &reporter{machine: true}, ready: make(chan struct{})}
	p.setMe(profile{Class: natSym, Mapped: []*net.UDPAddr{nil, udpAddr("1.2.3.4:5")}})
	p.sawPeer("nat=port.v6")
	out := outcome{actual: "直连", via: "5.6.7.8:9", took: 120 * time.Millisecond, punch: 80 * time.Millisecond, handshake: 4400 * time.Millisecond, rounds: 1}
	p.settle(out)
	p.settle(out) // 第二次不该再记
	w.Close()
	os.Stdout = stdout
	printed, _ := io.ReadAll(r)

	b, err := os.ReadFile(historyPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应只有一行记录，得到 %d 行", len(lines))
	}
	var rec histRec
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	want := histRec{Role: "send", Me: histProfile{Class: natSym, Mapped: "1.2.3.4:5"}, Peer: histProfile{Class: natPort, V6: true},
		Predict: predNo, Actual: "直连", Wrong: true, Via: "5.6.7.8:9", Rounds: 1, TookMs: 120, PunchMs: 80, HandshakeMs: 4400}
	rec.At, rec.Why = time.Time{}, ""
	if !reflect.DeepEqual(rec, want) {
		t.Fatalf("记录不对：\n得到 %+v\n要   %+v", rec, want)
	}
	if got := strings.Count(string(printed), "@CHECK {"); got != 1 {
		t.Fatalf("@CHECK 应输出 1 行，得到 %d：%s", got, printed)
	}
	if !strings.Contains(resultLine(want), "QUIC 握手 + 认证 4.4s") || !strings.Contains(resultLine(want), "预测错了") {
		t.Errorf("结果行：%s", resultLine(want))
	}
	// 失败也记，结果行里有原因
	q := &preflight{role: "recv", ready: make(chan struct{})}
	q.setMe(profile{Class: natPort})
	q.sawPeer("nat=sym")
	q.settle(outcome{actual: "失败", err: "打洞超时", rounds: 6})
	b, _ = os.ReadFile(historyPath())
	if n := strings.Count(string(b), "\n"); n != 2 || !strings.Contains(string(b), `"actual":"失败"`) {
		t.Fatalf("失败没记上：%s", b)
	}
}
