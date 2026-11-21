//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockFile 拿 path 的进程间排他锁（flock，关文件时自动释放），返回释放函数。拿不到锁文件就不锁（缓存类的写入宁可偶尔冲突）
func lockFile(path string) (unlock func()) {
	lk, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	syscall.Flock(int(lk.Fd()), syscall.LOCK_EX)
	return func() { lk.Close() }
}
