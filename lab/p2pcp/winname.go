package main

import (
	"runtime"
	"strings"
)

// localName：收到的文件名在本机能不能用。Windows 上不能有 \/:*?"<>| 和控制字符（冒号还会写进「备用数据流」），
// 结尾不能是点或空格，CON、NUL、COM1 这类设备名（带不带扩展名都算）会写进设备而不是文件。换成全角或加下划线，
// 名字看起来还差不多。别的系统原样返回（Linux、安卓上冒号是合法的）
func localName(name string) string {
	if runtime.GOOS != "windows" {
		return name
	}
	return winSafeName(name)
}

func winSafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20:
			return '_'
		case strings.ContainsRune(`\/:*?"<>|`, r):
			return r + 0xFEE0 // 全角：＼／：＊？＂＜＞｜
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "_"
	}
	stem, _, _ := strings.Cut(name, ".")
	switch strings.ToUpper(strings.TrimRight(stem, " ")) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "COM¹", "COM²", "COM³",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "LPT¹", "LPT²", "LPT³":
		name = "_" + name
	}
	return name
}
