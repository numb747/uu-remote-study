package main

// 诊断数据上报（docs/arch/DESIGN.md 决策 22）：每次连接结束（成功、走中继、失败都算），把那条结果记录发给会合服务器，
// 服务器追加进一个 jsonl（lab/punch 的 -reports），lab/real/report-stats.py 按 NAT 组合、原因码汇总，用来改进。
//
//	→ 会合服务器  /H <json>          ← /HK <json 的 sha256 前 8 位十六进制>
//
// 走现有的 UDP 会合端口，一个包装得下（几百字节）。没收到 /HK 就存进本机队列 upload-queue.jsonl（最多 100 条），
// 下次连接结束时补发。上次失败后 5 分钟内不再试，直接进队列：服务器是旧版（不认识 /H）、本机断网时，
// 别让每次连接结束都多等一秒多。
//
// 发什么：histRec（两端 NAT 画像含公网映射、预测、实际结果、原因码、各段耗时、第几轮、升级、版本、系统），
// 加上设备 ID 的哈希（dev）和连接号（conn，两端算出来一样）。不发文件名、好友名、任何密钥：err 文字可能带好友名，
// 只在原因码是 unknown 时带上、并且打码（diag.go 的 redactor）。默认开，-upload off / App 里的开关关掉（settings.json）。

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	uploadQueueMax = 100
	uploadPause    = 5 * time.Minute
)

// upRec 是上报的一条：结果记录 + 区分设备、对上两端用的两个哈希
type upRec struct {
	histRec
	Dev string `json:"dev,omitempty"`
}

func uploadQueuePath() string { return filepath.Join(diagDir(), "upload-queue.jsonl") }

// devHash：设备 ID 的哈希。服务器那边能区分「同一台设备的几次连接」，但对不上通讯录里的设备 ID
func devHash(cfg string) string {
	path := filepath.Join(cfg, "identity.json")
	if _, err := os.Stat(path); err != nil {
		return "" // 没有身份（-room 模式没用过通讯录）：不为上报去生成一个
	}
	id, err := loadIdentity(cfg)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(append([]byte("p2pcp dev "), id.pub()...))
	return hex.EncodeToString(h[:6])
}

// connHash：连接号。两端的会合 id 排个序再哈希，两边算出来一样，统计时能把同一次连接的两条记录对上
func connHash(a, b string) string {
	if a > b {
		a, b = b, a
	}
	h := sha256.Sum256([]byte("p2pcp conn " + a + " " + b))
	return hex.EncodeToString(h[:6])
}

// upload 在 settle 里调用：发这一条，顺带补发队列；发不出去就进队列
func upload(cfg, servers string, rec histRec) {
	if servers == "" || !loadSettings(cfg).uploadOn() {
		return
	}
	if rec.Reason != rsnUnknown {
		rec.Err = "" // 可能带好友名；原因码已经够分类了
	} else {
		rec.Err = newRedactor(cfg).apply(rec.Err)
	}
	b, err := json.Marshal(upRec{histRec: rec, Dev: devHash(cfg)})
	if err != nil {
		return
	}
	dir := diagDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	q, cur := uploadQueuePath(), string(b)
	// 文件锁只包住读写队列，不包住网络往返：常驻接收同时收好几个好友时，各连接的上报别互相排队等
	queued := func(lines ...string) {
		defer lockFile(q + ".lock")()
		writeQueue(q, append(readQueue(q), lines...))
	}
	failMark := filepath.Join(dir, "upload-failed")
	if st, err := os.Stat(failMark); err == nil && time.Since(st.ModTime()) < uploadPause {
		queued(cur)
		return
	}
	// 新的这条先发，再补发队列里的旧记录：先把队列领走（清空），发完把没发出去的放回去，排在这期间别人新加的前面。
	// 一次最多补 20 条（服务器按 IP 限速，每秒 20 个包），剩下的下次再补
	srv := strings.Split(servers, ",")[0]
	if !sendReport(srv, cur) {
		os.WriteFile(failMark, nil, 0o600)
		queued(cur)
		return
	}
	os.Remove(failMark)
	var old []string
	func() {
		defer lockFile(q + ".lock")()
		old = readQueue(q)
		writeQueue(q, nil)
	}()
	sent := 0
	for sent < len(old) && sent < 20 && sendReport(srv, old[sent]) {
		sent++
	}
	if sent < len(old) && sent < 20 {
		os.WriteFile(failMark, nil, 0o600)
	}
	if sent < len(old) {
		func() {
			defer lockFile(q + ".lock")()
			writeQueue(q, append(old[sent:], readQueue(q)...))
		}()
	}
}

// sendReport 发一条，等 /HK（每次 600ms，试两次）
func sendReport(server, line string) bool {
	addr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return false
	}
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return false
	}
	defer c.Close()
	sum := sha256.Sum256([]byte(line))
	want := "/HK " + hex.EncodeToString(sum[:4])
	buf := make([]byte, 256)
	for range 2 {
		c.WriteToUDP([]byte("/H "+line), addr)
		c.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if from.Port == addr.Port && from.IP.Equal(addr.IP) && string(buf[:n]) == want {
				return true
			}
		}
	}
	return false
}

func readQueue(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// writeQueue 只留最新的 uploadQueueMax 条
func writeQueue(path string, lines []string) {
	if len(lines) > uploadQueueMax {
		lines = lines[len(lines)-uploadQueueMax:]
	}
	if len(lines) == 0 {
		os.Remove(path)
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600) == nil {
		os.Rename(tmp, path)
	}
}
