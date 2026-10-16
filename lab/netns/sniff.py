#!/usr/bin/env python3
# 最小 UDP 抓包器：实验宇宙里没有 tcpdump，用 AF_PACKET 原始套接字代替。
# 用法: ip netns exec inet python3 sniff.py <网卡|any> [秒数=3] [最多行数=40]
import socket
import struct
import sys
import time

ifname = sys.argv[1]
secs = float(sys.argv[2]) if len(sys.argv) > 2 else 3
maxlines = int(sys.argv[3]) if len(sys.argv) > 3 else 40

s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.ntohs(0x0003))  # ETH_P_ALL；只订 ETH_P_IP 时网桥端口上收不到
if ifname != "any":  # any = 命名空间里所有网卡（网桥转发的包要在端口 vA/vB 上才看得到）
    s.bind((ifname, 0))
s.settimeout(0.2)
t0 = time.time()
n = 0
while time.time() - t0 < secs and n < maxlines:
    try:
        pkt, meta = s.recvfrom(2048)
    except socket.timeout:
        continue
    if meta[2] == socket.PACKET_OUTGOING or meta[0] == "br0":  # 同一个包会在多处出现，只留端口上进来的那次
        continue
    ip = pkt[14:]
    if pkt[12:14] != b"\x08\x00" or ip[9] != 17:  # 只看 IPv4 UDP
        continue
    ihl = (ip[0] & 0x0F) * 4
    sport, dport = struct.unpack("!HH", ip[ihl:ihl + 4])
    src, dst = socket.inet_ntoa(ip[12:16]), socket.inet_ntoa(ip[16:20])
    payload = ip[ihl + 8:ihl + 8 + 24].decode(errors="replace").split()[0:2]
    print(f"{time.time() - t0:6.3f} {meta[0]:>4}  {src}:{sport} → {dst}:{dport}  {' '.join(payload)}")
    n += 1
