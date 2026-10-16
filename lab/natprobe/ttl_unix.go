//go:build unix

package main

import (
	"net"
	"syscall"
)

// setTTL 设置 socket 发出包的 IPv4 TTL。为了只用标准库，按平台分文件实现。
func setTTL(c *net.UDPConn, ttl int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
	}); err != nil {
		return err
	}
	return serr
}
