#!/usr/bin/env bash
# netns NAT 实验室 —— 不需要 root。
# 用 unshare -Urnm 起一个私有的"实验宇宙"（user + net + mount 命名空间），
# 在里面以"假 root"身份搭网络；进程退出后所有命名空间自动消失，碰不到真实网络。
#
#   peerA 10.0.1.2 ── natA [10.0.1.1 | 203.0.113.1] ──┐
#                                                     ├── inet 路由器（lo 上 192.0.2.1 跑会合服务器）
#   peerB 10.0.2.2 ── natB [10.0.2.1 | 198.51.100.2] ─┘
#
# inet 是一跳路由器而不是网桥：两个 NAT 之间至少隔一跳，低 TTL 预热才有地方"死掉"。
# 从 peer 出发：TTL=2 能出自家 NAT（减到 1），到 inet 时减到 0 被丢弃，到不了对方的 NAT。
#
# 用法:
#   lab.sh run <natA> <natB> [选项]   跑一次 punch，输出是否打通、走哪条路、RTT
#   lab.sh matrix [选项]              所有 NAT 组合各跑 --repeat 次，输出成功矩阵
#   lab.sh shell <natA> <natB> [选项] 搭好后进交互 shell，自己 ip netns exec peerA ...
#   lab.sh script <natA> <natB> <脚本> [选项]  搭好后在宇宙里执行脚本（p2pcp 测试用，见 p2pcp-test.sh），
#                                     脚本里可用 $BIN_DIR/{punch,p2pcp}，会合服务器地址 192.0.2.1
#   lab.sh xfer <own|quic|tcp> [选项]  传输层测试：peerA（NAT 后）发帧流给 inet 上的接收端，
#                                     弱网用 --netem-a 加在 A 的接入链路上，xfer 参数用 --xfer-args 传
#
# NAT 类型:
#   full  全锥 (EIM + EIF)：保留端口 + 入方向 UDP 全部 DNAT 给内网主机
#   restricted  受限锥 (EIM + ADF)：保留端口 + 只把「这个端口发去过的 IP」来的入包 DNAT 给内网主机
#   port  端口限制锥 (EIM + APDF)：Linux MASQUERADE 默认行为
#   sym   对称·随机端口 (EDM)：MASQUERADE fully-random
#
# 选项:
#   --netem-a "<netem 参数>"  natA 接入链路的弱网（双向），如 "delay 20ms 5ms loss 2% rate 50mbit"
#   --netem-b "<netem 参数>"  同上，natB
#   --spray N                 punch 的端口喷洒范围 ±N
#   --timeout S               打洞超时秒数（默认 6）
#   --no-fw                   NAT 不在 INPUT 丢弃 WAN 来的未请求包（复现 conntrack 冲突换端口的坑）
#   --punch-args "<参数>"     传给两端 punch peer 的额外参数，如 "-ttl 2"
#   --bday                    按双方 NAT 类型自动分配生日攻击角色：port↔sym 时对称端 -bday-sockets 256、
#                             锥形端 -bday-probes 1024（相当于信令根据 NAT 画像做的决策）
#   --bday-probes N           生日攻击锥形端的探测预算（默认 1024，隐含 --bday）
#   --repeat N                matrix 每个组合跑几次（默认 1）
#   --xfer-args "<参数>"      传给 xfer 两端的参数，如 "-dur 10s -bitrate 20000000 -deadline 100ms"
#   -v                        打印完整日志
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
TYPES=(full restricted port sym)

die() { echo "lab.sh: $*" >&2; exit 2; }

# ---------------- 宇宙内部（以 unshare 后的假 root 运行） ----------------

nat_rules() { # <ns> <内网主机 IP> <类型>
	local ns=$1 inner=$2 type=$3
	local nft="ip netns exec $ns nft"
	$nft add table ip nat
	$nft add chain ip nat post '{ type nat hook postrouting priority 100; }'
	$nft add chain ip nat pre '{ type nat hook prerouting priority -100; }'
	case $type in
	port) $nft add rule ip nat post oifname wan masquerade ;;
	sym) $nft add rule ip nat post oifname wan masquerade fully-random ;;
	full)
		$nft add rule ip nat post oifname wan masquerade
		$nft add rule ip nat pre iifname wan udp dport 1024-65535 dnat to "$inner"
		;;
	restricted)
		# 受限锥：映射同 port（MASQUERADE 保留端口，外网端口 = 内网端口），过滤只核对源 IP。
		# 出包时把「对方 IP . 本方端口」记进动态集合；入包只要源 IP 在这个端口的名单上就放进来，不管源端口。
		$nft add rule ip nat post oifname wan masquerade
		$nft add set ip nat contacted '{ type ipv4_addr . inet_service; flags dynamic,timeout; timeout 120s; }'
		$nft add chain ip nat outbound '{ type filter hook forward priority 0; }'
		$nft add rule ip nat outbound iifname lan oifname wan meta l4proto udp update @contacted '{ ip daddr . udp sport }'
		$nft add rule ip nat pre iifname wan ip saddr . udp dport @contacted dnat to "$inner"
		;;
	*) die "未知 NAT 类型: $type（可选: ${TYPES[*]}）" ;;
	esac
	# 像真实路由器一样，在 INPUT 丢掉 WAN 上不请自来的包。丢在 conntrack 确认之前，所以不会留下记录。
	# 不加这条（--no-fw）时，对端先到的打洞包会被本机收下并确认成一条 conntrack，
	# 本方随后出去的映射跟它冲突，MASQUERADE 只好换端口 —— 端口限制锥之间互打就此失败。
	if [[ $NO_FW != 1 ]]; then
		$nft add table ip filter
		$nft add chain ip filter input '{ type filter hook input priority 0; }'
		$nft add rule ip filter input iifname wan ct state new drop
	fi
}

add_side() { # <A|B> <序号> <WAN 网段前缀> <NAT 类型> <netem 参数>
	local s=$1 i=$2 net=$3 type=$4 netem=$5 nat=nat$1 peer=peer$1
	# WAN：natX.wan ($net.$i) ↔ inet.vX ($net.254)
	ip link add wan netns "$nat" type veth peer name "v$s" netns inet
	ip -n inet addr add "$net.254/24" dev "v$s"
	ip -n inet link set "v$s" up
	ip -n "$nat" addr add "$net.$i/24" dev wan
	ip -n "$nat" link set wan up
	ip -n "$nat" route add default via "$net.254"
	# LAN：peerX.eth0 ↔ natX.lan
	ip link add eth0 netns "$peer" type veth peer name lan netns "$nat"
	ip -n "$nat" addr add "10.0.$i.1/24" dev lan
	ip -n "$nat" link set lan up
	ip -n "$peer" addr add "10.0.$i.2/24" dev eth0
	ip -n "$peer" link set eth0 up
	ip -n "$peer" route add default via "10.0.$i.1"
	ip netns exec "$nat" sysctl -qw net.ipv4.ip_forward=1
	nat_rules "$nat" "10.0.$i.2" "$type"
	# netem 只作用于出方向，所以接入链路两端各挂一个，双向都生效
	if [[ -n $netem ]]; then
		ip netns exec "$nat" tc qdisc add dev wan root netem $netem
		ip netns exec inet tc qdisc add dev "v$s" root netem $netem
	fi
}

setup() { # <natA> <natB> <netemA> <netemB>
	mount -t tmpfs none /run
	mkdir -p /run/netns
	for ns in inet natA natB peerA peerB; do
		ip netns add "$ns"
		ip -n "$ns" link set lo up
	done
	ip -n inet addr add 192.0.2.1/32 dev lo
	ip netns exec inet sysctl -qw net.ipv4.ip_forward=1
	add_side A 1 203.0.113 "$1" "$3"
	add_side B 2 198.51.100 "$2" "$4"
}

# 跑一次 punch，输出一行: <A 结果>|<B 结果>|<RTT>
run_punch() { # <bin> <logdir> <timeout> <spray>
	local bin=$1 log=$2 t=$3 spray=$4
	ip netns exec inet "$bin" server -listen 192.0.2.1:7000 >"$log/server.log" 2>&1 &
	local srv=$!
	sleep 0.2
	ip netns exec peerA timeout $((t + 8)) "$bin" peer -server 192.0.2.1:7000 -id A \
		-timeout "${t}s" -spray "$spray" $ARGS_A </dev/null >"$log/A.log" 2>&1 &
	local pa=$!
	sleep 0.1
	ip netns exec peerB timeout $((t + 8)) "$bin" peer -server 192.0.2.1:7000 -id B \
		-timeout "${t}s" -spray "$spray" $ARGS_B </dev/null >"$log/B.log" 2>&1 &
	local pb=$!
	wait $pa $pb || true
	kill $srv 2>/dev/null || true
	wait $srv 2>/dev/null || true
}

# 传输层测试：接收端在 inet（相当于公网服务器），发送端在 peerA 的 NAT 后面
run_xfer() { # <logdir>
	local log=$1
	ip netns exec inet "$XFER_BIN" recv -mode "$XFER_MODE" -listen 192.0.2.1:9000 $XFER_ARGS >"$log/recv.log" 2>&1 &
	local rp=$!
	# 先 ping 一次把沿途 ARP 解析好：新建的网络里 ARP 等待队列很小，关键帧一次涌出几百个包会被它丢掉
	ip netns exec peerA ping -c 1 -W 1 192.0.2.1 >/dev/null 2>&1 || true
	sleep 0.3
	ip netns exec peerA "$XFER_BIN" send -mode "$XFER_MODE" -to 192.0.2.1:9000 $XFER_ARGS >"$log/send.log" 2>&1 || true
	wait $rp || true
}

inner() {
	local mode=$1 bin=$2 log=$3 ta=$4 tb=$5 netem_a=$6 netem_b=$7 t=$8 spray=$9
	setup "$ta" "$tb" "$netem_a" "$netem_b"
	case $mode in
	run) run_punch "$bin" "$log" "$t" "$spray" ;;
	xfer) run_xfer "$log" ;;
	script) BIN_DIR=$(dirname "$bin") bash "$SCRIPT" ;;
	shell)
		echo "实验宇宙已就绪：natA=$ta natB=$tb。命名空间: inet natA natB peerA peerB"
		echo "  会合服务器: ip netns exec inet $bin server -listen 192.0.2.1:7000"
		echo "  对等端:     ip netns exec peerA $bin peer -server 192.0.2.1:7000 -id A"
		echo "退出 shell 后整个宇宙自动销毁。"
		PS1="[uulab $ta/$tb] \w\$ " bash --norc -i
		;;
	esac
}

# ---------------- 宇宙外部 ----------------

# 从 peer 日志里取 "✔ connected to B via 1.2.3.4:5 [种类, 原因]"
summarize() { # <logdir> → "A结果|B结果|RTT"
	local log=$1 a b rtt
	a=$(grep -o 'via [^ ]*' "$log/A.log" | head -1 | cut -d' ' -f2) || true
	b=$(grep -o 'via [^ ]*' "$log/B.log" | head -1 | cut -d' ' -f2) || true
	rtt=$(grep -o 'p50 [^ ]*' "$log/A.log" | head -1 | cut -d' ' -f2) || true
	echo "${a:-✘}|${b:-✘}|${rtt:--}"
}

# 生日攻击角色：只有 port↔sym 需要（full 不用打，sym↔sym 两边都随机，生日攻击也救不了）
bday_role() { # <本端类型> <对端类型>
	[[ $BDAY == 1 ]] || return 0
	[[ $1 == sym && $2 == port ]] && echo "-bday-sockets 256"
	[[ $1 == port && $2 == sym ]] && echo "-bday-probes $BDAY_PROBES"
	return 0
}

once() { # <natA> <natB> → 在新宇宙里跑一次，返回 summarize 结果
	local log
	log=$(mktemp -d "$WORK/run.XXXX")
	# 通过环境变量传进宇宙
	export ARGS_A="$PUNCH_ARGS $(bday_role "$1" "$2")" ARGS_B="$PUNCH_ARGS $(bday_role "$2" "$1")"
	unshare -Urnm bash "$HERE/lab.sh" __inner run "$BIN" "$log" "$1" "$2" \
		"$NETEM_A" "$NETEM_B" "$TIMEOUT" "$SPRAY" >"$log/lab.log" 2>&1 || true
	if [[ $VERBOSE == 1 ]]; then
		for f in lab server A B; do
			echo "----- $1/$2 $f.log -----"
			cat "$log/$f.log" 2>/dev/null || true
		done >&2
	fi
	summarize "$log"
}

main() {
	[[ ${1:-} == __inner ]] && { shift; inner "$@"; return; }

	local cmd=${1:-} ta="" tb=""
	[[ -n $cmd ]] || die "用法见文件头注释"
	shift
	if [[ $cmd == xfer ]]; then
		[[ $# -ge 1 ]] || die "xfer 需要传输方式: own | quic | tcp"
		export XFER_MODE=$1
		ta=port tb=port
		shift
	elif [[ $cmd == run || $cmd == shell ]]; then
		[[ $# -ge 2 ]] || die "$cmd 需要两个 NAT 类型"
		ta=$1 tb=$2
		shift 2
	elif [[ $cmd == script ]]; then
		[[ $# -ge 3 ]] || die "script 需要两个 NAT 类型和脚本"
		ta=$1 tb=$2
		export SCRIPT
		SCRIPT=$(realpath "$3")
		shift 3
	fi
	NETEM_A="" NETEM_B="" SPRAY=0 TIMEOUT=6 VERBOSE=0 PUNCH_ARGS="" BDAY=0 BDAY_PROBES=1024 REPEAT=1
	export XFER_ARGS=""
	export NO_FW=0 # 通过环境变量传进 unshare 后的宇宙
	while [[ $# -gt 0 ]]; do
		case $1 in
		--netem-a) NETEM_A=$2; shift 2 ;;
		--netem-b) NETEM_B=$2; shift 2 ;;
		--spray) SPRAY=$2; shift 2 ;;
		--timeout) TIMEOUT=$2; shift 2 ;;
		--no-fw) NO_FW=1; shift ;;
		--punch-args) PUNCH_ARGS=$2; shift 2 ;;
		--bday) BDAY=1; shift ;;
		--bday-probes) BDAY=1 BDAY_PROBES=$2; shift 2 ;;
		--repeat) REPEAT=$2; shift 2 ;;
		--xfer-args) XFER_ARGS=$2; shift 2 ;;
		-v) VERBOSE=1; shift ;;
		*) die "未知选项: $1" ;;
		esac
	done

	WORK=$(mktemp -d /tmp/uulab.XXXX)
	trap 'rm -rf "$WORK"' EXIT
	BIN=$WORK/punch
	export XFER_BIN=$WORK/xfer
	(cd "$HERE/.." && CGO_ENABLED=0 go build -o "$BIN" ./punch && CGO_ENABLED=0 go build -o "$XFER_BIN" ./xfer)
	[[ $cmd == script ]] && (cd "$HERE/.." && CGO_ENABLED=0 go build -o "$WORK/p2pcp" ./p2pcp)

	case $cmd in
	run)
		IFS='|' read -r a b rtt < <(once "$ta" "$tb")
		echo "natA=$ta natB=$tb  A→$a  B→$b  RTT p50=$rtt"
		;;
	xfer)
		local log
		log=$(mktemp -d "$WORK/xfer.XXXX")
		unshare -Urnm bash "$HERE/lab.sh" __inner xfer "$BIN" "$log" "$ta" "$tb" \
			"$NETEM_A" "$NETEM_B" "$TIMEOUT" "$SPRAY" >"$log/lab.log" 2>&1 || true
		[[ $VERBOSE == 1 ]] && cat "$log/lab.log" >&2
		cat "$log/send.log" "$log/recv.log"
		;;
	shell)
		unshare -Urnm bash "$HERE/lab.sh" __inner shell "$BIN" "$WORK" "$ta" "$tb" \
			"$NETEM_A" "$NETEM_B" "$TIMEOUT" "$SPRAY"
		;;
	script)
		unshare -Urnm bash "$HERE/lab.sh" __inner script "$BIN" "$WORK" "$ta" "$tb" \
			"$NETEM_A" "$NETEM_B" "$TIMEOUT" "$SPRAY"
		;;
	matrix)
		echo "| A \\ B |$(printf ' %s |' "${TYPES[@]}")"
		echo "|---|$(printf -- '---|%.0s' "${TYPES[@]}")"
		for a in "${TYPES[@]}"; do
			local row="| **$a** |"
			for b in "${TYPES[@]}"; do
				local ok=0 half=0
				for ((r = 0; r < REPEAT; r++)); do
					IFS='|' read -r ra rb rtt < <(once "$a" "$b")
					if [[ $ra != ✘ && $rb != ✘ ]]; then
						ok=$((ok + 1))
					elif [[ $ra != ✘ || $rb != ✘ ]]; then
						half=$((half + 1)) # 只有一边认为连上了
					fi
				done
				local mark=❌ note=""
				if ((ok == REPEAT)); then mark=✅; elif ((ok > 0)); then mark=⚠; fi
				if ((half > 0)); then note=" (单边 $half)"; fi
				row+=" $mark $ok/$REPEAT$note |"
			done
			echo "$row"
		done
		;;
	*) die "未知命令: $cmd" ;;
	esac
}

main "$@"
