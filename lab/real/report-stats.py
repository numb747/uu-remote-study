#!/usr/bin/env python3
"""汇总 p2pcp 的诊断上报：python3 -I report-stats.py reports.jsonl [更多文件…]

上报由会合服务器（punch server -reports）写成 jsonl，每行 {"rx": 收到时间, "from": 源地址, "r": 结果记录}，
结果记录就是 p2pcp 的 history.jsonl 那一行加上 dev（设备哈希），见 lab/p2pcp/upload.go。
原始记录里有朋友的公网 IP，放在不进 git 的 lab/real/reports/；这个脚本只输出汇总表（Markdown），可以进仓库和文章。

一次连接两端各报一条（conn 相同）：按连接统计时每个 conn 只取一条（优先发送端），没有 conn 的（还没见到对方就失败了）各算一条。

  - 总览：记录数、连接数、设备数、时间范围、版本和系统
  - NAT 组合：直连 / 中继 / 失败的比例，预测准确率（预测「能直连」「打不通」的才算）
  - 原因码分布（失败和连上后出错的）
  - 中继：占比、走中继时升级到直连的比例和用时
  - 建连耗时（从开始会合到 QUIC 认证完）：直连、中继分开的中位数和 90 分位
"""
import json
import statistics
import sys
from collections import Counter, defaultdict

REASONS = {
    "rdv_unreachable": "连不上会合服务器",
    "peer_offline": "对方没在线",
    "punch_blocked": "打不通",
    "relay_failed": "中继也不通",
    "conn_lost": "连上后断了",
    "auth_failed": "认证没过",
    "local_error": "本机读写出错",
    "unknown": "其他",
}


def load(paths):
    recs = []
    for p in paths:
        with open(p, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    w = json.loads(line)
                except json.JSONDecodeError:
                    continue
                r = w.get("r") if isinstance(w, dict) and "r" in w else w
                if isinstance(r, dict):
                    r["_rx"] = w.get("rx", r.get("at", ""))
                    recs.append(r)
    # 同一条可能被补发两次（补发时没收到回执）：按 设备 + 时间 + 角色 去重
    seen, out = set(), []
    for r in recs:
        k = (r.get("dev"), r.get("at"), r.get("role"))
        if k in seen:
            continue
        seen.add(k)
        out.append(r)
    return out


def per_conn(recs):
    """每个连接一条：同一个 conn 优先取发送端（它的 actual / took 是用户等的那段）"""
    by, loose = {}, []
    for r in recs:
        c = r.get("conn")
        if not c:
            loose.append(r)
            continue
        if c not in by or (r.get("role") == "send" and by[c].get("role") != "send"):
            by[c] = r
    return list(by.values()) + loose


def pct(a, b):
    return f"{100 * a / b:.0f}%" if b else "-"


def q(vals, p):
    if not vals:
        return "-"
    vals = sorted(vals)
    return f"{vals[min(len(vals) - 1, int(p * len(vals)))]}ms"


def table(head, rows):
    out = ["| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
    out += ["| " + " | ".join(str(x) for x in row) + " |" for row in rows]
    return "\n".join(out)


def main(paths):
    recs = load(paths)
    if not recs:
        print("没有记录")
        return
    conns = per_conn(recs)
    devs = {r.get("dev") for r in recs if r.get("dev")}
    times = sorted(r["_rx"] for r in recs if r.get("_rx"))
    print(f"# p2pcp 诊断上报汇总\n")
    print(f"记录 {len(recs)} 条，按连接去重后 {len(conns)} 次，设备 {len(devs)} 台，{times[0][:16]} ~ {times[-1][:16]}\n")
    print("版本：" + "，".join(f"{v} {n}" for v, n in Counter(r.get("ver", "?") for r in recs).most_common()))
    print("系统：" + "，".join(f"{v} {n}" for v, n in Counter(r.get("os", "?") for r in recs).most_common()) + "\n")

    # NAT 组合：两端的类别排个序，无向
    combos = defaultdict(list)
    for r in conns:
        a = (r.get("me") or {}).get("class") or "?"
        b = (r.get("peer") or {}).get("class") or "?"
        combos[" ↔ ".join(sorted([a, b]))].append(r)
    rows = []
    for k, rs in sorted(combos.items(), key=lambda kv: -len(kv[1])):
        n = len(rs)
        d = sum(r.get("actual") == "直连" for r in rs)
        rl = sum(r.get("actual") == "中继" for r in rs)
        fl = sum(r.get("actual") == "失败" for r in rs)
        judged = [r for r in rs if r.get("predict") in ("能直连", "打不通") and not r.get("forced")]
        right = sum(not r.get("wrong") for r in judged)
        rows.append([k, n, pct(d, n), pct(rl, n), pct(fl, n), f"{pct(right, len(judged))}（{len(judged)} 次）"])
    print("## NAT 组合\n")
    print(table(["组合", "连接", "直连", "中继", "失败", "预测准确"], rows) + "\n")

    bad = [r for r in conns if r.get("reason")]
    print("## 原因码\n")
    if bad:
        rows = [[c, REASONS.get(c, c), n, pct(n, len(conns))] for c, n in Counter(r["reason"] for r in bad).most_common()]
        print(table(["原因码", "说明", "次数", "占全部连接"], rows) + "\n")
    else:
        print("没有失败的连接\n")

    relayed = [r for r in conns if r.get("actual") == "中继"]
    up = [r for r in relayed if r.get("upgraded_via")]
    forced = sum(bool(r.get("forced")) for r in relayed)
    print("## 中继\n")
    print(f"走中继 {len(relayed)} 次（占 {pct(len(relayed), len(conns))}，其中强制 {forced} 次），"
          f"之后升级到直连 {len(up)} 次（{pct(len(up), len(relayed))}）"
          + (f"，走中继多久后升级：中位数 {statistics.median(r['upgraded_after_ms'] for r in up) / 1000:.1f}s" if up else "") + "\n")

    print("## 建连耗时（从开始会合到 QUIC 认证完）\n")
    rows = []
    for kind in ("直连", "中继"):
        ms = [r.get("took_ms", 0) + r.get("handshake_ms", 0) for r in conns if r.get("actual") == kind and r.get("role") == "send"]
        rows.append([kind, len(ms), q(ms, 0.5), q(ms, 0.9)])
    print(table(["路径", "次数（发送端）", "中位数", "90 分位"], rows))
    print("\n接收端常驻等待，它的「从开始会合」含等对方上线的时间，所以只算发送端。")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1:])
