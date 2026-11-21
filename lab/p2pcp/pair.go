package main

// 通讯录模式的命令：id / invite / join / contacts / serve（docs/arch/DESIGN.md，图 27-flow-pairing）。
//
// 加好友：邀请方 invite 生成一次性密钥、记进通讯录的「待用邀请」，常驻接收（serve）用它当房间密钥在邀请房间里等；
// 被邀请方 join 用同一个密钥会合、打洞、QUIC 认证（和传文件同一套），然后在 ALPN p2pcp-pair/1 上互换名片：
//
//	被邀请方 → {"v":1,"name","pub"}
//	邀请方   → {"v":1,"name","pub"} 或 {"err"}   邀请方先在锁里把邀请作废、把对方加进通讯录，才回自己的名片：
//	                                           同一个邀请码被两个人同时用，后到的那个拿不到名片、加不上
//
// 之后两边各用 pairSecret 算出同一个配对密钥，send -to / serve 拿它当房间密钥。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apernet/quic-go"
	"rsc.io/qr"
)

type card struct {
	V    int    `json:"v"`
	Name string `json:"name,omitempty"`
	Pub  []byte `json:"pub,omitempty"`
	Err  string `json:"err,omitempty"`
}

// cmdID 显示（-name 时先改）本机的设备名和设备 ID
func cmdID(o opts) error {
	dir := configDir(o)
	id, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	if o.name != "" {
		if id.Name = cleanName(o.name); id.Name == "" {
			return errors.New("设备名不能是空的")
		}
		if err := saveIdentity(dir, id); err != nil {
			return err
		}
	}
	fmt.Printf("设备名 %s，设备 ID %s（配置在 %s）\n", id.Name, deviceID(id.pub()), dir)
	b, _ := json.Marshal(map[string]string{"name": id.Name, "id": deviceID(id.pub())})
	o.rep.line("@ID %s", b)
	return nil
}

// cmdInvite 生成邀请码，记进通讯录的待用邀请（serve 会在后台等对方来用）
func cmdInvite(o opts) error {
	dir := configDir(o)
	if _, err := loadIdentity(dir); err != nil {
		return err
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	if err := updateBook(dir, func(b *book) error {
		b.Invites = append(b.Invites, pendingInvite{Secret: secret, Server: o.server, Expires: time.Now().Add(o.ttl)})
		return nil
	}); err != nil {
		return err
	}
	code := encodeInvite(secret, o.server)
	fmt.Printf("邀请码（%v 内有效，用一次就作废）：\n\n%s\n\n", o.ttl, code)
	fmt.Println("发给朋友，对方运行 p2pcp join <邀请码>（App 里点「加好友」粘贴）。你这边的常驻接收（p2pcp serve）要开着，对方才加得上。")
	o.rep.line("@INVITE %s", code)
	switch o.qr {
	case "":
	case "-":
		printQR(code)
	default:
		c, err := qr.Encode(code, qr.M)
		if err != nil {
			return err
		}
		c.Scale = 8
		if err := os.WriteFile(o.qr, c.PNG(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// printQR 在终端里画二维码：一个字符管上下两格，亮的格子画成实心（深色终端上扫得出）
func printQR(text string) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return
	}
	light := func(x, y int) bool { return x < 0 || y < 0 || x >= c.Size || y >= c.Size || !c.Black(x, y) }
	var sb strings.Builder
	for y := -2; y < c.Size+2; y += 2 {
		for x := -2; x < c.Size+2; x++ {
			switch t, b := light(x, y), light(x, y+1); {
			case t && b:
				sb.WriteString("█")
			case t:
				sb.WriteString("▀")
			case b:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}
	fmt.Print(sb.String())
}

// cmdJoin 用邀请码加好友
func cmdJoin(o opts, text string) error {
	secret, server, err := parseInvite(text)
	if err != nil {
		return err
	}
	dir := configDir(o)
	id, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	o.secret, o.server, o.stun = secret, server, "off" // 加好友不用体检；会合地址听邀请码的
	o.rep.state("连接邀请方……")
	l, err := dialPeer(o, pairALPN)
	if err != nil {
		return fmt.Errorf("连不上邀请方（邀请码过期了、用过了，或者对方的常驻接收没开）：%w", err)
	}
	defer l.close()
	peer, err := clientCards(l.conn, card{V: 1, Name: id.Name, Pub: id.pub()})
	if err != nil {
		return err
	}
	if bytes.Equal(peer.Pub, id.pub()) {
		return errors.New("这是你自己发出的邀请码")
	}
	name := cleanName(o.name)
	if name == "" {
		name = peer.Name
	}
	var final string
	if err := updateBook(dir, func(b *book) error {
		final = b.addContact(contact{Name: name, Pub: peer.Pub, Server: server, Added: time.Now()})
		return nil
	}); err != nil {
		return err
	}
	reportPaired(o, final, peer.Pub)
	return nil
}

func reportPaired(o opts, name string, pub []byte) {
	noteName("p", name)
	o.rep.state(fmt.Sprintf("已加好友「%s」（设备 ID %s）", name, deviceID(pub)))
	o.rep.desktop("P2P 直传：新好友", fmt.Sprintf("已加好友「%s」", name), -1)
	b, _ := json.Marshal(map[string]string{"name": name, "id": deviceID(pub)})
	o.rep.line("@PAIRED %s", b)
}

// clientCards（被邀请方）先发自己的名片，再收对方的
func clientCards(conn *quic.Conn, mine card) (card, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	str.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(str).Encode(mine); err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	var peer card
	if err := json.NewDecoder(str).Decode(&peer); err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	str.Close()
	if peer.Err != "" {
		return card{}, fmt.Errorf("邀请方拒绝：%s", peer.Err)
	}
	if err := peer.check(); err != nil {
		return card{}, err
	}
	return peer, nil
}

func (c card) check() error {
	if c.V != 1 || len(c.Pub) != 32 {
		return fmt.Errorf("对方的名片不对（版本 %d，公钥 %d 字节）", c.V, len(c.Pub))
	}
	return nil
}

// serveCards（邀请方）收对方的名片 → accept 决定收不收（作废邀请、加进通讯录）→ 回自己的名片或拒绝的原因
func serveCards(conn *quic.Conn, mine card, accept func(peer card) error) (card, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	str, err := conn.AcceptStream(ctx)
	if err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	str.SetDeadline(time.Now().Add(10 * time.Second))
	var peer card
	if err := json.NewDecoder(str).Decode(&peer); err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	err = peer.check()
	if err == nil {
		err = accept(peer)
	}
	if err != nil {
		json.NewEncoder(str).Encode(card{V: 1, Err: err.Error()})
		str.Close()
		return card{}, err
	}
	if err := json.NewEncoder(str).Encode(mine); err != nil {
		return card{}, fmt.Errorf("互换名片：%w", err)
	}
	str.Close()
	return peer, nil
}

// cmdContacts 列出好友；contacts rm 名字 / contacts rename 旧名 新名
func cmdContacts(o opts, args []string) error {
	dir := configDir(o)
	switch {
	case len(args) == 0:
	case args[0] == "rm" && len(args) == 2:
		return updateBook(dir, func(b *book) error {
			c, err := b.find(args[1])
			if err != nil {
				return err
			}
			b.Contacts = append(b.Contacts[:b.byName(c.Name)], b.Contacts[b.byName(c.Name)+1:]...)
			fmt.Printf("已删除好友「%s」（对方那边也要删，否则它发过来时这边不再接收）\n", c.Name)
			return nil
		})
	case args[0] == "rename" && len(args) == 3:
		return updateBook(dir, func(b *book) error {
			c, err := b.find(args[1])
			if err != nil {
				return err
			}
			name := cleanName(args[2])
			if name == "" || b.byName(name) >= 0 {
				return fmt.Errorf("新名字「%s」不能用（空的或者和别的好友重了）", args[2])
			}
			b.Contacts[b.byName(c.Name)].Name = name
			fmt.Printf("「%s」改名为「%s」\n", c.Name, name)
			return nil
		})
	default:
		return errors.New("用法：p2pcp contacts [rm 名字 | rename 旧名 新名]")
	}
	b, err := loadBook(dir)
	if err != nil {
		return err
	}
	if len(b.Contacts) == 0 {
		fmt.Println("还没有好友：p2pcp invite 生成邀请码发给朋友，或者 p2pcp join <朋友给的邀请码>")
	}
	sort.Slice(b.Contacts, func(i, j int) bool { return b.Contacts[i].Added.Before(b.Contacts[j].Added) })
	for _, c := range b.Contacts {
		fmt.Printf("%-20s  %s  会合 %s  %s 加的\n", c.Name, c.id(), c.Server, c.Added.Format("2006-01-02"))
	}
	if n := len(liveInvites(b.Invites)); n > 0 {
		fmt.Printf("（另有 %d 个邀请码还没被用）\n", n)
	}
	if o.rep.machine {
		type row struct {
			Name   string `json:"name"`
			ID     string `json:"id"`
			Server string `json:"server"`
		}
		var rows []row
		for _, c := range b.Contacts {
			rows = append(rows, row{c.Name, c.id(), c.Server})
		}
		j, _ := json.Marshal(rows)
		o.rep.line("@CONTACTS %s", j)
	}
	return nil
}

// resolveFriend：send -to / check -to 时按名字找好友，房间密钥和会合地址换成这个好友的。
// send 用对方的收件房间（inboxSecret）；check 两端跑同一条命令，用不分方向的配对密钥
func resolveFriend(o *opts, role string) error {
	dir := configDir(*o)
	id, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	b, err := loadBook(dir)
	if err != nil {
		return err
	}
	c, err := b.find(o.friend)
	if err != nil {
		return err
	}
	if o.secret, err = pairSecret(id, c.Pub); err != nil {
		return err
	}
	if role == "send" {
		o.secret = inboxSecret(o.secret, c.Pub)
	}
	if !o.serverSet {
		o.server = c.Server
	}
	o.friend = c.Name
	noteName("p", c.Name) // 诊断报告里打码用（diag.go）
	return nil
}

// serve 常驻接收：同时听通讯录里每个好友的房间（收文件，按好友分目录）和还没用掉的邀请（加好友）。
// 每 2 秒看一眼通讯录的修改时间，invite / join / contacts rm 改了它，这边就多听或少听几个房间，不用重启
func serve(o opts) error {
	dir := configDir(o)
	id, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	mine := card{V: 1, Name: id.Name, Pub: id.pub()}
	running := map[string]chan struct{}{}
	var lastMod time.Time
	first := true
	for {
		var mod time.Time
		if st, err := os.Stat(bookPath(dir)); err == nil {
			mod = st.ModTime()
		}
		if first || !mod.Equal(lastMod) {
			first, lastMod = false, mod
			b, err := loadBook(dir)
			if err != nil {
				log.Print(err)
			} else {
				want := map[string]func(chan struct{}){}
				for _, c := range b.Contacts {
					ps, err := pairSecret(id, c.Pub)
					if err != nil {
						log.Printf("好友「%s」：%v", c.Name, err)
						continue
					}
					// 名字、会合地址变了要重开（收件目录、服务器跟着变）
					inbox := inboxSecret(ps, mine.Pub) // 好友发给我的那个房间
					want["c "+hex.EncodeToString(c.Pub)+" "+c.Server+" "+c.Name] = func(q chan struct{}) { serveFriend(o, c, inbox, q) }
				}
				for _, iv := range liveInvites(b.Invites) {
					want["i "+hex.EncodeToString(iv.Secret)] = func(q chan struct{}) { serveInvite(o, dir, iv, mine, q) }
				}
				for k, q := range running {
					if want[k] == nil {
						close(q)
						delete(running, k)
					}
				}
				for k, start := range want {
					if running[k] == nil {
						q := make(chan struct{})
						running[k] = q
						go start(q)
					}
				}
				msg := fmt.Sprintf("等待好友发送（%d 个好友", len(b.Contacts))
				if n := len(liveInvites(b.Invites)); n > 0 {
					msg += fmt.Sprintf("，%d 个邀请码待用", n)
				}
				o.rep.state(msg + "）")
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// serveFriend 在和一个好友的配对房间里常驻接收，收到的文件放 <保存目录>/<好友名>/
func serveFriend(o opts, c contact, secret []byte, quit chan struct{}) {
	o.secret, o.server, o.quit, o.friend = secret, c.Server, quit, c.Name
	noteName("p", c.Name)
	o.out = filepath.Join(o.out, cleanName(c.Name))
	cleanStale(o.out)
	keepServing(o, "", func(o opts) (*link, error) { return acceptPeer(o, alpn) }, func(l *link) {
		log.Printf("好友「%s」连上了", c.Name)
		if err := recvSession(o, l); err != nil {
			o.rep.fail(fmt.Errorf("收「%s」的文件：%w", c.Name, err))
		}
	})
}

// serveInvite 在邀请房间里等对方来用邀请码
func serveInvite(o opts, dir string, iv pendingInvite, mine card, quit chan struct{}) {
	o.secret, o.server, o.quit, o.stun = iv.Secret, iv.Server, quit, "off"
	keepServing(o, "", func(o opts) (*link, error) { return acceptPeer(o, pairALPN) }, func(l *link) {
		defer l.close()
		var name string
		peer, err := serveCards(l.conn, mine, func(peer card) error {
			if bytes.Equal(peer.Pub, mine.Pub) {
				return errors.New("这是你自己发出的邀请码")
			}
			return updateBook(dir, func(b *book) error {
				i := -1
				for k, x := range b.Invites {
					if bytes.Equal(x.Secret, iv.Secret) {
						i = k
					}
				}
				if i < 0 {
					return errors.New("邀请码已经用过或者过期了")
				}
				b.Invites = append(b.Invites[:i], b.Invites[i+1:]...)
				name = b.addContact(contact{Name: peer.Name, Pub: peer.Pub, Server: iv.Server, Added: time.Now()})
				return nil
			})
		})
		if err != nil {
			log.Printf("有人用邀请码来加好友，没加上：%v", err)
		} else {
			reportPaired(o, name, peer.Pub)
		}
		// 等对方读完回复、先关连接（最多 3 秒）：这边先关，还在路上的名片或拒绝原因会被丢掉
		select {
		case <-l.conn.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
}
