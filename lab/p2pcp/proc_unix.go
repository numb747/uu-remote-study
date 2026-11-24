//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// startDetached 在后台另起一个自己（新会话，不跟着当前终端退出）
func startDetached(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// defaultOut：桌面版的接收目录 ~/Downloads/P2P直传
func defaultOut() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads", "P2P直传")
}
