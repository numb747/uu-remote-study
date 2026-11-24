//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 开机自启（Linux）= systemd 用户服务 p2p-direct 是否 enable（apps/pc/install.sh 装的）。没装过这个服务就不支持
const autostartUnit = "p2p-direct.service"

func unitInstalled() bool {
	d, err := os.UserConfigDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(d, "systemd", "user", autostartUnit))
	return err == nil
}

func autostartState() (on, ok bool) {
	if !unitInstalled() {
		return false, false
	}
	out, _ := exec.Command("systemctl", "--user", "is-enabled", autostartUnit).Output()
	return strings.TrimSpace(string(out)) == "enabled", true
}

func setAutostart(on bool) error {
	if !unitInstalled() {
		return errors.New("没有装开机自启的服务：先运行 apps/pc/install.sh")
	}
	verb := "disable"
	if on {
		verb = "enable"
	}
	if out, err := exec.Command("systemctl", "--user", verb, autostartUnit).CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}
