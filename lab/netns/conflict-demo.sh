#!/usr/bin/env bash
# conntrack 冲突演示：在中间的 inet 路由器上抓包，看 B 的出口端口有没有被换掉、A 的包有没有被 natB 自己吞掉。在实验宇宙里跑：
#   lab.sh script port port conflict-demo.sh --no-fw                       # 对方 NAT 不设防：B 被换端口，双方僵死
#   lab.sh script sym restricted conflict-demo.sh --no-fw                  # B 没换端口，但 A 的包一直被 natB 收下（ICMP 端口不可达）
#   PUNCH_EXTRA="-ttl 2" lab.sh script port port conflict-demo.sh --no-fw  # 加上低 TTL 预热：打通
#   lab.sh script port port conflict-demo.sh                               # NAT 在 INPUT 丢弃未请求包：打通
#
# B 晚 0.5 秒启动，保证 A 的 PUNCH 先到 natB（真实网络里两端拿到地址的时刻本来就有先后）。
set -u
P=$BIN_DIR/punch
HERE=$(cd "$(dirname "$0")" && pwd)
log=$(mktemp -d)

ip netns exec inet "$P" server -listen 192.0.2.1:7000 >"$log/server.log" 2>&1 &
srv=$!
sleep 0.2
ip netns exec inet python3 "$HERE/sniff.py" any 1.9 60 >"$log/sniff.txt" &
sn=$!
ip netns exec peerA timeout 6 "$P" peer -server 192.0.2.1:7000 -id A -timeout 4s ${PUNCH_EXTRA:-} </dev/null >"$log/A.log" 2>&1 &
pa=$!
sleep 0.5
ip netns exec peerB timeout 6 "$P" peer -server 192.0.2.1:7000 -id B -timeout 4s ${PUNCH_EXTRA:-} </dev/null >"$log/B.log" 2>&1 &
pb=$!
wait $sn
echo "== 会合服务器看到的映射"
grep -o 'id=[AB] pub=[^ ]*' "$log/server.log"
echo "== inet 上的抓包（时间 进来的网卡 到达时的 TTL 源 → 目的 内容；ttl=1 的包 inet 不会再转发；ICMP 端口不可达 = 原包被那台 NAT 自己收下了）"
grep -v ' → 10\.' "$log/sniff.txt" # 发往对方内网候选的包在 inet 上没有路由，略去
wait $pa $pb
echo "== 结果"
grep -hE '✔|✘' "$log/A.log" "$log/B.log" | sed 's/^[0-9/]* [0-9:]* //'
kill $srv 2>/dev/null
rm -rf "$log"
