//go:build windows

package main

import "os"

// reraise（Windows）：没有「按原信号退出」这回事，直接退出
func reraise(os.Signal) { os.Exit(1) }
