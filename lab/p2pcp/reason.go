package main

// 原因码（docs/arch/DESIGN.md 决策 23）：把各层的错误归成几类固定的码，每类配一句人话和一个能做的动作。
// 界面上显示人话（-progress 的 @REASON 行），history.jsonl、上报、@CHECK 里带码，服务器那边按码统计。
//
// 归类靠哨兵错误（rdv.go 的 errNoPeer 等、auth.go 的 errAuth、transfer.go 的 interrupted）和 errors.Is / As，
// 少数地方（握手失败）用 withReason 直接标上。新加的错误要么包一个哨兵，要么标码，否则会落进 unknown。

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

const (
	rsnRdv     = "rdv_unreachable" // 本机连不上会合服务器
	rsnOffline = "peer_offline"    // 对方没在线
	rsnPunch   = "punch_blocked"   // 两边都在线但打不通（有中继时会自动走中继，不算失败）
	rsnRelay   = "relay_failed"    // 中继也不通
	rsnLost    = "conn_lost"       // 连上之后断了
	rsnAuth    = "auth_failed"     // 对方不是这个好友
	rsnLocal   = "local_error"     // 本机读写文件出错
	rsnUnknown = "unknown"
	// 手动取消（cancel.go）：不是失败，记下来是为了统计里分得清
	rsnCanceled     = "canceled"      // 本机取消
	rsnPeerCanceled = "peer_canceled" // 对方取消
)

// reasonText：码 → 一句人话 + 一个能做的动作
var reasonText = map[string][2]string{
	rsnRdv:          {"连不上会合服务器", "检查本机网络；公司 / 学校网络可能封了 UDP，换手机热点试试"},
	rsnOffline:      {"对方没在线", "请对方打开 P2P 直传；安卓手机熄屏会被系统冻结，要在系统设置里给它打开「允许后台高耗电」"},
	rsnPunch:        {"两边都在线，但网络之间打不通", "两边的网络都太严（对称型 NAT），换个网络（比如手机热点）再试"},
	rsnRelay:        {"打不通，中继服务器也连不上", "过一会儿再试；一直不行就生成诊断报告发给我"},
	rsnLost:         {"连上之后连接断了", "网络不稳定或者换了网络；重新发送会从断的地方接着传"},
	rsnAuth:         {"对方没通过身份核对", "对方可能不是这个好友，或者通讯录记录坏了：删掉好友重新加一次"},
	rsnLocal:        {"本机读写文件出错", "检查文件还在不在、有没有权限、磁盘满没满"},
	rsnUnknown:      {"出错了", "生成诊断报告发给我"},
	rsnCanceled:     {"已取消", "要发的话重新发一次"},
	rsnPeerCanceled: {"对方取消了", "要发的话重新发一次"},
}

// reasonError 给错误标上码（握手失败这类没有哨兵的地方用）
type reasonError struct {
	code string
	err  error
}

func (e reasonError) Error() string { return e.err.Error() }
func (e reasonError) Unwrap() error { return e.err }

func withReason(code string, err error) error {
	if err == nil {
		return nil
	}
	return reasonError{code, err}
}

// reasonOf 归类；nil 和 errStopped（主动叫停）不算失败，返回空串
func reasonOf(err error) string {
	if err == nil || errors.Is(err, errStopped) {
		return ""
	}
	var ce canceledErr
	if errors.As(err, &ce) {
		if ce.peer {
			return rsnPeerCanceled
		}
		return rsnCanceled
	}
	if peerCanceled(err) {
		return rsnPeerCanceled
	}
	var re reasonError
	if errors.As(err, &re) {
		return re.code
	}
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.As(err, &pe), errors.As(err, &le), errors.Is(err, errNotRegular):
		return rsnLocal // 放在 interrupted 前面：万一磁盘错误被包进了 interrupted，也算本机的问题
	case errors.Is(err, errAuth):
		return rsnAuth
	case errors.Is(err, errNoPeer):
		return rsnOffline
	case errors.Is(err, errRelay):
		return rsnRelay
	case errors.Is(err, errPunchTimeout):
		return rsnPunch
	case errors.Is(err, errNoRendezvous), errors.Is(err, errBadPort), errors.Is(err, errLost):
		return rsnRdv
	case errors.As(err, new(interrupted)):
		return rsnLost
	}
	return rsnUnknown
}

// reasonLine 是 @REASON 行的内容：码 + 人话 + 动作
func reasonLine(code string) string {
	t, ok := reasonText[code]
	if !ok {
		t = reasonText[rsnUnknown]
	}
	return code + " " + t[0] + "。" + t[1]
}
