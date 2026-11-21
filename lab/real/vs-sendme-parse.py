#!/usr/bin/env python3
"""汇总 vs-sendme.sh 的结果：python3 -I vs-sendme-parse.py /tmp/vs

每轮给出：总时长（后上线一端的命令从启动到退出）、建连（能开始传数据）、何时切到直连、最终路径、吞吐。
- p2pcp：建连 = 发送端日志第一行到「连上」，吞吐取「发送完成…平均」，打洞通了才算建连，所以「到直连」= 建连。
- sendme：取接收端日志（iroh 事件）：conn::connected 是 QUIC 连上（通常先经中继），
  path::selected network_path=Ip(...) 是切到直连；吞吐 = 文件大小 / (总时长 - 建连)。
"""
import os
import re
import statistics
import sys
from datetime import datetime

OUT = sys.argv[1] if len(sys.argv) > 1 else "/tmp/vs"
SIZE = os.path.getsize(os.path.join(OUT, "src", "vs.bin"))
ANSI = re.compile(r"\x1b\[[0-9;]*m")
TS = re.compile(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d+)Z")


def go_dur(s):
    m = re.fullmatch(r"([\d.]+)(ms|s|µs)", s)
    if not m:
        return None
    v = float(m.group(1))
    return {"ms": v, "s": v * 1000, "µs": v / 1000}[m.group(2)]


def ts(line):
    m = TS.match(line)
    if not m:
        return None
    return datetime.fromisoformat(m.group(1)[:26]).timestamp() * 1000


def read(p):
    try:
        with open(p, encoding="utf-8", errors="replace") as f:
            return [ANSI.sub("", l) for l in f]
    except FileNotFoundError:
        return []


def hms(line):
    m = re.match(r"(\d\d):(\d\d):(\d\d\.\d+)", line)
    return (int(m.group(1)) * 3600 + int(m.group(2)) * 60 + float(m.group(3))) * 1000 if m else None


def p2pcp_row(tag):
    # 建连 = 发送端第一行日志 → 「连上」。日志里的「用时」只算最后成功的那一轮，前面失败重试的轮次不算
    lines = read(f"{OUT}/{tag}.send.log")
    conn = path = mbps = None
    start = hms(lines[0]) if lines else None
    for l in lines:
        m = re.search(r"连上 (.+?)，用时 (\S+)", l)
        if m and conn is None:
            path = "中继" if m.group(1).startswith("中继") else "直连"
            conn = hms(l) - start if start is not None else go_dur(m.group(2))
        m = re.search(r"发送完成.*平均 ([\d.]+) Mbps", l)
        if m:
            mbps = float(m.group(1))
    relayed_note = any("改走中继" in l for l in lines)
    return conn, conn if path == "直连" else None, path + ("（打洞失败）" if relayed_note else "") if path else "失败", mbps


def sendme_row(tag, total):
    lines = read(f"{OUT}/{tag}.recv.log")
    start = conn = direct = None
    last = "无"
    for l in lines:
        t = ts(l)
        if t is None:
            continue
        if start is None:
            start = t
        if "_events::conn::connected" in l and conn is None:
            conn = t - start
        m = re.search(r"path::selected: .*?network_path=(Ip|Relay)\(([^,)]*)", l)
        if m:
            last = "直连" if m.group(1) == "Ip" else "中继"
            if m.group(1) == "Ip" and direct is None:
                direct = t - start
    mbps = None
    if conn is not None and total:
        mbps = SIZE * 8 / ((total - conn) / 1000) / 1e6
    return conn, direct, last, mbps


def fmt(v, unit=""):
    return "—" if v is None else f"{v:.0f}{unit}"


rows = []
with open(f"{OUT}/results.tsv") as f:
    for l in f:
        tool, d, r, total, ok = l.rstrip("\n").split("\t")
        total = float(total)
        tag = f"{tool}-{d}-{r}"
        conn, direct, path, mbps = p2pcp_row(tag) if tool.startswith("p2pcp") else sendme_row(tag, total)
        rows.append((tool, d, r, total, conn, direct, path, mbps, ok))

print(f"文件 {SIZE / 1048576:.0f}MB\n")
print("| 工具 | 方向 | 轮 | 总时长 | 建连 | 到直连 | 最终路径 | 吞吐 Mbps | sha256 |")
print("|---|---|---|---|---|---|---|---|---|")
for tool, d, r, total, conn, direct, path, mbps, ok in rows:
    print(f"| {tool} | {d} | {r} | {total / 1000:.1f}s | {fmt(conn, 'ms')} | {fmt(direct, 'ms')} | {path} | {fmt(mbps)} | {ok} |")

print("\n中位数：\n")
print("| 工具 | 方向 | 总时长 | 建连 | 到直连 | 吞吐 Mbps |")
print("|---|---|---|---|---|---|")
for d in ("ph2pc", "pc2ph"):
    for tool in dict.fromkeys(x[0] for x in rows):
        sel = [x for x in rows if x[0] == tool and x[1] == d]
        if not sel:
            continue
        med = lambda i: statistics.median([x[i] for x in sel if x[i] is not None]) if any(x[i] is not None for x in sel) else None
        print(f"| {tool} | {d} | {statistics.median(x[3] for x in sel) / 1000:.1f}s |{fmt(med(4), 'ms')} | {fmt(med(5), 'ms')} | {fmt(med(7))} |")
