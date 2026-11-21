package main

// 诊断（docs/arch/DESIGN.md 决策 25、26）：日志落盘、崩溃留现场、诊断报告。
//
// 都在缓存目录 ~/.cache/p2pcp/（认 XDG_CACHE_HOME；安卓 App 给的是它自己的缓存目录）：
//
//	log/p2pcp.log、p2pcp.log.1   日志，每行带「日期 [命令 pid]」，超过 1MB 换一个文件，最多两个
//	crash.txt                   每个进程启动时把 Go 运行时的崩溃输出（任何 goroutine 的 panic、fatal error）接到这里；
//	                            下次启动时发现它不是空的，就改名成 crash-<时间>.txt，提示一句（-progress 时 @CRASH）
//	names.txt                   传过的文件名、好友名：诊断报告据此把它们换成代号（文件 3.mp4、好友 2），自己不进报告
//	report-<时间>.txt            p2pcp report 生成的诊断报告
//
// 诊断报告：本机信息和设置、现测一次单端体检（p2pcp check）、最近 50 次连接的结果记录、最近 10 次连接过程（trace.go）、
// 崩溃现场、最近的日志。几十 KB 的文本，微信能直接发。不含任何密钥；好友名、本机设备名、文件名都换成代号。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // 安卓上没有 /usr/share/zoneinfo：App 传 TZ=Asia/Shanghai 时靠它认得，日志、报告里的时间才是本地时间
	"unicode/utf8"
)

const (
	logMaxBytes = 1 << 20
	crashKeep   = 10
)

func diagDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "p2pcp")
}

// ---------------- 日志落盘 ----------------

// logFile 是落盘的那一份日志：几个进程（serve 和 send）同时追加同一个文件，每行一次 write（O_APPEND），不会交错
type logFile struct {
	mu   sync.Mutex
	path string
	tag  string
	f    *os.File
	n    int
}

func (w *logFile) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return len(b), nil
		}
		w.f = f
	}
	w.f.Write(append([]byte(time.Now().Format("01-02 ")+w.tag+" "), b...))
	if w.n++; w.n%100 == 1 { // 隔一阵看一次大小：别的进程也在写
		if st, err := w.f.Stat(); err == nil && st.Size() > logMaxBytes {
			w.f.Close()
			w.f = nil
			os.Rename(w.path, w.path+".1")
		}
	}
	return len(b), nil
}

// tee 写到 stderr 和日志文件；任何一边出错都不影响另一边（App 关掉了 stderr 的读端时日志照样落盘）
type tee []io.Writer

func (t tee) Write(b []byte) (int, error) {
	for _, w := range t {
		w.Write(b)
	}
	return len(b), nil
}

// setupLog：日志同时落盘
func setupLog(role string) {
	dir := diagDir()
	if dir == "" || os.MkdirAll(filepath.Join(dir, "log"), 0o700) != nil {
		return
	}
	log.SetOutput(tee{os.Stderr, &logFile{path: filepath.Join(dir, "log", "p2pcp.log"), tag: fmt.Sprintf("[%s %d]", role, os.Getpid())}})
}

// ---------------- 崩溃现场 ----------------

// setupCrash：先把上次留下的崩溃输出改名存好、提示一句，再把这个进程的崩溃输出接到 crash.txt
func setupCrash(rep *reporter) {
	dir := diagDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "crash.txt")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		dst := filepath.Join(dir, "crash-"+st.ModTime().Format("20060102-150405")+".txt")
		if os.Rename(path, dst) == nil {
			log.Printf("P2P 直传上次崩溃了，现场存在 %s（会放进诊断报告）", dst)
			rep.line("@CRASH %s", dst)
		}
		pruneDir(dir, ".txt", crashKeep, 1<<30, "crash-")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	debug.SetCrashOutput(f, debug.CrashOptions{})
	f.Close() // SetCrashOutput 自己复制了一份描述符
}

// saveCrash：recover 住的 panic（常驻接收里一个连接出的问题，不该带走整个进程）也留一份现场
func saveCrash(r any, stack []byte) string {
	dir := diagDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	path := filepath.Join(dir, "crash-"+time.Now().Format("20060102-150405")+"-"+randHex(2)+".txt")
	os.WriteFile(path, fmt.Appendf(nil, "p2pcp %s %s/%s，已 recover 的 panic：%v\n\n%s", version, runtime.GOOS, runtime.GOARCH, r, stack), 0o600)
	pruneDir(dir, ".txt", crashKeep, 1<<30, "crash-")
	return path
}

// ---------------- 文件名、好友名登记（给诊断报告打码用） ----------------

var (
	namesMu   sync.Mutex
	namesSeen = map[string]bool{}
)

// noteName 记下一个要打码的名字。kind：f = 文件名或路径，p = 好友名
func noteName(kind, name string) {
	if utf8.RuneCountInString(name) < 2 {
		return
	}
	namesMu.Lock()
	defer namesMu.Unlock()
	k := kind + "\t" + name
	if namesSeen[k] || strings.ContainsAny(name, "\n\t") {
		return
	}
	namesSeen[k] = true
	dir := diagDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "names.txt")
	if st, err := os.Stat(path); err == nil && st.Size() > 256<<10 {
		os.Rename(path, path+".1") // 太长就换一个，旧的报告时也读
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		f.WriteString(k + "\n")
		f.Close()
	}
}

// noteFile 登记一个文件：完整路径和文件名都要打码
func noteFile(path string) {
	noteName("f", path)
	if abs, err := filepath.Abs(path); err == nil {
		noteName("f", abs)
	}
	noteName("f", filepath.Base(path))
}

// redactor 把名字换成代号：先换长的（完整路径里含着文件名和好友名）
type redactor struct {
	pairs [][2]string
}

func newRedactor(cfg string) *redactor {
	files, friends := []string{}, []string{}
	add := func(list *[]string, s string) {
		if utf8.RuneCountInString(s) >= 2 && !slices.Contains(*list, s) {
			*list = append(*list, s)
		}
	}
	dir := diagDir()
	for _, p := range []string{filepath.Join(dir, "names.txt.1"), filepath.Join(dir, "names.txt")} {
		b, _ := os.ReadFile(p)
		for _, l := range strings.Split(string(b), "\n") {
			kind, name, ok := strings.Cut(l, "\t")
			switch {
			case !ok:
			case kind == "f":
				add(&files, name)
			case kind == "p":
				add(&friends, name)
			}
		}
	}
	if bk, err := loadBook(cfg); err == nil {
		for _, c := range bk.Contacts {
			add(&friends, c.Name)
		}
	}
	r := &redactor{}
	if id, err := loadIdentity(cfg); err == nil && utf8.RuneCountInString(id.Name) >= 2 {
		r.pairs = append(r.pairs, [2]string{id.Name, "本机"})
	}
	for i, f := range friends {
		r.pairs = append(r.pairs, [2]string{f, fmt.Sprintf("好友%d", i+1)})
	}
	// 文件：完整路径和文件名对到同一个代号，保留扩展名（看得出是视频还是压缩包）
	codes := map[string]string{}
	for _, f := range files {
		base := filepath.Base(f)
		if codes[base] == "" {
			codes[base] = fmt.Sprintf("文件%d%s", len(codes)+1, filepath.Ext(base))
		}
		r.pairs = append(r.pairs, [2]string{f, "<" + codes[base] + ">"})
	}
	slices.SortStableFunc(r.pairs, func(a, b [2]string) int { return len(b[0]) - len(a[0]) })
	return r
}

func (r *redactor) apply(s string) string {
	for _, p := range r.pairs {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	return s
}

// ---------------- 诊断报告 ----------------

// cmdReport 生成诊断报告，打印路径（-progress 时 @REPORT <路径>）。args[0] 可以指定输出文件
func cmdReport(o opts, args []string) error {
	cfg := configDir(o)
	dir := diagDir()
	if dir == "" {
		return withReason(rsnLocal, fmt.Errorf("找不到缓存目录"))
	}
	out := filepath.Join(dir, "report-"+time.Now().Format("20060102-150405")+".txt")
	if len(args) > 0 {
		out = args[0]
	}
	o.rep.state("正在生成诊断报告（要现测一次网络，十几秒）……")
	var b strings.Builder
	sec := func(title string) { fmt.Fprintf(&b, "\n==================== %s ====================\n", title) }

	fmt.Fprintf(&b, "P2P 直传诊断报告\n生成于 %s\np2pcp %s，%s/%s，%s\n", time.Now().Format("2006-01-02 15:04:05 -0700"), version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	if id, err := loadIdentity(cfg); err == nil {
		fmt.Fprintf(&b, "本机设备 ID %s\n", deviceID(id.pub()))
	}
	servers := []string{}
	if o.server != "" {
		servers = append(servers, o.server)
	}
	if bk, err := loadBook(cfg); err == nil {
		fmt.Fprintf(&b, "好友 %d 个，待用邀请码 %d 个\n", len(bk.Contacts), len(liveInvites(bk.Invites)))
		for _, c := range bk.Contacts {
			if c.Server != "" && !slices.Contains(servers, c.Server) {
				servers = append(servers, c.Server)
			}
		}
	}
	fmt.Fprintf(&b, "会合服务器 %s\n", strings.Join(servers, " | "))
	fmt.Fprintf(&b, "诊断数据上报：%s\n", map[bool]string{true: "开", false: "关"}[loadSettings(cfg).uploadOn()])

	sec("本机体检（现测）")
	if len(servers) == 0 {
		b.WriteString("没有会合服务器地址，跳过\n")
	} else {
		b.WriteString(selfCheck(o, servers[0]))
	}

	sec("最近 50 次连接的结果记录（history.jsonl）")
	b.WriteString(tailLines(historyPath(), 50))
	if q := tailLines(filepath.Join(dir, "upload-queue.jsonl"), 100); q != "" {
		sec("还没上报出去的记录")
		b.WriteString(q)
	}

	sec("最近 10 次连接过程（trace）")
	b.WriteString(lastFiles(traceDir(), "", 10, 24<<10))

	sec("崩溃现场")
	if s := lastFiles(dir, "crash-", 3, 32<<10); s != "" {
		b.WriteString(s)
	} else {
		b.WriteString("没有\n")
	}

	sec("最近的日志")
	logs := tailLines(filepath.Join(dir, "log", "p2pcp.log.1"), 1000) + tailLines(filepath.Join(dir, "log", "p2pcp.log"), 1000)
	b.WriteString(lastN(logs, 600))

	text := newRedactor(cfg).apply(b.String())
	if err := os.WriteFile(out, []byte(text), 0o600); err != nil {
		return withReason(rsnLocal, err)
	}
	pruneDir(dir, ".txt", 5, 1<<30, "report-")
	log.Printf("诊断报告：%s（%.1fKB），可以直接发给我", out, float64(len(text))/1024)
	o.rep.line("@REPORT %s", out)
	fmt.Println(out)
	return nil
}

// selfCheck 另起一个进程跑单端体检 p2pcp check，收下它的输出（check 直接往 stdout 打）
func selfCheck(o opts, server string) string {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Sprintf("跑不了：%v\n", err)
	}
	args := []string{"check", "-server", server, "-stun", o.stun}
	if o.bind != "" {
		args = append(args, "-bind", o.bind)
	}
	if o.lan != "" {
		args = append(args, "-lan", o.lan)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.Env = append(os.Environ(), "P2PCP_NO_LOG=1") // 体检的输出已经进报告了，不用再落一遍盘
	err = cmd.Run()
	s := fmt.Sprintf("$ p2pcp %s\n%s", strings.Join(args, " "), buf.String())
	if err != nil {
		s += fmt.Sprintf("（退出：%v）\n", err)
	}
	return s
}

// tailLines 文件的最后 n 行（文件不在时为空）
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 4<<20 {
		f.Seek(-4<<20, io.SeekEnd)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 2*n {
			lines = slices.Clone(lines[len(lines)-n:])
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func lastN(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// lastFiles 目录里文件名以 prefix 开头的最近 n 个文件（按文件名，名字以时间开头），每个最多 max 字节
func lastFiles(dir, prefix string, n, max int) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	if len(names) > n {
		names = names[len(names)-n:]
	}
	var b strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "---- %s ----\n", name)
		if len(data) > max {
			fmt.Fprintf(&b, "（太长，只放前 %dKB）\n", max>>10)
			data = data[:max]
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---------------- 设置 ----------------

// settings 是配置目录里的 settings.json（DESIGN 决策 22：诊断数据上报默认开、可以关）
type settings struct {
	Upload *bool `json:"upload,omitempty"` // nil = 默认（开）
}

func (s settings) uploadOn() bool { return s.Upload == nil || *s.Upload }

func settingsPath(cfg string) string { return filepath.Join(cfg, "settings.json") }

func loadSettings(cfg string) settings {
	var s settings
	if b, err := os.ReadFile(settingsPath(cfg)); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func saveSettings(cfg string, s settings) error {
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		return err
	}
	return writeJSON(settingsPath(cfg), s)
}
