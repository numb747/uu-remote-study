package main

// 桌面版 p2pcp app（docs/arch/DESIGN.md 决策 9，实现顺序第 4 步；图 34-contacts 的 webui）：
// 一个进程里跑常驻接收 serve，再开一个只听 127.0.0.1 的小 HTTP 服务，网页（app.html，内嵌进二进制）就是界面：
// 通讯录、加好友（二维码 / 粘贴邀请码）、选文件发送（拖进来也行）、收发进度和记录、诊断报告、上报开关。
// 纯 Go、不要 cgo，Windows / macOS / Linux 一份代码。
//
// 进度：serve 和每次发送各用一个 reporter，sink 把和 -progress 一样的 @ 行（report.go）经 SSE 推给网页，
// 网页照安卓 App 的 Panel.handle 解析。最近 200 行留在内存里，网页刷新时补上（收发记录先不落盘）。
//
// 本地网页的安全（别的网站能让浏览器往 127.0.0.1 发请求，还有 DNS rebinding）：
//   - 随机端口 + 随机令牌。浏览器用 http://127.0.0.1:端口/?t=令牌 打开，换成 HttpOnly、SameSite=Strict 的 cookie
//   - 每个请求核对 Host 必须是 127.0.0.1:端口（挡 DNS rebinding），核对 cookie 里的令牌
//   - 改东西的接口只收 POST，并核对 Origin 必须是 http://127.0.0.1:端口（挡 CSRF）
//
// 单实例：配置目录里写 app.json（端口 + 令牌 + pid）。再运行 p2pcp app 时，先问一下那个端口还在不在（/ping），
// 在就只打开浏览器到它、退出；开机自启用 -no-browser。

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed app.html
var appHTML []byte

const appCookie = "p2pcp_t"

type appInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func (a appInfo) url() string { return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", a.Port, a.Token) }

// ---------------- 事件：@ 行推给网页 ----------------

type appEvent struct {
	Seq  int64  `json:"seq"`
	At   int64  `json:"at"` // 毫秒
	Ch   string `json:"ch"` // recv | send:N
	Line string `json:"line"`
}

type hub struct {
	mu     sync.Mutex
	seq    int64
	recent []appEvent
	subs   map[chan appEvent]bool
}

func (h *hub) pub(ch string) func(string) {
	return func(line string) {
		if strings.HasPrefix(line, "@PROGRESS ") { // 进度每 500ms 一行，不留在记录里，只推
			h.send(ch, line, false)
			return
		}
		h.send(ch, line, true)
	}
}

func (h *hub) send(ch, line string, keep bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	e := appEvent{Seq: h.seq, At: time.Now().UnixMilli(), Ch: ch, Line: line}
	if keep {
		h.recent = append(h.recent, e)
		if len(h.recent) > 200 {
			h.recent = slices.Clone(h.recent[len(h.recent)-200:])
		}
	}
	for c := range h.subs {
		select {
		case c <- e:
		default: // 网页卡住了就丢，别拖住传输
		}
	}
}

func (h *hub) subscribe() (chan appEvent, []appEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan appEvent, 256)
	h.subs[c] = true
	return c, slices.Clone(h.recent), func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

// ---------------- 启动 ----------------

func appInfoPath(cfg string) string { return filepath.Join(cfg, "app.json") }

// runningApp：已经有一个 p2pcp app 在跑，返回它的信息
func runningApp(cfg string) (appInfo, bool) {
	var a appInfo
	b, err := os.ReadFile(appInfoPath(cfg))
	if err != nil || json.Unmarshal(b, &a) != nil || a.Port == 0 {
		return a, false
	}
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", a.Port))
	if err != nil {
		return a, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return a, resp.StatusCode == 200 && string(body) == "p2pcp app"
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("打不开浏览器（%v），自己打开 %s", err, url)
		return
	}
	go cmd.Wait()
}

// cmdApp：p2pcp app [-no-browser] [-o 目录] [-bind auto] [-notify]
func cmdApp(o opts, noBrowser bool) error {
	cfg := configDir(o)
	if a, ok := runningApp(cfg); ok {
		log.Printf("P2P 直传已经在运行（pid %d），打开 %s", a.PID, strings.SplitN(a.url(), "?", 2)[0])
		if !noBrowser {
			openBrowser(a.url())
		}
		return nil
	}
	if _, err := loadIdentity(cfg); err != nil { // 第一次运行：生成设备密钥
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return withReason(rsnLocal, err)
	}
	info := appInfo{Port: ln.Addr().(*net.TCPAddr).Port, Token: randHex(16), PID: os.Getpid()}
	if err := writeJSON(appInfoPath(cfg), info); err != nil {
		return withReason(rsnLocal, err)
	}
	a := &app{o: o, cfg: cfg, info: info, h: &hub{subs: map[chan appEvent]bool{}}}
	// 常驻接收：和 p2pcp serve 一样，进度推给网页
	ro := o
	ro.rep = &reporter{notify: o.rep.notify, sink: a.h.pub("recv")}
	go func() {
		if err := serve(ro); err != nil {
			ro.rep.fail(err)
		}
	}()
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("P2P 直传桌面版：http://127.0.0.1:%d/（只有本机能打开）", info.Port)
	if !noBrowser {
		openBrowser(info.url())
	}
	return srv.Serve(ln)
}

type app struct {
	o     opts
	cfg   string
	info  appInfo
	h     *hub
	sends atomic.Int64
}

// ---------------- HTTP ----------------

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "p2pcp app") })
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("POST /api/invite", a.invite)
	mux.HandleFunc("POST /api/join", a.join)
	mux.HandleFunc("POST /api/send", a.send)
	mux.HandleFunc("POST /api/cancel", a.cancel)
	mux.HandleFunc("POST /api/me", a.rename)
	mux.HandleFunc("POST /api/friend", a.friend)
	mux.HandleFunc("POST /api/upload", a.upload)
	mux.HandleFunc("POST /api/report", a.report)
	mux.HandleFunc("GET /api/report", a.reportFile)
	return a.guard(mux)
}

// guard：Host、令牌、Origin（见文件开头）
func (a *app) guard(next http.Handler) http.Handler {
	host := fmt.Sprintf("127.0.0.1:%d", a.info.Port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "只能用 http://"+host+"/ 打开", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/ping" {
			next.ServeHTTP(w, r)
			return
		}
		// 带令牌打开：换成 cookie，再跳到不带令牌的地址（令牌别留在地址栏和历史记录里）
		if t := r.URL.Query().Get("t"); t != "" && r.Method == http.MethodGet && r.URL.Path == "/" {
			if !a.tokenOK(t) {
				http.Error(w, "令牌不对：重新运行 p2pcp app 打开", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: appCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(appCookie)
		if err != nil || !a.tokenOK(c.Value) {
			http.Error(w, "请从 P2P 直传的启动器（或运行 p2pcp app）打开，不要直接输入地址", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") != "http://"+host {
			http.Error(w, "来源不对", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (a *app) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(a.info.Token)) == 1
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
	w.Write(appHTML)
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// httpFail 把错误连同原因码回给网页
func httpFail(w http.ResponseWriter, err error) {
	code := reasonOf(err)
	if code == "" {
		code = rsnUnknown
	}
	t := reasonText[code]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "reason": code, "human": t[0], "action": t[1]})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v)
}

func (a *app) state(w http.ResponseWriter, r *http.Request) {
	id, err := loadIdentity(a.cfg)
	if err != nil {
		httpFail(w, err)
		return
	}
	b, err := loadBook(a.cfg)
	if err != nil {
		httpFail(w, err)
		return
	}
	type friend struct {
		Name   string `json:"name"`
		ID     string `json:"id"`
		Server string `json:"server"`
	}
	fs := []friend{}
	slices.SortFunc(b.Contacts, func(x, y contact) int { return x.Added.Compare(y.Added) })
	for _, c := range b.Contacts {
		fs = append(fs, friend{c.Name, c.id(), c.Server})
	}
	a.h.mu.Lock()
	recent := slices.Clone(a.h.recent)
	a.h.mu.Unlock()
	writeJSONResp(w, map[string]any{
		"me":      map[string]string{"name": id.Name, "id": deviceID(id.pub())},
		"friends": fs, "invites": len(liveInvites(b.Invites)),
		"upload": loadSettings(a.cfg).uploadOn(), "version": version, "out": a.o.out,
		"events": recent,
	})
}

// events：SSE。先补上最近的记录（seq 大于 since 的），再一直推
func (a *app) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	c, _, done := a.h.subscribe()
	defer done()
	write := func(e appEvent) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	io.WriteString(w, ": hi\n\n")
	fl.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-c:
			write(e)
			fl.Flush()
		case <-tick.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (a *app) invite(w http.ResponseWriter, r *http.Request) {
	code, err := newInvite(a.o)
	if err != nil {
		httpFail(w, err)
		return
	}
	png, err := qrPNG(code)
	if err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]string{"code": code, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
}

func (a *app) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readBody(r, &req); err != nil {
		httpFail(w, err)
		return
	}
	o := a.o
	o.timeout = 20 * time.Second // 和命令行 join 一样：邀请方在线时 1 秒内就配上
	o.rep = &reporter{notify: a.o.rep.notify, sink: a.h.pub("pair")}
	name, err := join(o, req.Code)
	if err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]string{"name": name})
}

// send：网页选的文件先经 HTTP 传进本机的临时目录（和安卓 App 先复制到缓存一样），再在后台发；马上返回发送号
func (a *app) send(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		httpFail(w, err)
		return
	}
	tmp, err := os.MkdirTemp("", "p2pcp-app-")
	if err != nil {
		httpFail(w, withReason(rsnLocal, err))
		return
	}
	var friend string
	var paths []string
	cleanup := func() { os.RemoveAll(tmp) }
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			httpFail(w, err)
			return
		}
		switch p.FormName() {
		case "friend":
			b, _ := io.ReadAll(io.LimitReader(p, 256))
			friend = string(b)
		case "file":
			name := filepath.Base(p.FileName())
			if name == "." || name == "/" || name == "" {
				continue
			}
			// 同名的放进不同子目录，接收端看到的还是原名
			dir := filepath.Join(tmp, fmt.Sprint(len(paths)))
			os.MkdirAll(dir, 0o700)
			path := filepath.Join(dir, name)
			f, err := os.Create(path)
			if err != nil {
				cleanup()
				httpFail(w, withReason(rsnLocal, err))
				return
			}
			_, err = io.Copy(f, p)
			f.Close()
			if err != nil {
				cleanup()
				httpFail(w, withReason(rsnLocal, err))
				return
			}
			paths = append(paths, path)
		}
	}
	if friend == "" || len(paths) == 0 {
		cleanup()
		httpFail(w, errors.New("要选好友和文件"))
		return
	}
	o := a.o
	o.friend = friend
	if err := resolveFriend(&o, "send"); err != nil {
		cleanup()
		httpFail(w, err)
		return
	}
	ch := fmt.Sprintf("send:%d", a.sends.Add(1))
	o.rep = &reporter{notify: a.o.rep.notify, sink: a.h.pub(ch)}
	o.x = newXfer(ch, "send", o.friend) // 网页上的「取消」按发送号找它（cancel.go）
	o.rep.line("@TO %s", o.friend)
	go func() {
		defer cleanup()
		defer o.x.end()
		if err := send(o, paths); err != nil {
			o.rep.fail(err)
		}
		o.rep.line("@END") // 这次发送结束了（成功、失败、取消都有）：网页收起「取消」按钮
	}()
	writeJSONResp(w, map[string]string{"ch": ch, "friend": o.friend})
}

// cancel：{"ch":"send:N"} 取消这次发送；{"ch":"recv"} 取消正在收的（所有好友的）
func (a *app) cancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ch string `json:"ch"`
	}
	if err := readBody(r, &req); err != nil {
		httpFail(w, err)
		return
	}
	n := cancelXfers(func(x *xfer) bool {
		if req.Ch == "recv" {
			return x.dir == "recv"
		}
		return x.dir == "send" && x.id == req.Ch
	})
	writeJSONResp(w, map[string]int{"n": n})
}

func (a *app) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readBody(r, &req); err != nil {
		httpFail(w, err)
		return
	}
	id, err := loadIdentity(a.cfg)
	if err == nil {
		if id.Name = cleanName(req.Name); id.Name == "" {
			err = errors.New("设备名不能是空的")
		} else {
			err = saveIdentity(a.cfg, id)
		}
	}
	if err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]string{"name": id.Name})
}

// friend：{"op":"rm","name"} 删好友；{"op":"rename","name","to"} 改名
func (a *app) friend(w http.ResponseWriter, r *http.Request) {
	var req struct{ Op, Name, To string }
	if err := readBody(r, &req); err != nil {
		httpFail(w, err)
		return
	}
	var err error
	switch req.Op {
	case "rm":
		_, err = rmContact(a.cfg, req.Name)
	case "rename":
		_, _, err = renameContact(a.cfg, req.Name, req.To)
	default:
		err = errors.New("不认识的操作")
	}
	if err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]bool{"ok": true})
}

func (a *app) upload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := readBody(r, &req); err != nil {
		httpFail(w, err)
		return
	}
	st := loadSettings(a.cfg)
	st.Upload = &req.On
	if err := saveSettings(a.cfg, st); err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]bool{"upload": req.On})
}

// report：生成诊断报告（要现测一次网络，十几秒），回报告的文件名，网页再用 GET 下载
func (a *app) report(w http.ResponseWriter, r *http.Request) {
	o := a.o
	var buf bytes.Buffer
	o.rep = &reporter{sink: func(l string) {
		if p, ok := strings.CutPrefix(l, "@REPORT "); ok {
			buf.WriteString(p)
		}
	}}
	if err := cmdReport(o, nil); err != nil {
		httpFail(w, err)
		return
	}
	writeJSONResp(w, map[string]string{"name": filepath.Base(buf.String()), "path": buf.String()})
}

func (a *app) reportFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("name"))
	if !strings.HasPrefix(name, "report-") || !strings.HasSuffix(name, ".txt") {
		http.Error(w, "只能下载诊断报告", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="p2p-report-%s"`, strings.TrimPrefix(name, "report-")))
	http.ServeFile(w, r, filepath.Join(diagDir(), name))
}
