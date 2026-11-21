#!/bin/bash
# p2pcp 与 sendme（iroh 1.x）对比实测：PC（家宽，开着 sing-box TUN）↔ 手机（USB 调试，走电信 5G）。
# 两边都用默认配置：p2pcp 走 99-server 会合（2 条连接 + BBR），sendme 走 n0 的公共中继。
#
#   vs-sendme.sh prep [MB]     两端各生成一个随机文件（默认 50MB），记下 sha256
#   vs-sendme.sh run <轮数>     每轮依次跑 p2pcp / sendme × 手机→PC / PC→手机
#   OWN="https://<中继>/?token=..." vs-sendme.sh run3 <轮数>   再加一路 sendme 走自建 iroh-relay
#   PX="-conns 1 -cc cubic" TOOL=p2pcp-1c-cubic vs-sendme.sh pc2ph-only <轮数>
#   vs-sendme-parse.py $OUT     汇总
#
# 计时口径：先让一端就绪（p2pcp 是接收端先挂着，sendme 是发送端先拿到 ticket），
# 从「后上线的那一端」启动命令开始计，到它退出为止。房间名每轮随机，不落盘。
# 依赖：PC 上 /tmp/p2pcp 与 sendme，手机上 /data/local/tmp/{p2pcp,sendme}。
set -u
OUT=${OUT:-/tmp/vs}
SRV=${SRV:-$(cat ~/.config/p2pcp/server)}
BIND=$(ip -4 -o addr show wlan0 | awk '{print $4}' | cut -d/ -f1)
PCB=/tmp/p2pcp
PH=/data/local/tmp
F=vs.bin
PX=${PX:-}                # 只用于 p2pcp PC→手机 的额外参数，如 "-conns 1 -cc cubic"
TOOL=${TOOL:-p2pcp}       # 配合 PX 给结果起名，如 p2pcp-1c-cubic
SMR=default               # sendme 的 --relay：default = n0 公共中继
SMT=sendme                # sendme 结果的名字
OWN=${OWN:-}              # 自建 iroh-relay 的 URL（含 ?token=），给 run3 用；别写进仓库
EV='iroh::_events=debug,iroh::net_report=debug,warn'
now() { date +%s%N; }
ms() { echo $((($2 - $1) / 1000000)); }
room() { head -c8 /dev/urandom | xxd -p; }

pc_rx() { rm -rf "$OUT/rx" && mkdir -p "$OUT/rx"; }
ph_rx() { adb shell "rm -rf $PH/vs/rx && mkdir -p $PH/vs/rx"; }
pc_sum() { sha256sum "$OUT/rx/$F" 2>/dev/null | cut -c1-16; }
ph_sum() { adb shell "sha256sum $PH/vs/rx/$F 2>/dev/null" | cut -c1-16; }

# 等 sendme 发送端打印 ticket
ticket() {
	local i
	for i in $(seq 120); do
		T=$(grep -o 'blob[a-z0-9]*' "$1" | head -1)
		[ -n "$T" ] && return 0
		sleep 0.25
	done
	return 1
}

record() { # 工具 方向 轮 毫秒 sha
	local want=$PHSUM
	[ "$2" = pc2ph ] && want=$PCSUM
	local ok=BAD
	[ "$5" = "$want" ] && ok=ok
	printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$ok" | tee -a "$OUT/results.tsv"
}

p2pcp_ph2pc() {
	local L=$OUT/p2pcp-ph2pc-$1 R
	R=$(room)
	pc_rx
	timeout 150 $PCB recv -room "$R" -o "$OUT/rx" -bind auto >"$L.recv.log" 2>&1 &
	local RP=$!
	sleep 1
	local t0 t1
	t0=$(now)
	adb shell "cd $PH/vs/src && timeout 140 $PH/p2pcp send -server $SRV -room $R $F" >"$L.send.log" 2>&1
	t1=$(now)
	wait $RP
	record p2pcp ph2pc "$1" "$(ms "$t0" "$t1")" "$(pc_sum)"
}

p2pcp_pc2ph() {
	local L=$OUT/$TOOL-pc2ph-$1 R
	R=$(room)
	ph_rx
	adb shell "timeout 150 $PH/p2pcp recv -server $SRV -room $R -o $PH/vs/rx" >"$L.recv.log" 2>&1 &
	local RP=$!
	sleep 1.5
	local t0 t1
	t0=$(now)
	(cd "$OUT/src" && timeout 140 $PCB send -server "$SRV" -room "$R" -bind auto $PX $F) >"$L.send.log" 2>&1
	t1=$(now)
	wait $RP
	record "$TOOL" pc2ph "$1" "$(ms "$t0" "$t1")" "$(ph_sum)"
}

sendme_ph2pc() {
	local L=$OUT/$SMT-ph2pc-$1
	adb shell "cd $PH/vs/src && RUST_LOG='$EV' timeout 180 $PH/sendme send $F --no-progress --relay '$SMR'" >"$L.send.log" 2>&1 &
	local SP=$!
	ticket "$L.send.log" || { echo "sendme ph2pc $1: 没拿到 ticket"; adb shell pkill -f $PH/sendme; wait $SP; return; }
	pc_rx
	local t0 t1
	t0=$(now)
	(cd "$OUT/rx" && RUST_LOG=$EV timeout 140 sendme receive --magic-ipv4-addr "$BIND:0" --no-progress --relay "$SMR" "$T") >"$L.recv.log" 2>&1
	t1=$(now)
	adb shell pkill -f $PH/sendme
	wait $SP
	record "$SMT" ph2pc "$1" "$(ms "$t0" "$t1")" "$(pc_sum)"
}

sendme_pc2ph() {
	local L=$OUT/$SMT-pc2ph-$1
	(cd "$OUT/src" && RUST_LOG=$EV timeout 180 sendme send --magic-ipv4-addr "$BIND:0" --no-progress --relay "$SMR" $F) >"$L.send.log" 2>&1 &
	local SP=$!
	ticket "$L.send.log" || { echo "sendme pc2ph $1: 没拿到 ticket"; kill $SP; wait $SP; return; }
	ph_rx
	local t0 t1
	t0=$(now)
	adb shell "cd $PH/vs/rx && RUST_LOG='$EV' timeout 140 $PH/sendme receive --no-progress --relay '$SMR' $T" >"$L.recv.log" 2>&1
	t1=$(now)
	pkill -f "sendme send --magic-ipv4-addr $BIND"
	wait $SP
	record "$SMT" pc2ph "$1" "$(ms "$t0" "$t1")" "$(ph_sum)"
}

case $1 in
prep)
	MB=${2:-50}
	mkdir -p "$OUT/src"
	head -c $((MB * 1048576)) /dev/urandom >"$OUT/src/$F"
	adb shell "mkdir -p $PH/vs/src && head -c $((MB * 1048576)) /dev/urandom > $PH/vs/src/$F"
	sha256sum "$OUT/src/$F" | cut -c1-16 >"$OUT/pc.sum"
	adb shell "sha256sum $PH/vs/src/$F" | cut -c1-16 >"$OUT/ph.sum"
	echo "PC $(cat "$OUT/pc.sum")  手机 $(cat "$OUT/ph.sum")  各 ${MB}MB"
	;;
run)
	PCSUM=$(cat "$OUT/pc.sum")
	PHSUM=$(cat "$OUT/ph.sum")
	for r in $(seq "${2:-3}"); do
		p2pcp_ph2pc "$r"
		sendme_ph2pc "$r"
		p2pcp_pc2ph "$r"
		sendme_pc2ph "$r"
	done
	;;
run3) # p2pcp / sendme（n0 中继）/ sendme（自建中继 $OWN）交替
	[ -n "$OWN" ] || { echo "要先设 OWN=https://中继/?token=..."; exit 1; }
	PCSUM=$(cat "$OUT/pc.sum")
	PHSUM=$(cat "$OUT/ph.sum")
	for r in $(seq "${2:-3}"); do
		for d in ph2pc pc2ph; do
			p2pcp_$d "$r"
			SMR=default SMT=sendme sendme_$d "$r"
			SMR=$OWN SMT=sendme-own sendme_$d "$r"
		done
	done
	;;
pc2ph-only) # 只跑 p2pcp PC→手机，配合 PX / TOOL 拆拥塞控制和连接数的影响
	PCSUM=$(cat "$OUT/pc.sum")
	PHSUM=$(cat "$OUT/ph.sum")
	for r in $(seq "${2:-2}"); do p2pcp_pc2ph "$r"; done
	;;
*) sed -n '2,12p' "$0" ;;
esac
