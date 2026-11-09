#!/usr/bin/env bash
# 过滤行为演示：natA 到底核对入包的哪些字段。在实验宇宙里跑：
#   lab.sh script <full|restricted|port> port filter-check.sh
#
# peerA 在 5000 端口收包，按顺序有四次敲门：
#   阶段 1（A 只往 inet 192.0.2.1:9 发过包）
#     inet:7777  同一个 IP、换了端口
#     B:4000     A 从没联系过的 IP
#   阶段 2（A 又往 B:4000 发了一个包）
#     B:4000     刚好是 A 联系过的 IP + 端口
#     B:4001     同一个 IP、换了端口
# 预期：full 四次都收到（EIF），restricted 收到 inet:7777、B:4000、B:4001（ADF），port 只收到阶段 2 的 B:4000（APDF）
set -u
ip netns exec peerA python3 -u -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("0.0.0.0", 5000))
def drain(sec):
    s.settimeout(sec)
    try:
        while True:
            d, a = s.recvfrom(100)
            print("  A 收到", d.decode())
    except socket.timeout:
        pass
s.sendto(b"open", ("192.0.2.1", 9))
drain(4)
s.sendto(b"open", ("198.51.100.2", 4000))
drain(3)
' &
pa=$!

knock() { # <ns> <源端口> <说明> [源 IP]
	ip netns exec "$1" python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(('${4:-0.0.0.0}', $2))
s.sendto('$3'.encode(), ('203.0.113.1', 5000))"
}

sleep 1
echo "阶段 1：A 只往 inet 192.0.2.1:9 发过包"
knock inet 7777 "inet:7777（同 IP、换端口）" 192.0.2.1 # inet 默认会用 203.0.113.254 当源地址，要显式绑定
knock peerB 4000 "B:4000（陌生 IP）"
sleep 4
echo "阶段 2：A 又往 B:4000 发了一个包"
knock peerB 4000 "B:4000（联系过的 IP + 端口）"
knock peerB 4001 "B:4001（同 IP、换端口）"
wait $pa
