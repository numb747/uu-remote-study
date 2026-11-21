package main

// 通讯录：设备身份、好友、待用的邀请、配对密钥、邀请码（docs/arch/DESIGN.md，图 27-flow-pairing / 34-contacts）。
//
//   - 设备身份 identity.json：第一次用时生成 X25519 私钥和设备名。设备 ID = 公钥 sha256 的前 4 字节，给人看、区分重名的。
//   - 通讯录 contacts.json：好友（名字、公钥、会合地址）和自己发出去、还没被用掉的邀请。两样放一个文件：
//     常驻接收（serve）只盯这一个文件的修改时间，就知道该多听 / 少听哪些房间。里面有邀请密钥，权限 0600。
//   - 配对密钥 = HKDF(ECDH(我的私钥, 对方公钥))，两边算出来一样，直接当原来的「房间密钥」用（auth.go）：
//     会合服务器、打洞、QUIC 认证都不用改，线路上照样只出现它的 HMAC。
//   - 邀请码 = p2pcp://join/<base32(版本 | 16 字节一次性密钥 | 会合地址)>，可以当文字发微信，也可以显示成二维码。
//     密钥要 128 位：会合服务器看得到房间号（密钥的 HMAC），短了能被离线穷举。

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	pairALPN      = "p2pcp-pair/1" // 加好友时互换名片
	inviteTTL     = 24 * time.Hour // 邀请码有效期：微信发过去，对方可能过几个小时才点
	inviteVersion = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// configDir：身份和通讯录放哪。-config 优先（安卓 App 给自己的私有目录），否则 ~/.config/p2pcp（各系统的用户配置目录）
func configDir(o opts) string {
	if o.config != "" {
		return o.config
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "p2pcp")
	}
	return ".p2pcp"
}

type identity struct {
	Name string `json:"name"`
	Priv []byte `json:"priv"` // X25519 私钥（JSON 里是 base64）
}

func (id *identity) key() (*ecdh.PrivateKey, error) { return ecdh.X25519().NewPrivateKey(id.Priv) }

func (id *identity) pub() []byte {
	k, err := id.key()
	if err != nil {
		return nil
	}
	return k.PublicKey().Bytes()
}

// deviceID 是给人看的短 ID：公钥 sha256 的前 4 字节
func deviceID(pub []byte) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:4])
}

// loadIdentity 读设备身份，没有就生成一个（设备名默认取主机名）
func loadIdentity(dir string) (*identity, error) {
	path := filepath.Join(dir, "identity.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer lockFile(path + ".lock")()
	var id identity
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &id); err != nil {
			return nil, fmt.Errorf("%s 坏了：%w", path, err)
		}
		if _, err := id.key(); err != nil {
			return nil, fmt.Errorf("%s 里的私钥不对：%w", path, err)
		}
		return &id, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id.Priv = k.Bytes()
	id.Name, _ = os.Hostname()
	if id.Name == "" || id.Name == "localhost" {
		id.Name = "设备-" + deviceID(k.PublicKey().Bytes())
	}
	return &id, writeJSON(path, id)
}

// saveIdentity 改设备名之后写回
func saveIdentity(dir string, id *identity) error {
	path := filepath.Join(dir, "identity.json")
	defer lockFile(path + ".lock")()
	return writeJSON(path, id)
}

// writeJSON 先写临时文件再改名：进程在任何时候被杀都不会留下写了一半的文件
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type contact struct {
	Name   string    `json:"name"`
	Pub    []byte    `json:"pub"`
	Server string    `json:"server"` // 和这个好友会合用的服务器（加好友时邀请码里带的）
	Added  time.Time `json:"added"`
}

func (c contact) id() string { return deviceID(c.Pub) }

type pendingInvite struct {
	Secret  []byte    `json:"secret"`
	Server  string    `json:"server"`
	Expires time.Time `json:"expires"`
}

type book struct {
	Contacts []contact       `json:"contacts"`
	Invites  []pendingInvite `json:"invites,omitempty"`
}

func bookPath(dir string) string { return filepath.Join(dir, "contacts.json") }

// loadBook 读通讯录；没有文件就是空的
func loadBook(dir string) (book, error) {
	var b book
	data, err := os.ReadFile(bookPath(dir))
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("%s 坏了：%w", bookPath(dir), err)
	}
	return b, nil
}

// updateBook 在文件锁里读、改、写：invite、join、serve（加好友成功时）可能同时在改
func updateBook(dir string, fn func(*book) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer lockFile(bookPath(dir) + ".lock")()
	b, err := loadBook(dir)
	if err != nil {
		return err
	}
	if err := fn(&b); err != nil {
		return err
	}
	b.Invites = liveInvites(b.Invites)
	return writeJSON(bookPath(dir), b)
}

// liveInvites 去掉过期的邀请
func liveInvites(in []pendingInvite) []pendingInvite {
	var out []pendingInvite
	for _, iv := range in {
		if time.Now().Before(iv.Expires) {
			out = append(out, iv)
		}
	}
	return out
}

// addContact 加进通讯录：同一个公钥已经在了就更新（重新加好友），名字和别人重了就加后缀，返回最终的名字
func (b *book) addContact(c contact) string {
	for i := range b.Contacts {
		if bytes.Equal(b.Contacts[i].Pub, c.Pub) {
			b.Contacts[i].Server = c.Server
			return b.Contacts[i].Name
		}
	}
	base := cleanName(c.Name)
	if base == "" {
		base = "好友-" + c.id()
	}
	c.Name = base
	for n := 2; b.byName(c.Name) >= 0; n++ {
		c.Name = fmt.Sprintf("%s (%d)", base, n)
	}
	b.Contacts = append(b.Contacts, c)
	return c.Name
}

func (b *book) byName(name string) int {
	for i, c := range b.Contacts {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// find 按名字找好友：先完全一致，再不分大小写，再按设备 ID 前缀；对不上或者不唯一都报错，并列出有哪些好友
func (b *book) find(q string) (contact, error) {
	if i := b.byName(q); i >= 0 {
		return b.Contacts[i], nil
	}
	var hits []contact
	for _, c := range b.Contacts {
		if strings.EqualFold(c.Name, q) || len(q) >= 4 && strings.HasPrefix(c.id(), strings.ToLower(q)) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	var names []string
	for _, c := range b.Contacts {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return contact{}, errors.New("通讯录是空的：先用 p2pcp invite / join 加好友")
	}
	if len(hits) > 1 {
		return contact{}, fmt.Errorf("「%s」对上了好几个好友，说全名：%s", q, strings.Join(names, "、"))
	}
	return contact{}, fmt.Errorf("通讯录里没有「%s」，现有好友：%s", q, strings.Join(names, "、"))
}

// cleanName 去掉名字里不能当目录名的字符（收到的文件按好友分目录），最长 40 个字
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	for utf8.RuneCountInString(s) > 40 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

// pairSecret 两个设备之间的房间密钥：ECDH 的共享值过一遍 HKDF，盐是两把公钥按大小排好拼起来（两边顺序一致）
func pairSecret(id *identity, peerPub []byte) ([]byte, error) {
	k, err := id.key()
	if err != nil {
		return nil, err
	}
	peer, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("好友的公钥不对：%w", err)
	}
	shared, err := k.ECDH(peer)
	if err != nil {
		return nil, err
	}
	a, b := k.PublicKey().Bytes(), peerPub
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	return hkdf.Key(sha256.New, shared, append(append([]byte{}, a...), b...), "p2pcp pair v1", 32)
}

// inboxSecret 是发给 to（收件方公钥）的房间密钥：配对密钥再按方向派生一次。每台设备的 serve 只听「发给自己」的房间，
// 否则自己的 send -to 好友和自己的 serve 在同一个房间里，会合服务器会把它们配成一对，文件发给了自己
func inboxSecret(pair, to []byte) []byte { return mac(pair, []byte("p2pcp inbox"), to) }

// encodeInvite / parseInvite：邀请码。base32 只有大写字母和数字，好抄、二维码也能用字母数字模式
func encodeInvite(secret []byte, server string) string {
	raw := append([]byte{inviteVersion}, secret...)
	return "p2pcp://join/" + b32.EncodeToString(append(raw, server...))
}

var inviteRe = regexp.MustCompile(`(?i)p2pcp://join/([a-z2-7]+)`)

// parseInvite 从一段文字里找邀请码（微信里转发时前后可能带别的字）
func parseInvite(text string) (secret []byte, server string, err error) {
	m := inviteRe.FindStringSubmatch(text)
	if m == nil {
		return nil, "", errors.New("没找到邀请码（p2pcp://join/ 开头的那一串）")
	}
	raw, err := b32.DecodeString(strings.ToUpper(m[1]))
	if err != nil || len(raw) < 1+16+1 {
		return nil, "", errors.New("邀请码不完整，可能复制漏了一截")
	}
	if raw[0] != inviteVersion {
		return nil, "", fmt.Errorf("邀请码是新版本（v%d）生成的，先升级 p2pcp", raw[0])
	}
	return raw[1:17], string(raw[17:]), nil
}

// roomKey：这次会合用的房间密钥。通讯录模式下是配对密钥或邀请密钥（o.secret），否则由 -room 派生
func roomKey(o opts) []byte {
	if o.secret != nil {
		return o.secret
	}
	return roomSecret(o.room)
}
