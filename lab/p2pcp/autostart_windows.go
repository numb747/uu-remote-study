//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// 开机自启（Windows）= 「启动」文件夹里有没有「P2P 直传.lnk」（安装时建的，desktop_windows.go）
func startupLink() (string, error) {
	d, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "P2P 直传.lnk"), nil
}

func autostartState() (on, ok bool) {
	p, err := startupLink()
	if err != nil {
		return false, false
	}
	_, err = os.Stat(p)
	return err == nil, true
}

func setAutostart(on bool) error {
	p, err := startupLink()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	script := fmt.Sprintf(`$l = (New-Object -ComObject WScript.Shell).CreateShortcut(%s)
$l.TargetPath = %s; $l.Arguments = 'app -no-browser'; $l.WorkingDirectory = %s
$l.Description = 'P2P 直传：开机后在后台接收好友发来的文件'
if (Test-Path %s) { $l.IconLocation = %s }
$l.Save()`, psQuote(p), psQuote(exe), psQuote(filepath.Dir(exe)), psQuote(filepath.Join(filepath.Dir(exe), "P2P直传.ico")), psQuote(filepath.Join(filepath.Dir(exe), "P2P直传.ico")))
	if out, err := powershell(script); err != nil {
		return fmt.Errorf("建启动项：%v %s", err, out)
	}
	return nil
}
