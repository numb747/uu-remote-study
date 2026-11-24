package main

// 传文件。一次会合只打一个洞，同一个 socket 上开 N 条 QUIC 连接（各有各的拥塞控制），多个文件依次在这些连接上传。
// 第 0 条连接上有一条控制流（认证流之后的第一条双向流），一行一个 JSON：
//
//	发送端 → {"t":"offer","name","size","fid"}       fid = 文件指纹（名字 + 大小 + 开头和结尾各 1MB 的哈希）
//	接收端 → {"t":"have","ranges":[[off,len],...]}   上次断掉时已经收到的区间（断点续传），没有就不带
//	发送端在各条连接上并行发缺的部分：每块（≤ 4MB）一条单向流，流头 "P2PCP2" | off u64 | len u64。
//	       接收端写进 .p2pcp-<fid>.part 的对应位置，收到的区间记在旁边的 .p2pcp-<fid>.json，断了下次从这里接着传
//	发送端 → {"t":"end","sha256"}                    整个文件的 sha256（边发边算）
//	接收端 → {"t":"ok"} / {"t":"bad","err"}          块收齐、校验通过才改名成正式文件
//	……下一个文件…… 最后发送端 → {"t":"bye"}
//
// 打通之后传输中途断了（换网、对方 App 被杀、对方重启），发送端自己重连，已经收到的部分不重传。
// 走中继时后台继续打洞，打通了就关掉中继会话、在直连上重开一个会话接着传（upgrade.go）。
// 块由各条连接抢着发：快的连接多发，慢的少发，不会像按连接数均分那样被最慢的一条拖住。

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
)

const pieceSize = 4 << 20

type ctlMsg struct {
	T      string      `json:"t"`
	Name   string      `json:"name,omitempty"`
	Size   uint64      `json:"size,omitempty"`
	FID    string      `json:"fid,omitempty"`
	Ranges [][2]uint64 `json:"ranges,omitempty"` // [off, len]
	SHA256 string      `json:"sha256,omitempty"`
	Err    string      `json:"err,omitempty"`
	SID    string      `json:"sid,omitempty"` // offer 带：发送号（cancel.go）；接收端回 {"t":"cancel"} 表示这次发送已被它取消
}

var errNotRegular = errors.New("不是普通文件")

// interrupted：打通之后才出的错，重连续传有意义
type interrupted struct{ error }

func (e interrupted) Unwrap() error { return e.error }

// meter 统计已传字节，每 500ms 报一次进度。
type meter struct {
	n atomic.Uint64
}

func (m *meter) run(rep *reporter, dir string, total uint64, name string) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		last := m.n.Load() // 续传时从已有的字节数起算，速度不会虚高
		for {
			select {
			case <-done:
				return
			case <-t.C:
				g := m.n.Load()
				if g > 0 {
					rep.progress(dir, g, total, float64(g-last)*8/0.5/1e6, name)
				}
				last = g
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

type countingReader struct {
	r io.Reader
	m *meter
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.m.n.Add(uint64(n))
	return n, err
}

// ---------------- send ----------------

// send 发一组文件。打通之后断了就重连续传；连续 5 次重连都没有任何进展才放弃
func send(o opts, paths []string) error {
	for _, p := range paths {
		noteFile(p) // 诊断报告里打码用（diag.go）；出错的日志里也有文件名，所以最先登记
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil {
			return err
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s %w", p, errNotRegular)
		}
	}
	if o.x == nil {
		o.x = newXfer("send", "send", o.friend)
		defer o.x.end()
	}
	o.sid = randHex(8)
	o.quit = o.x.done // 取消时还在会合 / 打洞的，马上不等了
	files := append([]string(nil), paths...)
	backoff, fails := 2*time.Second, 0
	var next *link // 中继会话期间打通的直连：下一个会话直接用它
	for {
		progressed, up, err := sendSession(o, &files, next)
		next = nil
		if o.x.canceled() { // 本机取消：不管这一轮是怎么结束的
			if up != nil {
				up.closeWith(codeCanceled, "canceled")
			}
			return canceledErr{}
		}
		if err == nil {
			return nil
		}
		if isCanceled(err) || peerCanceled(err) {
			if up != nil {
				up.close()
			}
			return canceledErr{peer: true}
		}
		if up != nil {
			next, backoff, fails = up, 2*time.Second, 0
			continue
		}
		if progressed {
			backoff, fails = 2*time.Second, 0
		}
		fails++
		if !errors.As(err, new(interrupted)) || fails >= 5 {
			return err
		}
		o.rep.state(fmt.Sprintf("连接断了（%v），%v 后重连续传", err, backoff))
		select {
		case <-time.After(backoff):
		case <-o.x.done:
			return canceledErr{}
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// sendSession 打一次洞（l 非空时直接用它），把 files 依次发完；发完一个就从 files 里去掉，重连时从没发完的接着来。
// 走中继时后台在打洞，打通了就关掉中继、返回那条直连（up），send 在它上面重开会话
func sendSession(o opts, files *[]string, l *link) (progressed bool, up *link, err error) {
	var total int64
	for _, p := range *files {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	n := max(o.conns, 1)
	if total < 1<<20 {
		n = 1 // 小文件多开连接没用
	}
	if l == nil {
		if len(*files) == 1 {
			o.rep.state(fmt.Sprintf("连接对方：%s（%.1fMB）", filepath.Base((*files)[0]), float64(total)/1e6))
		} else {
			o.rep.state(fmt.Sprintf("连接对方：%d 个文件（%.1fMB）", len(*files), float64(total)/1e6))
		}
		if l, err = dialPeer(o, alpn); err != nil {
			return false, nil, err
		}
	}
	defer l.close()
	defer o.x.watch(l)()
	u := startUpgrade(o, l)
	defer func() {
		// defer 后进先出，在 close 之前跑：体检的结论在传输结束时补齐（preflight.go）
		out := l.outcome()
		u.annotate(&out)
		if err != nil && up == nil { // 切到了直连（up）不算出错
			out.err, out.reason = err.Error(), reasonOf(err)
			if o.x.canceled() { // 本机取消：流上看到的是自己关连接的错，别记成断线
				out.reason = rsnCanceled
			}
			l.pre.tracef("会话出错：%v", err)
		}
		l.pre.settle(out)
	}()
	defer func() {
		if up = u.stop(); up != nil && err == nil {
			up.close() // 切过去时其实已经传完了（走不到这里：最后一个文件发出 end 后就不切了）
			up = nil
		}
	}()
	logLinked(l)
	o.rep.state("已连上 " + l.via())
	if l.relayed {
		n = 1 // 中继只有 3M，多开连接只会互相抢
	}
	conns := []*quic.Conn{l.conn}
	for i := 1; i < n; i++ {
		c, err := l.dial()
		if err != nil {
			log.Printf("第 %d 条连接没开成（%v），用 %d 条传", i, err, len(conns))
			break
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if err := applyCC(c, o.cc); err != nil {
			return false, nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ctl, err := l.conn.OpenStreamSync(ctx)
	cancel()
	if err != nil {
		return false, nil, interrupted{err}
	}
	enc, dec := json.NewEncoder(ctl), json.NewDecoder(ctl)
	for len(*files) > 0 {
		moved, err := sendFile(o, (*files)[0], conns, enc, dec, u, len(*files) == 1)
		progressed = progressed || moved
		if err != nil {
			return progressed, nil, err
		}
		*files = (*files)[1:]
		progressed = true
	}
	enc.Encode(ctlMsg{T: "bye"})
	ctl.Close()
	select { // 等接收端关连接，最后的确认都送到了
	case <-l.conn.Context().Done():
	case <-time.After(2 * time.Second):
	}
	return progressed, nil, nil
}

// sendFile 发一个文件。u 非空时（走中继）end → ok 之间拿着它的锁，这期间不切到直连；last = 这是最后一个文件
func sendFile(o opts, path string, conns []*quic.Conn, enc *json.Encoder, dec *json.Decoder, u *upgrader, last bool) (moved bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := uint64(fi.Size())
	fid, err := fileID(f, fi.Name(), size)
	if err != nil {
		return false, err
	}
	if err := enc.Encode(ctlMsg{T: "offer", Name: fi.Name(), Size: size, FID: fid, SID: o.sid}); err != nil {
		return false, interrupted{err}
	}
	var have ctlMsg
	if err := dec.Decode(&have); err != nil {
		return false, interrupted{fmt.Errorf("等对方回应: %w", err)}
	}
	if have.T == "bad" {
		return false, fmt.Errorf("对方拒收 %s：%s", fi.Name(), have.Err)
	}
	if have.T == "cancel" { // 对方已经取消了这次发送（它的 CONNECTION_CLOSE 我们没收到，cancel.go）
		return false, canceledErr{peer: true}
	}
	missing := complement(have.Ranges, size)
	var haveBytes uint64 = size
	for _, r := range missing {
		haveBytes -= r[1]
	}
	if haveBytes > 0 {
		o.rep.state(fmt.Sprintf("续传 %s：对方已有 %.1f / %.1fMB", fi.Name(), float64(haveBytes)/1e6, float64(size)/1e6))
	} else {
		o.rep.state(fmt.Sprintf("发送 %s（%.1fMB，%d 条连接，拥塞控制 %s）", fi.Name(), float64(size)/1e6, len(conns), o.cc))
	}

	// 边发边算整个文件的 sha256：另开一个文件句柄顺序读，和发送共用页缓存。发送中途出错（取消、断线）就不算了，
	// 否则 2.5GB 的文件取消后还要在后台读完一遍
	sum := make(chan string, 1)
	abort := make(chan struct{})
	go func() {
		g, err := os.Open(path)
		if err != nil {
			sum <- ""
			return
		}
		defer g.Close()
		h := sha256.New()
		if _, err := io.Copy(h, abortReader{g, abort}); err != nil {
			sum <- ""
			return
		}
		sum <- hex.EncodeToString(h.Sum(nil))
	}()

	var list [][2]uint64
	for _, r := range missing {
		for off := r[0]; off < r[0]+r[1]; off += pieceSize {
			list = append(list, [2]uint64{off, min(pieceSize, r[0]+r[1]-off)})
		}
	}
	pieces := make(chan [2]uint64, len(list))
	for _, p := range list {
		pieces <- p
	}
	close(pieces)
	var m meter
	m.n.Store(haveBytes)
	stop := m.run(o.rep, "send", size, fi.Name())
	start := time.Now()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		quit  = make(chan struct{})
	)
	for _, c := range conns {
		wg.Go(func() {
			for p := range pieces {
				select {
				case <-quit:
					return
				default:
				}
				if err := sendPiece(c, f, p, &m); err != nil {
					once.Do(func() { first = err; close(quit) })
					return
				}
			}
		})
	}
	wg.Wait()
	stop()
	sent := m.n.Load() - haveBytes
	if first != nil {
		close(abort)
		return sent > 0, interrupted{first}
	}
	digest := <-sum
	u.enter()
	if err := enc.Encode(ctlMsg{T: "end", SHA256: digest}); err != nil {
		u.leave(false)
		return true, interrupted{err}
	}
	var res ctlMsg
	err = dec.Decode(&res)
	u.leave(last)
	if err != nil {
		return true, interrupted{fmt.Errorf("等对方确认: %w", err)}
	}
	if res.T != "ok" {
		return true, fmt.Errorf("对方校验 %s 失败：%s", fi.Name(), res.Err)
	}
	el := time.Since(start)
	for i, c := range conns {
		log.Printf("[%d] %s", i, statsLine(c))
	}
	o.rep.done("send", size, sent, el, fi.Name())
	return true, nil
}

// abortReader：abort 关闭后再读就出错
type abortReader struct {
	r     io.Reader
	abort <-chan struct{}
}

var errAborted = errors.New("不读了")

func (a abortReader) Read(p []byte) (int, error) {
	select {
	case <-a.abort:
		return 0, errAborted
	default:
	}
	return a.r.Read(p)
}

func sendPiece(c *quic.Conn, f *os.File, p [2]uint64, m *meter) error {
	str, err := c.OpenUniStreamSync(context.Background())
	if err != nil {
		return err
	}
	hdr := make([]byte, 0, 22)
	hdr = append(hdr, "P2PCP2"...)
	hdr = binary.BigEndian.AppendUint64(hdr, p[0])
	hdr = binary.BigEndian.AppendUint64(hdr, p[1])
	if _, err := str.Write(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(str, countingReader{io.NewSectionReader(f, int64(p[0]), int64(p[1])), m}); err != nil {
		return err
	}
	return str.Close()
}

// fileID：同一个文件重发时指纹不变，断点续传才找得到半成品。不用修改时间：手机 App 每次都把文件复制到缓存再发，
// 修改时间每次都不同。只哈希开头和结尾各 1MB 是为了快；万一中间不同而指纹撞上，最后的整文件 sha256 会发现
func fileID(f *os.File, name string, size uint64) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", name, size)
	const sample = 1 << 20
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, sample)); err != nil {
		return "", err
	}
	if size > sample {
		if _, err := io.Copy(h, io.NewSectionReader(f, int64(size)-sample, sample)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// complement 返回 [0,size) 里不在 have 中的区间
func complement(have [][2]uint64, size uint64) [][2]uint64 {
	var out [][2]uint64
	pos := uint64(0)
	for _, r := range normalize(have, size) {
		if r[0] > pos {
			out = append(out, [2]uint64{pos, r[0] - pos})
		}
		pos = max(pos, r[0]+r[1])
	}
	if pos < size {
		out = append(out, [2]uint64{pos, size - pos})
	}
	return out
}

// normalize 排序、合并、裁到 [0,size)
func normalize(rs [][2]uint64, size uint64) [][2]uint64 {
	var v [][2]uint64
	for _, r := range rs {
		if r[0] < size && r[1] > 0 {
			v = append(v, [2]uint64{r[0], min(r[1], size-r[0])})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i][0] < v[j][0] })
	var out [][2]uint64
	for _, r := range v {
		if n := len(out); n > 0 && r[0] <= out[n-1][0]+out[n-1][1] {
			out[n-1][1] = max(out[n-1][0]+out[n-1][1], r[0]+r[1]) - out[n-1][0]
			continue
		}
		out = append(out, r)
	}
	return out
}

// ---------------- recv ----------------

// recv 接一次（一次连接里对方发几个文件就收几个）
func recv(o opts) error {
	cleanStale(o.out)
	l, err := acceptPeer(o, alpn)
	if err != nil {
		return err
	}
	return recvSession(o, l)
}

// daemon 常驻接收，可以同时接好几个发送端
func daemon(o opts) {
	cleanStale(o.out)
	keepServing(o, "等待对方发送", func(o opts) (*link, error) { return acceptPeer(o, alpn) }, func(l *link) {
		if err := recvSession(o, l); err != nil {
			o.rep.fail(err)
		}
	})
}

// inFile 是正在接收的文件
type inFile struct {
	name, fid  string
	size       uint64
	part, meta string
	f          *os.File
	m          meter
	sealed     atomic.Bool // 收齐了、开始校验：迟到的块不能再写进来

	mu       sync.Mutex
	ranges   [][2]uint64
	have     uint64
	lastSave time.Time
	changed  chan struct{}
}

type partMeta struct {
	Name   string      `json:"name"`
	Size   uint64      `json:"size"`
	Ranges [][2]uint64 `json:"ranges"`
}

func (f *inFile) add(off, n uint64) {
	if n == 0 {
		return
	}
	f.mu.Lock()
	f.ranges = normalize(append(f.ranges, [2]uint64{off, n}), f.size)
	f.have = 0
	for _, r := range f.ranges {
		f.have += r[1]
	}
	if time.Since(f.lastSave) > 500*time.Millisecond || f.have == f.size {
		f.saveLocked()
	}
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func (f *inFile) complete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have == f.size
}

func (f *inFile) save() {
	f.mu.Lock()
	f.saveLocked()
	f.mu.Unlock()
}

// saveLocked 原子地写下已收区间（先写临时文件再改名），进程在任何时候被杀都不会留下写了一半的记录
func (f *inFile) saveLocked() {
	if f.sealed.Load() { // 收齐了（改名后会删记录）或者取消了（半成品已删）：迟到的块别再把记录写回来
		return
	}
	f.lastSave = time.Now()
	b, _ := json.Marshal(partMeta{Name: f.name, Size: f.size, Ranges: f.ranges})
	tmp := f.meta + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, f.meta)
	}
}

// fidLocks：同一个文件同时只能有一个会话在收（对方换网重连时，旧会话可能还没超时）。带引用计数，没人用就删掉，
// 常驻 daemon 收过的文件不会一直留在表里
var (
	fidMu    sync.Mutex
	fidLocks = map[string]*fidLock{}
)

type fidLock struct {
	sync.Mutex
	refs int
}

func lockFID(fid string) (unlock func()) {
	fidMu.Lock()
	l := fidLocks[fid]
	if l == nil {
		l = &fidLock{}
		fidLocks[fid] = l
	}
	l.refs++
	fidMu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		fidMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(fidLocks, fid)
		}
		fidMu.Unlock()
	}
}

// recvSession 处理一个发送端。走中继时对方可能在后台打通直连、关掉中继会话，在直连上重开会话（upgrade.go）：
// 那就在新的链接上接着收，同一个文件照常续传
func recvSession(o opts, l *link) error {
	o.x = newXfer("recv:"+o.friend, "recv", o.friend)
	defer o.x.end()
	for {
		next, err := recvOne(o, l)
		if isCanceled(err) {
			if next != nil {
				next.close()
			}
			return err
		}
		if o.x.canceled() {
			if next != nil {
				next.closeWith(codeCanceled, "canceled")
			}
			return canceledErr{}
		}
		if peerCanceled(err) {
			return canceledErr{peer: true}
		}
		if next == nil {
			return err
		}
		l = next
	}
}

// recvOne 在一个链接上收：除了第一条连接，对方还会在同一个洞上开别的连接，都在后台接着；
// 每条连接上的单向流是数据块，写进当前文件。走中继时同时挂在升级房间里等（waitUpgrade），
// 中继会话断开时对方已经在直连上连过来了，就返回那条直连
func recvOne(o opts, l *link) (next *link, err error) {
	stop := make(chan struct{})
	up := waitUpgrade(o, l, stop)
	t0 := time.Now()
	defer func() { // 体检的结论在传输结束时补齐（preflight.go）
		out := l.outcome()
		if next != nil {
			out.upgraded, out.upgradedVia = time.Since(t0), next.peer.String()
		} else if err != nil {
			out.err, out.reason = err.Error(), reasonOf(err)
			if o.x.canceled() {
				out.reason = rsnCanceled
			}
			l.pre.tracef("会话出错：%v", err)
		}
		l.pre.settle(out)
	}()
	defer l.close()
	defer close(stop)
	defer o.x.watch(l)()
	if err = serveLink(o, l); err != nil && l.relayed && !o.x.canceled() && !peerCanceled(err) {
		// 发送端是先接上直连、再关中继的，所以这边中继一断，直连多半已经在等着了；留几秒给最后的认证
		select {
		case next = <-up:
			log.Printf("中继会话断开（%v）：对方已在直连 %s 上重开会话，接着收", err, next.via())
			return next, nil
		case <-time.After(3 * time.Second):
		}
	}
	return nil, err
}

func serveLink(o opts, l *link) error {
	logLinked(l)
	o.rep.state("已连上 " + l.via())
	var cur atomic.Pointer[inFile]
	dataLoop := func(c *quic.Conn) {
		for {
			str, err := c.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go recvPiece(str, cur.Load())
		}
	}
	go dataLoop(l.conn)
	go func() {
		for !l.isClosed() {
			c, err := l.accept(0)
			if err != nil {
				// 认证失败（路上有人知道房间号、往打通的洞里塞连接）不能让这个循环退出，否则发送端后面的连接就没人接了
				if !l.isClosed() {
					log.Printf("拒绝一条额外连接：%v", err)
				}
				continue
			}
			go dataLoop(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	ctl, err := l.conn.AcceptStream(ctx)
	cancel()
	if err != nil {
		return interrupted{fmt.Errorf("等对方的控制流: %w", err)}
	}
	enc, dec := json.NewEncoder(ctl), json.NewDecoder(ctl)
	for {
		var m ctlMsg
		if err := dec.Decode(&m); err != nil {
			return interrupted{fmt.Errorf("连接断了（%w），对方重连后会续传", err)}
		}
		switch m.T {
		case "bye":
			return nil
		case "offer":
			if err := recvFile(o, l, m, &cur, enc, dec); err != nil {
				return err
			}
		}
	}
}

func recvFile(o opts, l *link, offer ctlMsg, cur *atomic.Pointer[inFile], enc *json.Encoder, dec *json.Decoder) (err error) {
	// 不信任对方给的路径。Windows 上先换掉不能用的字符（winname.go）：斜杠、冒号变全角，「w:x」不会被当成盘符截掉
	name := filepath.Base(localName(offer.Name))
	noteName("f", offer.Name) // 不合法的名字也会进错误文字：先登记，诊断报告、上报里才能打码
	if name == "." || name == ".." || name == "/" || strings.HasPrefix(name, ".p2pcp-") || len(offer.FID) != 32 || strings.Trim(offer.FID, "0123456789abcdef") != "" {
		enc.Encode(ctlMsg{T: "bad", Err: "文件名或指纹不合法"})
		return fmt.Errorf("对方发来的文件名或指纹不合法：%q %q", offer.Name, offer.FID)
	}
	noteName("f", name)
	if wasCanceled(offer.SID) { // 这次发送刚被本机取消过，对方没收到、当成中断重连过来了
		enc.Encode(ctlMsg{T: "cancel"})
		l.closeWith(codeCanceled, "canceled") // 光回 cancel 不够：紧接着关连接，没发出去的流数据会被丢掉
		return canceledErr{}
	}
	o.x.setSID(offer.SID)
	defer lockFID(offer.FID)()

	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}
	f := &inFile{
		name: name, fid: offer.FID, size: offer.Size,
		part:    filepath.Join(o.out, ".p2pcp-"+offer.FID+".part"),
		meta:    filepath.Join(o.out, ".p2pcp-"+offer.FID+".json"),
		changed: make(chan struct{}, 1),
	}
	// 有上次留下的半成品就接着用
	if b, err := os.ReadFile(f.meta); err == nil {
		var pm partMeta
		if json.Unmarshal(b, &pm) == nil && pm.Size == f.size {
			if st, err := os.Stat(f.part); err == nil && uint64(st.Size()) == f.size {
				f.ranges = normalize(pm.Ranges, f.size)
			}
		}
	}
	if f.f, err = os.OpenFile(f.part, os.O_RDWR|os.O_CREATE, 0o644); err != nil {
		return err
	}
	defer f.f.Close()
	defer func() { // 取消（哪一边都算）：半成品删掉，不留着续传（cancel.go）
		if err == nil {
			return
		}
		switch {
		case o.x.canceled():
			err = canceledErr{}
		case peerCanceled(err) || peerCanceled(context.Cause(l.conn.Context())):
			err = canceledErr{peer: true} // 下面几处错误是用 %v 拼的，原来的 ApplicationError 包不进去
		}
		if isCanceled(err) {
			f.mu.Lock()          // 和 saveLocked 互斥：别让还在路上的块在删掉之后又把记录写回来
			f.sealed.Store(true) // 还在写的块别再写
			f.f.Close()
			os.Remove(f.part)
			os.Remove(f.meta)
			f.mu.Unlock()
			log.Printf("%s 已取消，删掉收了一半的文件", name)
		}
	}()
	if f.ranges == nil {
		if err := f.f.Truncate(int64(f.size)); err != nil {
			return err
		}
	}
	for _, r := range f.ranges {
		f.have += r[1]
	}
	startHave := f.have
	f.save()
	cur.Store(f)
	defer cur.Store(nil)
	if err := enc.Encode(ctlMsg{T: "have", Ranges: f.ranges}); err != nil {
		return err
	}
	if startHave > 0 {
		o.rep.state(fmt.Sprintf("续传 %s：已有 %.1f / %.1fMB", name, float64(startHave)/1e6, float64(f.size)/1e6))
	} else {
		o.rep.state(fmt.Sprintf("接收 %s（%.1fMB）", name, float64(f.size)/1e6))
	}
	f.m.n.Store(startHave)
	stop := f.m.run(o.rep, "recv", f.size, name)
	defer stop()
	start := time.Now()

	var end ctlMsg
	if err := dec.Decode(&end); err != nil || end.T != "end" {
		f.save()
		return interrupted{fmt.Errorf("%s 传到 %.1f / %.1fMB 时连接断了（%v），对方重连后会续传", name, float64(f.m.n.Load())/1e6, float64(f.size)/1e6, err)}
	}
	for !f.complete() {
		select {
		case <-f.changed:
		case <-l.conn.Context().Done():
			f.save()
			return interrupted{fmt.Errorf("%s 没收齐连接就断了，对方重连后会续传", name)}
		}
	}
	stop()
	el := time.Since(start)
	f.sealed.Store(true)
	cur.Store(nil)

	// 整个文件再读一遍算 sha256，和发送端的对上才算数
	if _, err := f.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f.f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != end.SHA256 {
		f.f.Close()
		os.Remove(f.part)
		os.Remove(f.meta)
		enc.Encode(ctlMsg{T: "bad", Err: "sha256 不一致"})
		return fmt.Errorf("%s 的 sha256 对不上（收到 %s，应为 %s），已删除，重发一次", name, got[:12], end.SHA256[:min(12, len(end.SHA256))])
	}
	out := uniquePath(o.out, name)
	noteFile(out) // 诊断报告里打码用（diag.go）
	f.f.Close()   // Windows 上开着的文件改不了名
	if err := renameRetry(f.part, out); err != nil {
		return err
	}
	os.Remove(f.meta)
	if err := enc.Encode(ctlMsg{T: "ok"}); err != nil {
		return err
	}
	o.rep.done("recv", f.size, f.size-startHave, el, out)
	return nil
}

// renameRetry：Windows 上文件刚关掉时杀毒软件、搜索索引可能还占着一会儿，改名失败就再试几次（最多约 1 秒）
func renameRetry(from, to string) (err error) {
	for i := range 5 {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(100*(i+1)) * time.Millisecond)
	}
	return err
}

// recvPiece 收一块。断在中间也把已经写下的部分记上，续传时只补剩下的
func recvPiece(str *quic.ReceiveStream, f *inFile) {
	if f == nil {
		str.CancelRead(1)
		return
	}
	var hdr [22]byte
	if _, err := io.ReadFull(str, hdr[:]); err != nil || string(hdr[:6]) != "P2PCP2" {
		str.CancelRead(1)
		return
	}
	off, n := binary.BigEndian.Uint64(hdr[6:]), binary.BigEndian.Uint64(hdr[14:])
	if off > f.size || n > f.size-off {
		str.CancelRead(1)
		return
	}
	buf := make([]byte, 256<<10)
	var got uint64
	defer func() { f.add(off, got) }()
	for got < n {
		k, err := str.Read(buf[:min(uint64(len(buf)), n-got)])
		if f.sealed.Load() {
			str.CancelRead(1)
			return
		}
		if k > 0 {
			if _, werr := f.f.WriteAt(buf[:k], int64(off+got)); werr != nil {
				str.CancelRead(1)
				return
			}
			got += uint64(k)
			f.m.n.Add(uint64(k))
		}
		if err != nil {
			return
		}
	}
}

// cleanStale 删掉一周没动过的半成品（对方再也没来续传的）
func cleanStale(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".p2pcp-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// uniquePath 在 dir 里给 name 找一个不冲突的路径：a.mp4 → a (1).mp4 → a (2).mp4 …
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}
