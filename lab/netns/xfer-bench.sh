#!/usr/bin/env bash
# 传输层对比：own / quic / tcp × 几种弱网条件，输出 Markdown 表格。
# 弱网加在发送端（peerA）的接入链路上、双向生效，所以 "delay 20ms" 意味着 RTT ≈ 40ms。
#
# 用法: xfer-bench.sh [每次时长=20s] [额外 xfer 参数...]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
DUR=${1:-20s}
shift || true
EXTRA="$*"

CONDS=(
	"delay 20ms rate 50mbit"
	"delay 20ms rate 50mbit loss 1%"
	"delay 20ms rate 50mbit loss 3%"
	"delay 20ms 5ms rate 50mbit loss 1%"
)
MODES=(own quic tcp)

echo "| 网络条件 | 传输 | 按时 | 丢失 | 迟到 | p50 | p95 | p99 | max | 关键帧 p50 | 可播放 | 冻结 |"
echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
for c in "${CONDS[@]}"; do
	for m in "${MODES[@]}"; do
		line=$("$HERE/lab.sh" xfer "$m" --netem-a "$c" --xfer-args "-dur $DUR $EXTRA" | grep '^RESULT' || true)
		if [[ -z $line ]]; then
			echo "| $c | $m | 运行失败 |||||||||||"
			continue
		fi
		python3 - "$c" "$line" <<'EOF'
import sys
c, line = sys.argv[1], sys.argv[2]
kv = dict(x.split("=", 1) for x in line.split()[1:])
print(f"| {c} | {kv['mode']} | {kv['ontime']}% | {kv['lost']} | {kv['late']} | {kv['p50']} | {kv['p95']} | "
      f"{kv['p99']} | {kv['max']} | {kv['key_p50']} | {kv['playable']}% | {kv['freezes']} 次 {int(kv['frozen_ms'])/1000:.1f}s |")
EOF
	done
done
