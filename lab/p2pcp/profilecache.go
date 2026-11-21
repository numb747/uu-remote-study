package main

// 画像缓存（docs/arch/DESIGN.md 决策 1、待定里的缓存键）：同一个网络里 NAT 行为不会每分钟变，没必要每次都问 STUN。
//
// 键 = 网络指纹：本机出口 IP + main 表默认路由的网卡和网关 + STUN / 会合服务器清单。换了网络（出口 IP、网关变了）
// 自然对不上；手机 App 换网时本来就会重启 daemon。30 分钟过期。只缓存 IPv4 的部分，有没有 IPv6 每次现查（很便宜）。
// 文件 ~/.cache/p2pcp/profile.json（认 XDG_CACHE_HOME），按指纹分条存，最多 16 条；拿不到缓存目录（比如 adb shell 下没有 HOME）就不缓存。

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	profileTTL     = 30 * time.Minute
	profileEntries = 16
)

type cachedEntry struct {
	At      time.Time `json:"at"`
	Profile profile   `json:"profile"`
}

// cachedProfile 返回本机画像：缓存里有、没过期就用，否则现测并存起来（测不出来的不存，下次再测）
func cachedProfile(o opts) profile {
	v6 := !o.only4 && global6(o) != nil
	fp := netFingerprint(o)
	path := profileCachePath()
	cache := loadProfileCache(path)
	if e, ok := cache[fp]; ok && time.Since(e.At) < profileTTL && e.Profile.Class != "" {
		p := e.Profile
		p.V6, p.CachedAt, p.Took = v6, e.At, 0
		return p
	}
	p := natProfile(o)
	if p.Class != "" && path != "" {
		updateProfileCache(path, fp, cachedEntry{At: time.Now(), Profile: p})
	}
	return p
}

// updateProfileCache 在文件锁里重新读一遍、合并、写回：同一台机器上 daemon 和 send（或 netns 实验室的两端）
// 可能同时测完、同时写，各写各的会把对方那条盖掉
func updateProfileCache(path, fp string, e cachedEntry) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if lk, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer lk.Close()
		syscall.Flock(int(lk.Fd()), syscall.LOCK_EX) // 关文件时自动释放
	}
	m := loadProfileCache(path)
	m[fp] = e
	saveProfileCache(path, m)
}

// netFingerprint 不发包：出口 IP 由内核选路得到（对 UDP 做 connect 只查路由）
func netFingerprint(o opts) string {
	local := ""
	switch o.bind {
	case "", "auto":
		var ip net.IP
		if o.bind == "auto" {
			ip = defaultRouteIP()
		}
		if ip == nil {
			first, _, _ := strings.Cut(o.stun, ",")
			if a, err := net.ResolveUDPAddr("udp4", first); err == nil {
				if d, err := net.DialUDP("udp4", nil, a); err == nil {
					ip = d.LocalAddr().(*net.UDPAddr).IP
					d.Close()
				}
			}
		}
		local = ip.String()
	default:
		local = o.bind
	}
	ifc, gw := defaultRoute()
	return fmt.Sprintf("%s|%s|%s|%s|%s", local, ifc, gw, o.stun, o.server)
}

// defaultRoute 返回 main 表默认路由的网卡名和网关（/proc/net/route，读不到时为空）
func defaultRoute() (ifc, gw string) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", ""
	}
	bestMetric := 1 << 30
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		var metric int
		fmt.Sscan(f[6], &metric)
		if metric < bestMetric {
			ifc, gw, bestMetric = f[0], f[2], metric
		}
	}
	return
}

func profileCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "p2pcp", "profile.json")
}

func loadProfileCache(path string) map[string]cachedEntry {
	m := map[string]cachedEntry{}
	if path == "" {
		return m
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// saveProfileCache 去掉过期的，超过上限时扔掉最旧的；先写临时文件再改名，两个进程同时写也不会写出半个文件
func saveProfileCache(path string, m map[string]cachedEntry) {
	for k, e := range m {
		if time.Since(e.At) >= profileTTL {
			delete(m, k)
		}
	}
	for len(m) > profileEntries {
		oldest := ""
		for k, e := range m {
			if oldest == "" || e.At.Before(m[oldest].At) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}
