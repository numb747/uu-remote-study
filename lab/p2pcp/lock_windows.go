//go:build windows

package main

import (
	"os"
	"time"
)

// lockFile（Windows）：用「独占创建锁文件」代替 flock。持锁的进程崩了会留下锁文件，超过 10 秒就当它作废
func lockFile(path string) (unlock func()) {
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(20 * time.Millisecond) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }
		}
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) > 10*time.Second {
			os.Remove(path)
		}
	}
	return func() {}
}
