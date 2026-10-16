//go:build !unix

package main

import (
	"errors"
	"net"
)

func setTTL(c *net.UDPConn, ttl int) error {
	return errors.New("低 TTL 预热目前只实现了类 Unix 系统")
}
