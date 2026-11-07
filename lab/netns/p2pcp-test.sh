#!/usr/bin/env bash
# p2pcp 在 netns NAT 实验室里的回归测试（由 lab.sh script 在"实验宇宙"里执行，不需要 root）：
#
#   lab.sh script port port p2pcp-test.sh   # TESTS 环境变量选用例，默认全跑
#   lab.sh script sym  sym  p2pcp-test.sh   # 对称 NAT 两端：打不通，走中继
#
# 用例：
#   send     两个文件、-conns 2 共用一个洞，比对 sha256
#   resume   发送端传到一半被杀，重发时只补剩下的
#   reconn   传到一半路径断 20 秒，发送端自己重连续传
#   tunnel   expose/forward 回显；中途三次把 forward 正在用的 UDP 端口封掉，每次都迁移到新洞上，TCP 会话不断
#   upgrade  forward 先强制走中继，15 秒后自己打通直连并迁移过去，TCP 会话不断
#
# 宇宙里：peerA 10.0.1.2（NAT A 后）发送 / forward，peerB 10.0.2.2（NAT B 后）接收 / expose，会合服务器 192.0.2.1。
set -uo pipefail
B=$BIN_DIR
W=$(mktemp -d /tmp/p2pcp-test.XXXX)
SRV=192.0.2.1:7001,192.0.2.1:7000
ROOM=lab-$RANDOM-$RANDOM
TESTS=${TESTS:-send resume reconn tunnel upgrade}
A() { ip netns exec peerA "$@"; }
Bn() { ip netns exec peerB "$@"; }
# 后台跑：$! 就是 p2pcp 进程本身（ip netns exec 会 exec 过去），kill 得到
bgA() { ip netns exec peerA "$@" & }
bgB() { ip netns exec peerB "$@" & }
pass=0 fail=0
ok() { echo "  ✅ $*"; pass=$((pass + 1)); }
bad() { echo "  ❌ $*"; fail=$((fail + 1)); }

ip netns exec inet "$B/punch" server -listen 192.0.2.1:7000,192.0.2.1:7001 -relay 192.0.2.1:3478 >"$W/server.log" 2>&1 &
# 收尾：不管哪一步出错，后台进程都杀掉
trap 'kill $(jobs -p) 2>/dev/null' EXIT
SRVPID=$!
# 把沿途 ARP 先解析好
A ping -c1 -W1 192.0.2.1 >/dev/null 2>&1
Bn ping -c1 -W1 192.0.2.1 >/dev/null 2>&1
sleep 0.3

# 回显服务（在 peerB 上，expose 转发到它）和回显客户端（在 peerA 上，经 forward）
cat >"$W/echo.py" <<'EOF'
import socket, sys, time, threading
if sys.argv[1] == "server":
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", 9000)); s.listen()
    def h(c):
        while (d := c.recv(4096)): c.sendall(d)
    while True:
        c, _ = s.accept(); threading.Thread(target=h, args=(c,), daemon=True).start()
else:
    # 每 0.3 秒发一行、等回显，统计最长的停顿；连接断了就算失败
    dur = float(sys.argv[2]); c = socket.create_connection(("127.0.0.1", 2223), timeout=40)
    t0 = time.time(); n = 0; gap = 0; last = time.time(); f = c.makefile("rb")
    while time.time() - t0 < dur:
        c.sendall(b"%d\n" % n)
        line = f.readline()
        if not line: print("EOF"); sys.exit(1)
        assert int(line) == n
        now = time.time(); gap = max(gap, now - last); last = now; n += 1
        time.sleep(0.3)
    print(f"echo ok n={n} maxgap={gap:.1f}s")
EOF

t_send() {
	echo "== send：两个文件，-conns 2 共用一个洞"
	head -c 30000000 /dev/urandom >"$W/big.bin"
	echo hello >"$W/small.txt"
	mkdir -p "$W/out"
	bgB "$B/p2pcp" recv -server "$SRV" -room "$ROOM" -o "$W/out" >"$W/send-recv.log" 2>&1
	local r=$!
	A "$B/p2pcp" send -server "$SRV" -room "$ROOM" -conns 2 "$W/big.bin" "$W/small.txt" >"$W/send-send.log" 2>&1
	wait $r
	if cmp -s "$W/big.bin" "$W/out/big.bin" && cmp -s "$W/small.txt" "$W/out/small.txt"; then
		ok "两个文件都对（$(grep -o '连上 [^，]*' "$W/send-send.log" | head -1)）"
	else
		bad "文件不对"; tail -n 5 "$W/send-send.log" "$W/send-recv.log"
	fi
	if grep -q '改走中继' "$W/send-send.log"; then
		grep -q '\[1\] SRTT' "$W/send-send.log" && bad "走中继时不该开第 2 条连接" || ok "走中继，只用 1 条连接（$(grep -o '第 [0-9] 轮失败' "$W/send-send.log" | wc -l) 轮打洞失败后）"
	else
		grep -q '\[1\] SRTT' "$W/send-send.log" && ok "开了第 2 条连接" || bad "没开第 2 条连接"
	fi
	ls -A "$W/out" | grep -q '^\.p2pcp-' && bad "留下了半成品" || ok "没有半成品残留"
}

t_resume() {
	echo "== resume：发送端传到一半被杀，重发只补剩下的"
	head -c 40000000 /dev/urandom >"$W/r.bin"
	mkdir -p "$W/rout"
	bgB "$B/p2pcp" daemon -server "$SRV" -room "$ROOM.r" -o "$W/rout" >"$W/resume-recv.log" 2>&1
	local r=$!
	# 限速 40Mbit，8 秒大概传一大半
	ip netns exec natA tc qdisc add dev wan root tbf rate 40mbit burst 64kb latency 50ms
	A timeout 6 "$B/p2pcp" send -server "$SRV" -room "$ROOM.r" -conns 1 "$W/r.bin" >"$W/resume-send1.log" 2>&1
	sleep 1
	local part
	part=$(ls "$W/rout"/.p2pcp-*.part 2>/dev/null | head -1)
	[[ -n $part ]] && ok "被杀后留下了半成品 $(basename "$part")" || bad "没有半成品"
	A "$B/p2pcp" send -server "$SRV" -room "$ROOM.r" -conns 1 "$W/r.bin" >"$W/resume-send2.log" 2>&1
	ip netns exec natA tc qdisc del dev wan root
	sleep 0.5
	kill $r 2>/dev/null
	if cmp -s "$W/r.bin" "$W/rout/r.bin"; then ok "续传后文件正确"; else bad "续传后文件不对"; tail -n 5 "$W/resume-send2.log" "$W/resume-recv.log"; fi
	grep -q '续传' "$W/resume-send2.log" && ok "$(grep -o '续传 [^ ]*：对方已有 [^M]*MB' "$W/resume-send2.log" | head -1)" || bad "第二次没有续传"
}

t_reconn() {
	echo "== reconn：传到一半路径断 20 秒，发送端自己重连续传"
	head -c 40000000 /dev/urandom >"$W/c.bin"
	mkdir -p "$W/cout"
	bgB "$B/p2pcp" daemon -server "$SRV" -room "$ROOM.c" -o "$W/cout" >"$W/reconn-recv.log" 2>&1
	local r=$!
	ip netns exec natA tc qdisc add dev wan root tbf rate 40mbit burst 64kb latency 50ms
	bgA "$B/p2pcp" send -server "$SRV" -room "$ROOM.c" -conns 1 "$W/c.bin" >"$W/reconn-send.log" 2>&1
	local s=$!
	sleep 4
	# 断掉 A 往外的 UDP（会合也一起断，模拟换网 / 信号丢失），20 秒后恢复
	ip netns exec natA nft add table ip cut
	ip netns exec natA nft add chain ip cut blk '{ type filter hook forward priority -10; }'
	ip netns exec natA nft add rule ip cut blk ip saddr 10.0.1.2 meta l4proto udp drop
	sleep 20
	ip netns exec natA nft delete table ip cut
	wait $s
	ip netns exec natA tc qdisc del dev wan root
	sleep 0.5
	kill $r 2>/dev/null
	if cmp -s "$W/c.bin" "$W/cout/c.bin"; then ok "断网 20 秒后自己续传完成"; else bad "文件不对"; tail -n 8 "$W/reconn-send.log" "$W/reconn-recv.log"; fi
	grep -q '重连续传' "$W/reconn-send.log" && ok "$(grep -o '连接断了.*重连续传' "$W/reconn-send.log" | head -1 | cut -c1-80)" || bad "没有重连"
}

start_tunnel() { # <名字> <forward 额外参数>
	bgB python3 "$W/echo.py" server
	ECHO=$!
	bgB env QUIC_GO_LOG_LEVEL=${QDEBUG:-} "$B/p2pcp" expose -server "$SRV" -room "$ROOM.$1" -to 127.0.0.1:9000 >"$W/$1-expose.log" 2>&1
	EXP=$!
	bgA env QUIC_GO_LOG_LEVEL=${QDEBUG:-} "$B/p2pcp" forward -server "$SRV" -room "$ROOM.$1" -listen 127.0.0.1:2223 $2 >"$W/$1-forward.log" 2>&1
	FWD=$!
	sleep 0.5
}

stop_tunnel() {
	kill $FWD $EXP $ECHO 2>/dev/null
	wait $FWD $EXP $ECHO 2>/dev/null
}

t_tunnel() {
	echo "== tunnel：路径连断三次，每次都迁移到新洞上，TCP 会话不断"
	start_tunnel tunnel ""
	bgA python3 "$W/echo.py" client 40 >"$W/tunnel-echo.log" 2>&1
	local c=$! i port
	A nft add table ip cut
	A nft add chain ip cut blk '{ type filter hook output priority 0; }'
	A nft add chain ip cut blkin '{ type filter hook input priority 0; }'
	for i in 1 2 3; do
		sleep 9
		# 封掉 forward 当前 QUIC socket 的端口，进出都封，相当于那块网卡没了（新开的 socket 不受影响）。
		# 当前 socket：迁移过就是日志里最后一个"本端"，还没迁移过时只有一个 socket
		port=$(grep -o '本端 [^）]*' "$W/tunnel-forward.log" | tail -1 | sed 's/.*://')
		[[ -n $port ]] || port=$(A ss -Huanp | awk '/p2pcp/ {print $4}' | sed 's/.*://' | head -1)
		A nft add rule ip cut blk udp sport "$port" drop
		A nft add rule ip cut blkin udp dport "$port" drop
		echo "  第 $i 次：封掉 peerA UDP 端口 $port"
	done
	wait $c && ok "TCP 会话没断：$(cat "$W/tunnel-echo.log")" || bad "TCP 会话断了：$(cat "$W/tunnel-echo.log")"
	local n
	n=$(grep -c '连接已迁移' "$W/tunnel-forward.log")
	[[ $n -ge 3 ]] && ok "迁移了 $n 次：$(grep -o '用时 [^；]*' "$W/tunnel-forward.log" | tr '\n' ' ')" || { bad "只迁移了 $n 次"; tail -n 12 "$W/tunnel-forward.log" "$W/tunnel-expose.log"; }
	A nft delete table ip cut
	stop_tunnel
}

t_upgrade() {
	echo "== upgrade：先走中继，后台打通直连后迁移过去"
	start_tunnel upgrade "-relay always"
	A python3 "$W/echo.py" client 25 >"$W/upgrade-echo.log" 2>&1
	[[ $? == 0 ]] && ok "TCP 会话没断：$(cat "$W/upgrade-echo.log")" || bad "TCP 会话断了：$(cat "$W/upgrade-echo.log")"
	grep -q '改走中继' "$W/upgrade-forward.log" && ok "先走了中继" || bad "没走中继"
	if grep -q '连接已迁移' "$W/upgrade-forward.log"; then
		ok "$(grep -o '连接已迁移.*用时 [^；]*' "$W/upgrade-forward.log" | head -1)"
	else
		echo "  · 没迁到直连（对称 NAT 两端时打不通是对的）：$(grep -o '迁移失败.*' "$W/upgrade-forward.log" | tail -1 | cut -c1-90)"
	fi
	stop_tunnel
}

for t in $TESTS; do "t_$t"; done
kill $SRVPID 2>/dev/null
echo
echo "通过 $pass，失败 $fail（日志在 $W）"
[[ -n ${KEEP_LOGS:-} ]] || [[ $fail -gt 0 ]] || rm -rf "$W"
