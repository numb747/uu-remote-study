package main

// 组合预测：两端画像 → 能不能直连、为什么（docs/arch/designs/2026-11-21-preflight.md 决策 8）。
//
// 依据 netns 实验室的 4×4 矩阵（docs/06 第 1.1 节）：
//   - 两端都是 EIM（cone / port 任意组合）：会合服务器给的映射就是对方要打的地址，能通
//   - cone ↔ sym：cone 那端放行任何来源（或只认 IP），从对方的入包学到它真实的新端口，能通（restricted ↔ sym 5/5）
//   - port ↔ sym：port 只放行自己发过包的「IP:端口」，而 sym 每换目的地就换端口，对方猜不到，打不通（0/5）
//   - sym ↔ sym：两边都猜不到，打不通
//   - eim（过滤没测）↔ sym：看 eim 那端是 cone 还是 port，不确定
//   - open（没有 NAT）当 cone 处理
//
// 两端都有全局 IPv6 时另走 IPv6（没有 NAT，只有运营商的状态防火墙），IPv4 打不通也「可能」直连。

import "fmt"

const (
	predYes     = "能直连"
	predNo      = "打不通"
	predMaybe   = "不确定"
	predUnknown = "无法预测"
)

type verdict struct {
	Result string // predYes / predNo / predMaybe / predUnknown
	Why    string
}

func (v verdict) String() string { return v.Result + "：" + v.Why }

// predict 预测本机 me 和对方 peer 之间能不能直连
func predict(me, peer profile) verdict {
	switch {
	case me.Class == "":
		return verdict{predUnknown, "本机画像没测出来（" + me.Note + "）"}
	case peer.Class == "":
		return verdict{predUnknown, "对方" + peer.Note}
	}
	v := predict4(me.Class, peer.Class)
	if me.V6 && peer.V6 {
		if v.Result == predYes {
			v.Why += "；两端还都有 IPv6"
		} else {
			v = verdict{predMaybe, v.Why + "；但两端都有 IPv6，IPv6 可能直连（看运营商防火墙）"}
		}
	}
	return v
}

// predict4 只看 IPv4 的类别
func predict4(me, peer string) verdict {
	cone := func(c string) bool { return c == natCone || c == natOpen }
	who := func(isMe bool) string {
		if isMe {
			return "本机"
		}
		return "对方"
	}
	switch {
	case me == natSym && peer == natSym:
		return verdict{predNo, "两端都是对称型，各自的新端口对方都猜不到"}
	case me != natSym && peer != natSym:
		return verdict{predYes, "两端映射都与目的地无关，会合服务器看到的地址就是要打的地址"}
	}
	// 恰好一端是 sym
	other, otherIsMe := peer, false
	if peer == natSym {
		other, otherIsMe = me, true
	}
	switch {
	case cone(other):
		return verdict{predYes, fmt.Sprintf("%s对称型，但%s过滤宽松，能从入包学到它的新端口", who(!otherIsMe), who(otherIsMe))}
	case other == natPort:
		return verdict{predNo, fmt.Sprintf("%s对称型，%s只放行发过包的 IP:端口，猜不到对方的新端口", who(!otherIsMe), who(otherIsMe))}
	default: // eim：过滤没测
		return verdict{predMaybe, fmt.Sprintf("%s对称型，要看%s的过滤：宽松能通，端口受限就不通（还没测）", who(!otherIsMe), who(otherIsMe))}
	}
}
