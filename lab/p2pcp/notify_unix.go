//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// osNotify（Linux）：notify-send 弹一条，replace 不空时原地更新那一条；返回这条的 ID
func osNotify(title, body string, pct int, replace string) (string, bool) {
	args := []string{"-a", "P2P 直传", "-i", "network-transmit-receive", "-p"}
	if replace != "" {
		args = append(args, "-r", replace)
	}
	if pct >= 0 {
		args = append(args, "-h", fmt.Sprintf("int:value:%d", pct), "-u", "low")
	}
	out, err := exec.Command("notify-send", append(args, title, body)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
