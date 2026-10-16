#!/bin/bash
# PC（家宽）↔ 手机（USB 调试、走移动网络）的真实网络打洞测试，见 docs/08。
# 会合服务器由本脚本代劳：把双方的 STUN 映射地址写进对方的 -file。
#
#   pcphone.sh deploy                     交叉编译 natprobe 并推到手机
#   pcphone.sh stun                       两端的映射行为 + 端口分配
#   pcphone.sh filter pc|phone            测某一端的过滤：陌生 IP / 同 IP 换端口能不能进来
#   pcphone.sh order pc|phone <秒>        一端先单独发 N 秒再让另一端开始，看会不会被换端口
#   pcphone.sh setup "<peer 参数>" <轮数>  双方同时开始，统计建连耗时（如 "-trigger -interval 20ms"）
#   pcphone.sh interval <间隔> <时长>       打通后按固定间隔互发，看 RTT 分布
#   pcphone.sh xfer pc2ph|ph2pc <码率> [时长]  打洞后在直连路径上跑 xfer own 视频流（码率如 20000000）
#
# 环境变量：BIND（PC 物理网卡地址，开着 TUN 时必须）、STUN（公共 STUN 的 IP:端口）
set -e
cd "$(dirname "$0")/.."
BIND=${BIND:-$(ip -4 -o addr show wlan0 | awk '{print $4}' | cut -d/ -f1)}
STUN=${STUN:-111.206.174.2:3478}
STUNS=${STUNS:-111.206.174.2:3478,106.12.251.193:3478,162.159.207.0:3478,111.206.174.3:3478}
PCBIN=/tmp/natprobe
PHBIN=/data/local/tmp/natprobe
PCF=/tmp/peer-pc.txt
PHF=/data/local/tmp/peer.txt
PC() { $PCBIN peer -stun $STUN -bind $BIND "$@"; }
PH() { adb shell $PHBIN peer -stun $STUN "$@"; }
mapped() { sed -n 's/.*MAPPED //p' "$1"; }
reset_files() { rm -f $PCF; adb shell rm -f $PHF; }
give_pc() { echo "$1" > $PCF; }
give_ph() { adb shell "echo $1 > $PHF"; }

case $1 in
deploy)
	CGO_ENABLED=0 go build -o $PCBIN ./natprobe
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/natprobe-arm64 ./natprobe
	adb push /tmp/natprobe-arm64 $PHBIN >/dev/null && adb shell chmod 755 $PHBIN && echo "已部署"
	;;
stun)
	echo "##### PC"; $PCBIN stun -bind $BIND -servers $STUNS -alloc 10
	echo; echo "##### 手机"; adb shell $PHBIN stun -servers $STUNS -alloc 16
	;;
filter) # X = 被测方；X 先向 Y 的 socket1 发包，再让 Y 的 socket2 往 X 发
	reset_files
	if [ "$2" = pc ]; then
		PH -dur 12s >/tmp/f-y1.log 2>&1 &
		PC -file $PCF -dur 10s >/tmp/f-x.log 2>&1 &
		sleep 2; Y1=$(mapped /tmp/f-y1.log); X=$(mapped /tmp/f-x.log)
		give_pc $Y1; sleep 1
		PH -to $X -dur 5s >/tmp/f-y2.log 2>&1
	else
		PC -dur 12s >/tmp/f-y1.log 2>&1 &
		PH -file $PHF -dur 10s >/tmp/f-x.log 2>&1 &
		sleep 2; Y1=$(mapped /tmp/f-y1.log); X=$(mapped /tmp/f-x.log)
		give_ph $Y1; sleep 1
		PC -to $X -dur 5s >/tmp/f-y2.log 2>&1
	fi
	wait
	echo "被测方 X=$X 先向 Y1=$Y1 发包；Y2=$(mapped /tmp/f-y2.log)（同 IP 另一端口）向 X 发了 $(grep -o '发 HI [0-9]*' /tmp/f-y2.log)"
	echo "X 收到的来源:"; grep 来源 /tmp/f-x.log || echo "  （无）→ 端口受限过滤"
	;;
order)
	reset_files
	PH -file $PHF -dur 12s >/tmp/o-ph.log 2>&1 &
	PC -file $PCF -dur 11s >/tmp/o-pc.log 2>&1 &
	sleep 1.5; MP=$(mapped /tmp/o-ph.log); MC=$(mapped /tmp/o-pc.log)
	echo "手机 $MP  PC $MC  先发=$2 间隔=${3}s"
	if [ "$2" = phone ]; then give_ph $MC; sleep $3; give_pc $MP; else give_pc $MP; sleep $3; give_ph $MC; fi
	wait
	echo "== PC";   grep -E "首个|来源|打通：|RTT min" /tmp/o-pc.log
	echo "== 手机"; grep -E "首个|来源|打通：|RTT min" /tmp/o-ph.log
	;;
setup)
	for r in $(seq $3); do
		reset_files
		PH -file $PHF -dur 4s $2 >/tmp/s-ph.log 2>&1 &
		PC -file $PCF -dur 3s $2 >/tmp/s-pc.log 2>&1 &
		sleep 1.5; MP=$(mapped /tmp/s-ph.log); MC=$(mapped /tmp/s-pc.log)
		give_pc $MP; give_ph $MC
		wait
		echo "$r PC $(sed -n 's/.*双向打通：//p' /tmp/s-pc.log) 手机 $(sed -n 's/.*双向打通：//p' /tmp/s-ph.log)"
	done
	;;
interval)
	reset_files
	PH -file $PHF -interval $2 -dur $3 >/tmp/i-ph.log 2>&1 &
	PC -file $PCF -interval $2 -dur $3 >/tmp/i-pc.log 2>&1 &
	sleep 1.5; MP=$(mapped /tmp/i-ph.log); MC=$(mapped /tmp/i-pc.log)
	give_pc $MP; give_ph $MC
	wait
	echo "interval=$2  PC:   $(grep -E 'RTT min|汇总' /tmp/i-pc.log | tr '\n' ' ')"
	echo "interval=$2  手机: $(grep -E 'RTT min|汇总' /tmp/i-ph.log | tr '\n' ' ')"
	;;
xfer) # 先用固定端口打洞并测出时钟偏差，再让 xfer 复用这两个端口跑视频流
	dir=$2 rate=$3 dur=${4:-10s}
	PP=40000 CP=40001
	reset_files
	PH -port $PP -file $PHF -trigger -interval 20ms -dur 3s >/tmp/x-ph.log 2>&1 &
	PC -port $CP -file $PCF -trigger -interval 20ms -dur 3s >/tmp/x-pc.log 2>&1 &
	sleep 1.5; MP=$(mapped /tmp/x-ph.log); MC=$(mapped /tmp/x-pc.log)
	give_pc $MP; give_ph $MC
	wait
	off=$(awk '/OFFSET/{sub(/（.*/, "", $2); print $2}' /tmp/x-pc.log) # 手机时钟 - PC 时钟
	[ -n "$off" ] || { echo "打洞失败"; cat /tmp/x-pc.log; exit 1; }
	neg=$([ "${off:0:1}" = - ] && echo "${off:1}" || echo "-$off")
	echo "打洞完成：手机 $MP  PC $MC  时钟偏差(手机-PC) $off  方向 $dir  码率 $rate  时长 $dur"
	if [ "$dir" = pc2ph ]; then
		adb shell /data/local/tmp/xfer recv -mode own -listen :$PP -clock-offset $off >/tmp/x-recv.log 2>&1 &
		sleep 1
		/tmp/xfer send -mode own -to $MP -local $BIND:$CP -bitrate $rate -dur $dur >/tmp/x-send.log 2>&1
	else
		/tmp/xfer recv -mode own -listen $BIND:$CP -clock-offset $neg >/tmp/x-recv.log 2>&1 &
		sleep 1
		adb shell /data/local/tmp/xfer send -mode own -to $MC -local :$PP -bitrate $rate -dur $dur >/tmp/x-send.log 2>&1
	fi
	wait
	cat /tmp/x-send.log /tmp/x-recv.log
	;;
*)
	sed -n '2,14p' "$0"; exit 2
	;;
esac
