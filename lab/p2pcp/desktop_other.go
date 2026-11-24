//go:build !windows

package main

import "errors"

// Windows 才有的安装 / 卸载（desktop_windows.go）；Linux 用 apps/pc/install.sh
func cmdLaunch(o opts) error {
	return errors.New("双击安装只有 Windows 版有；Linux 用 apps/pc/install.sh")
}
func cmdUninstall(o opts) error {
	return errors.New("只有 Windows 版有；Linux 用 apps/pc/install.sh uninstall")
}

// openUI：别的系统用浏览器打开网页界面（Windows 是 WebView2 窗口，window_windows.go）
func openUI(url string) { openBrowser(url) }

func cmdWindow(url string) error { openBrowser(url); return nil }
