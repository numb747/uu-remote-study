package main

// 右键发送（Windows「发送到」，docs/arch/DESIGN.md 决策 38）：p2pcp sendto 文件…
// 不自己发：把文件路径交给正在跑的 p2pcp app（POST /api/pick），再打开网页让用户选发给谁。
// 好处是文件不用复制（网页选的文件要先经 HTTP 传一遍），进度、取消、记录都在网页上，和别的发送一样。
// app 没在跑（刚装好、被关了）就先在后台把它拉起来。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
)

func cmdSendto(o opts, files []string) error {
	if len(files) == 0 {
		return errors.New("用法：p2pcp sendto 文件…")
	}
	var paths []string
	for _, f := range files {
		p, err := filepath.Abs(f)
		if err != nil {
			return withReason(rsnLocal, err)
		}
		paths = append(paths, p)
	}
	cfg := configDir(o)
	a, ok := runningApp(cfg)
	if !ok {
		args := []string{"app", "-no-browser"}
		if o.config != "" {
			args = append(args, "-config", o.config)
		}
		if err := startDetached(args...); err != nil {
			return withReason(rsnLocal, fmt.Errorf("启动 P2P 直传：%w", err))
		}
		for start := time.Now(); !ok && time.Since(start) < 15*time.Second; {
			time.Sleep(300 * time.Millisecond)
			a, ok = runningApp(cfg)
		}
		if !ok {
			return withReason(rsnLocal, errors.New("P2P 直传 15 秒内没启动起来，看看日志"))
		}
	}
	var resp struct{ ID string }
	if err := a.post("/api/pick", map[string]any{"paths": paths}, &resp); err != nil {
		return err
	}
	openBrowser(a.url())
	return nil
}

// post 以「本机读得到令牌的程序」的身份调 app 的接口（带上 cookie 和 Origin，和网页一样过 guard）
func (a appInfo) post(path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d%s", a.Port, path), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", a.Port))
	req.AddCookie(&http.Cookie{Name: appCookie, Value: a.Token})
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("P2P 直传回了 %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
