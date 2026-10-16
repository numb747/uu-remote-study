package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/apernet/quic-go"

	"uulab/third_party/hyscc/bbr"
	"uulab/third_party/hyscc/brutal"
)

// 拥塞控制（只影响发送方向，所以只在发送端设置）：
//
//	cubic         quic-go 默认。看到丢包就降速，5G 上的乱序会被当成丢包（docs/09）
//	bbr           Hysteria 2 带的 BBR：按测到的带宽和最小 RTT 发包，不靠丢包判断拥塞
//	brutal:<速率>  Hysteria 2 的 Brutal：按给定速率匀速发，丢了不降速，反而按丢包率多发补上。
//	              速率写成 45M / 6MB 这种：M = Mbit/s，MB = MByte/s
func applyCC(conn *quic.Conn, spec string) error {
	name, arg, _ := strings.Cut(spec, ":")
	switch name {
	case "", "cubic":
	case "bbr":
		size := min(conn.InitialPacketSize(), bbr.GetInitialPacketSize(conn.RemoteAddr()))
		conn.SetCongestionControl(bbr.NewBbrSender(bbr.DefaultClock{}, size, bbr.ProfileStandard))
	case "brutal":
		bps, err := parseRate(arg)
		if err != nil {
			return err
		}
		conn.SetCongestionControl(brutal.NewBrutalSender(bps, false))
	default:
		return fmt.Errorf("未知拥塞控制 %q（cubic | bbr | brutal:45M）", spec)
	}
	return nil
}

// parseRate 把 "45M" / "6MB" 解析成字节每秒。
func parseRate(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	mul := 1.0
	switch {
	case strings.HasSuffix(s, "MB"):
		s, mul = strings.TrimSuffix(s, "MB"), 1e6
	case strings.HasSuffix(s, "M"):
		s, mul = strings.TrimSuffix(s, "M"), 1e6/8
	default:
		return 0, fmt.Errorf("brutal 需要速率，如 brutal:45M（Mbit/s）或 brutal:6MB（MByte/s）")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("速率 %q 无效", s)
	}
	return uint64(v * mul), nil
}
