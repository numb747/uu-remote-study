//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reraise 按原信号退出：systemd、App（Process.destroy）看到的退出方式不变
func reraise(s os.Signal) { syscall.Kill(os.Getpid(), s.(syscall.Signal)) }
