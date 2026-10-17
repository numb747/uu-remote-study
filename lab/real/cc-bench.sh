#!/bin/bash
# 拥塞控制 × 连接数对比：手机（adb shell）→ PC 传同一个文件，每组输出一行，见 docs/09 第 6 节。
#
#   cc-bench.sh "cubic 1" "bbr 1" "bbr 4" "brutal:30M 1" ...
#
# 环境变量：FILE（手机上的测试文件，默认 /data/local/tmp/cc150.bin）、BIND（PC 物理网卡地址）、ANDROID_SERIAL（多设备时指定手机）
set -e
cd "$(dirname "$0")/.."
FILE=${FILE:-/data/local/tmp/cc150.bin}
BIND=${BIND:-$(ip -4 -o addr show wlan0 | awk '{print $4}' | cut -d/ -f1)}
PCBIN=/tmp/p2pcp-amd64
OUT=$(mktemp -d)

CGO_ENABLED=0 go build -trimpath -o $PCBIN ./p2pcp
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /tmp/p2pcp-arm64 ./p2pcp
adb push /tmp/p2pcp-arm64 /data/local/tmp/p2pcp >/dev/null

echo "| 拥塞控制 | 连接数 | 用时 | 平均 | 各连接 SRTT | 发包 | 打洞重试 | 走的路径 |"
echo "|---|---|---|---|---|---|---|---|"
for spec in "$@"; do
	read -r cc n <<<"$spec"
	room=bench-$RANDOM$RANDOM
	$PCBIN recv -bind "$BIND" -room $room -o "$OUT" >"$OUT/recv.log" 2>&1 &
	sleep 0.5
	adb shell "/data/local/tmp/p2pcp send -room $room -conns $n -cc $cc $FILE" >"$OUT/send.log" 2>&1 || true
	wait
	find "$OUT" -type f ! -name '*.log' -delete
	t=$(grep -oE '用时 [0-9.ms]+，平均 [0-9.]+ Mbps' "$OUT/send.log" | tail -1)
	srtt=$(grep -oE 'SRTT [0-9.]+ms' "$OUT/send.log" | awk '{print $2}' | paste -sd/)
	pk=$(grep -oE '发包 [0-9]+' "$OUT/send.log" | awk '{s+=$2} END{print s}')
	re=$(cat "$OUT/send.log" "$OUT/recv.log" | grep -c '换端口重来' || true)
	if [ -n "$t" ]; then
		peer=$(grep -oE '\[0\] 打通 [0-9.:]+' "$OUT/send.log" | awk '{print $3}')
		echo "| $cc | $n | $(echo "$t" | sed -E 's/用时 ([^，]+)，.*/\1/') | $(echo "$t" | sed -E 's/.*平均 ([0-9.]+) Mbps/\1/') Mbps | $srtt | $pk | $re | $peer |"
	else
		echo "| $cc | $n | 失败 | $(grep -oE '超时.*|失败.*|没有上线' "$OUT/send.log" | head -1) | | | $re |"
	fi
	sleep 3
done
rm -rf "$OUT"
