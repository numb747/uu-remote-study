package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const alpn = "p2pcp"

func quicConf() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:                10 * time.Second, // 两端 NAT 映射寿命最短 60 秒（docs/08），10 秒一次足够
		MaxIdleTimeout:                 30 * time.Second,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         64 << 20,
		InitialConnectionReceiveWindow: 4 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
	}
}

// header 在每条连接的流开头，告诉接收端这一段放在文件的哪里。
//
//	magic "P2PCP1" | conns u16 | idx u16 | size u64 | off u64 | len u64 | name_len u16 | name
type header struct {
	conns, idx     uint16
	size, off, len uint64
	name           string
}

func (h header) marshal() []byte {
	b := make([]byte, 0, 36+len(h.name))
	b = append(b, "P2PCP1"...)
	b = binary.BigEndian.AppendUint16(b, h.conns)
	b = binary.BigEndian.AppendUint16(b, h.idx)
	b = binary.BigEndian.AppendUint64(b, h.size)
	b = binary.BigEndian.AppendUint64(b, h.off)
	b = binary.BigEndian.AppendUint64(b, h.len)
	b = binary.BigEndian.AppendUint16(b, uint16(len(h.name)))
	return append(b, h.name...)
}

func readHeader(r io.Reader) (header, error) {
	var b [36]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return header{}, err
	}
	if string(b[:6]) != "P2PCP1" {
		return header{}, errors.New("不是 p2pcp 流")
	}
	h := header{
		conns: binary.BigEndian.Uint16(b[6:]), idx: binary.BigEndian.Uint16(b[8:]),
		size: binary.BigEndian.Uint64(b[10:]), off: binary.BigEndian.Uint64(b[18:]), len: binary.BigEndian.Uint64(b[26:]),
	}
	name := make([]byte, binary.BigEndian.Uint16(b[34:]))
	if _, err := io.ReadFull(r, name); err != nil {
		return header{}, err
	}
	h.name = filepath.Base(string(name)) // 不信任对方给的路径
	return h, nil
}

// answerPunch 在 QUIC 接管 socket 后继续回应对方迟到的打洞探测：对方可能还没收到我们的回应。
func answerPunch(ctx context.Context, tr *quic.Transport) {
	buf := make([]byte, 1500)
	for {
		n, from, err := tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if string(buf[:min(n, 3)]) == msgPunch {
			tr.WriteTo([]byte(msgAck), from)
		}
	}
}

func roomOf(o opts, i int) string { return fmt.Sprintf("%s.%d", o.room, i) }

func statsLine(c *quic.Conn) string {
	st := c.ConnectionStats()
	lossPct := 0.0
	if st.PacketsSent > 0 {
		lossPct = float64(st.PacketsLost) / float64(st.PacketsSent) * 100
	}
	return fmt.Sprintf("SRTT %v  MinRTT %v  发包 %d  判定丢失 %d (%.2f%%)",
		st.SmoothedRTT.Round(100*time.Microsecond), st.MinRTT.Round(100*time.Microsecond), st.PacketsSent, st.PacketsLost, lossPct)
}

// ---------------- send ----------------

func send(o opts, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := uint64(fi.Size())
	n := max(o.conns, 1)
	log.Printf("发送 %s（%.1fMB），%d 条连接，房间 %s", fi.Name(), float64(size)/1e6, n, o.room)

	sum := make(chan string, 1)
	go func() { sum <- fileSHA256(path) }()

	chunk := (size + uint64(n) - 1) / uint64(n)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		start time.Time // 第一条连接开始发数据的时刻
	)
	for i := range n {
		off := min(uint64(i)*chunk, size)
		h := header{conns: uint16(n), idx: uint16(i), size: size, off: off, len: min(chunk, size-off), name: fi.Name()}
		wg.Go(func() {
			if err := sendPart(o, f, h, &mu, &start); err != nil {
				mu.Lock()
				first = cmp(first, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if first != nil {
		return first
	}
	el := time.Since(start)
	log.Printf("全部完成：%.1fMB 用时 %v，平均 %.1f Mbps（%.2f MB/s）", float64(size)/1e6, el.Round(time.Millisecond),
		float64(size)*8/el.Seconds()/1e6, float64(size)/el.Seconds()/1e6)
	log.Printf("sha256 %s", <-sum)
	return nil
}

func cmp(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func sendPart(o opts, f *os.File, h header, mu *sync.Mutex, start *time.Time) error {
	s, err := connect(o, roomOf(o, int(h.idx)), "send")
	if err != nil {
		return err
	}
	log.Printf("[%d] 打通 %s，用时 %v", h.idx, s.peer, s.took.Round(time.Millisecond))
	tr := &quic.Transport{Conn: s.conn}
	defer tr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go answerPunch(ctx, tr)

	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	conn, err := tr.Dial(dctx, s.peer, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}, quicConf())
	if err != nil {
		return fmt.Errorf("[%d] QUIC 握手: %w", h.idx, err)
	}
	defer conn.CloseWithError(0, "")
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	mu.Lock()
	if start.IsZero() {
		*start = time.Now()
	}
	mu.Unlock()
	t0 := time.Now()
	if _, err := str.Write(h.marshal()); err != nil {
		return err
	}
	if _, err := io.Copy(str, io.NewSectionReader(f, int64(h.off), int64(h.len))); err != nil {
		return fmt.Errorf("[%d] 发送: %w", h.idx, err)
	}
	str.Close()
	// 等接收端写完盘回 "OK"，这样计时包含了最后一个字节真正到达
	ok := make([]byte, 2)
	if _, err := io.ReadFull(str, ok); err != nil || string(ok) != "OK" {
		return fmt.Errorf("[%d] 没等到确认: %v", h.idx, err)
	}
	el := time.Since(t0)
	log.Printf("[%d] %.1fMB 用时 %v（%.1f Mbps）  %s", h.idx, float64(h.len)/1e6, el.Round(time.Millisecond),
		float64(h.len)*8/el.Seconds()/1e6, statsLine(conn))
	return nil
}

// ---------------- recv ----------------

func recv(o opts) error {
	tc, err := selfSigned()
	if err != nil {
		return err
	}
	var (
		got      atomic.Uint64
		out      *os.File
		outPath  string
		size     uint64
		openOnce sync.Once
		openErr  error
		first    time.Time
		mu       sync.Mutex
	)
	var (
		wg   sync.WaitGroup
		errs = make(chan error, 256)
		part func(i int) (header, error)
	)
	spawn := func(i int) {
		wg.Go(func() {
			if _, err := part(i); err != nil {
				errs <- err
			}
		})
	}
	// part 接一条连接：打洞 → QUIC → 读头 → 写到文件对应位置。第 0 条连接的头里有总连接数，读到后立即去接其余几条
	// （发送端那几条正在会合服务器上等着）。
	part = func(i int) (header, error) {
		s, err := connect(o, roomOf(o, i), "recv")
		if err != nil {
			return header{}, err
		}
		log.Printf("[%d] 打通 %s，用时 %v", i, s.peer, s.took.Round(time.Millisecond))
		tr := &quic.Transport{Conn: s.conn}
		defer tr.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go answerPunch(ctx, tr)
		ln, err := tr.Listen(tc, quicConf())
		if err != nil {
			return header{}, err
		}
		actx, acancel := context.WithTimeout(ctx, 15*time.Second)
		defer acancel()
		conn, err := ln.Accept(actx)
		if err != nil {
			return header{}, fmt.Errorf("[%d] 等 QUIC 连接: %w", i, err)
		}
		defer conn.CloseWithError(0, "")
		str, err := conn.AcceptStream(ctx)
		if err != nil {
			return header{}, err
		}
		h, err := readHeader(str)
		if err != nil {
			return h, err
		}
		if i == 0 {
			for j := 1; j < int(h.conns); j++ {
				spawn(j)
			}
		}
		openOnce.Do(func() {
			size = h.size
			outPath = filepath.Join(o.out, h.name)
			if out, openErr = os.Create(outPath); openErr == nil {
				openErr = out.Truncate(int64(h.size))
			}
		})
		if openErr != nil {
			return h, openErr
		}
		mu.Lock()
		if first.IsZero() {
			first = time.Now()
		}
		mu.Unlock()
		w := io.NewOffsetWriter(out, int64(h.off))
		buf := make([]byte, 256<<10)
		var n uint64
		for n < h.len {
			k, err := str.Read(buf)
			if k > 0 {
				if _, werr := w.Write(buf[:k]); werr != nil {
					return h, werr
				}
				n += uint64(k)
				got.Add(uint64(k))
			}
			if err != nil {
				if err == io.EOF && n == h.len {
					break
				}
				return h, fmt.Errorf("[%d] 接收: %w（收到 %d/%d）", i, err, n, h.len)
			}
		}
		str.Write([]byte("OK"))
		str.Close()
		log.Printf("[%d] 收完 %.1fMB  %s", i, float64(n)/1e6, statsLine(conn))
		time.Sleep(300 * time.Millisecond) // 让 OK 送达再关连接
		return h, nil
	}

	// 进度：每秒一行
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var last uint64
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				g := got.Load()
				if g == 0 {
					continue
				}
				pct := 0.0
				if size > 0 {
					pct = float64(g) / float64(size) * 100
				}
				log.Printf("  %6.1fMB  %5.1f%%  当前 %.1f Mbps", float64(g)/1e6, pct, float64(g-last)*8/1e6)
				last = g
			}
		}
	}()
	defer close(stop)

	spawn(0)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	el := time.Since(first)
	out.Close()
	log.Printf("全部收完：%s  %.1fMB 用时 %v，平均 %.1f Mbps（%.2f MB/s）", outPath, float64(size)/1e6, el.Round(time.Millisecond),
		float64(size)*8/el.Seconds()/1e6, float64(size)/el.Seconds()/1e6)
	log.Printf("sha256 %s", fileSHA256(outPath))
	return nil
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func selfSigned() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{alpn}}, nil
}
