#!/usr/bin/env python3
# 生成文章用的打洞流程动图 punch.gif：注册 → 交换地址 → 两边打洞 → 直连。
# 包经过 NAT 时，下面那张「NAT 状态表」同步变化，对应正文的「映射 + 放行」两件事。
# 用法: python3 gen_punch_gif.py  （需要 Pillow 和更纱黑体）
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

W, H = 960, 540
FONT = "/usr/share/fonts/sarasa-gothic/Sarasa-Regular.ttc"
BOLD = "/usr/share/fonts/sarasa-gothic/Sarasa-Bold.ttc"
f_cap = ImageFont.truetype(BOLD, 22, index=1)
f_sub = ImageFont.truetype(FONT, 16, index=1)
f_box = ImageFont.truetype(BOLD, 20, index=1)
f_sm = ImageFont.truetype(FONT, 14, index=13)  # 等宽，地址对齐
f_pkt = ImageFont.truetype(BOLD, 14, index=13)

BG, INK, MUTED, LINE = "#f6f7f9", "#17212b", "#6b7785", "#c3ccd5"
SIG, PUNCH, DATA, OK, BAD, NEW = "#b06e00", "#2457c5", "#7a43b6", "#1f8048", "#c0392b", "#fff3c4"

# 拓扑坐标
A_BOX, NA_BOX, NB_BOX, B_BOX = (20, 300, 150, 370), (200, 290, 340, 380), (620, 290, 760, 380), (810, 300, 940, 370)
SRV_BOX = (390, 92, 570, 170)
A_OUT, NA_L, NA_R, MID, SRV = (150, 335), (200, 335), (340, 335), (480, 335), (480, 170)
NB_L, NB_R, B_IN = (620, 335), (760, 335), (810, 335)
P_A_SRV = [A_OUT, NA_L, NA_R, MID, SRV]
P_B_SRV = [B_IN, NB_R, NB_L, MID, SRV]
P_SRV_A = P_A_SRV[::-1]
P_SRV_B = P_B_SRV[::-1]
P_A_B = [A_OUT, NA_L, NA_R, MID, NB_L, NB_R, B_IN]
P_B_A = P_A_B[::-1]

A_PUB, B_PUB = "203.0.113.1:40000", "198.51.100.2:50000"


def state0():
    return {
        "cap": "", "sub": "",
        "na": [], "nb": [], "srv": [],  # 表里的行：(文字, 状态) 状态 = new | hit | ""
        "a_knows": "", "b_knows": "",
        "direct": False,
    }


def rbox(d, box, fill, outline, r=10, width=2):
    d.rounded_rectangle(box, r, fill=fill, outline=outline, width=width)


def ctext(d, box, text, font, fill):
    x0, y0, x1, y1 = box
    w = d.textlength(text, font=font)
    d.text(((x0 + x1 - w) / 2, y0), text, font=font, fill=fill)


def table(d, x, y, title, rows):
    d.text((x, y), title, font=f_sub, fill=MUTED)
    for i, (t, st) in enumerate(rows):
        yy = y + 24 + i * 24
        if st:
            color = {"new": NEW, "hit": "#d6f5df"}[st]
            d.rounded_rectangle((x - 4, yy - 2, x + d.textlength(t, font=f_sm) + 6, yy + 19), 4, fill=color)
        d.text((x, yy), t, font=f_sm, fill=INK)


def render(s, pkt=None, mark=None):
    im = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(im)
    # 说明
    d.text((24, 18), s["cap"], font=f_cap, fill=INK)
    d.text((24, 50), s["sub"], font=f_sub, fill=MUTED)
    # 连线
    srv_c = LINE if s["direct"] else MUTED
    d.line([A_OUT, NA_L], fill=MUTED, width=3)
    d.line([NA_R, NB_L], fill=MUTED, width=3)
    d.line([NB_R, B_IN], fill=MUTED, width=3)
    d.line([MID, SRV], fill=srv_c, width=3)
    d.text((MID[0] - 28, MID[1] + 8), "互联网", font=f_sub, fill=MUTED)
    if s["direct"]:
        d.line([(A_OUT[0], 322), (B_IN[0], 322)], fill=DATA, width=3)
    # 节点
    rbox(d, A_BOX, "white", INK)
    ctext(d, (A_BOX[0], 306, A_BOX[2], 0), "A", f_box, INK)
    ctext(d, (A_BOX[0], 336, A_BOX[2], 0), "10.0.1.2:5000", f_sm, MUTED)
    rbox(d, B_BOX, "white", INK)
    ctext(d, (B_BOX[0], 306, B_BOX[2], 0), "B", f_box, INK)
    ctext(d, (B_BOX[0], 336, B_BOX[2], 0), "10.0.2.2:6000", f_sm, MUTED)
    for box, name, pub in ((NA_BOX, "NAT A", "203.0.113.1"), (NB_BOX, "NAT B", "198.51.100.2")):
        rbox(d, box, "#e9eef3", INK)
        ctext(d, (box[0], box[1] + 18, box[2], 0), name, f_box, INK)
        ctext(d, (box[0], box[1] + 50, box[2], 0), pub, f_sm, MUTED)
    rbox(d, SRV_BOX, "white" if not s["direct"] else BG, srv_c)
    ctext(d, (SRV_BOX[0], 102, SRV_BOX[2], 0), "会合服务器", f_box, srv_c)
    ctext(d, (SRV_BOX[0], 134, SRV_BOX[2], 0), "STUN + 信令", f_sm, srv_c)
    # 状态
    table(d, 588, 96, "服务器看到的公网地址", s["srv"])
    table(d, 120, 396, "NAT A 的表", s["na"])
    table(d, 540, 396, "NAT B 的表", s["nb"])
    if s["a_knows"]:
        d.text((20, 268), s["a_knows"], font=f_sm, fill=SIG)
    if s["b_knows"]:
        w = d.textlength(s["b_knows"], font=f_sm)
        d.text((940 - w, 268), s["b_knows"], font=f_sm, fill=SIG)
    # 包
    for p in pkt or []:
        (x, y), label, color = p
        w = d.textlength(label, font=f_pkt)
        d.rounded_rectangle((x - w / 2 - 8, y - 13, x + w / 2 + 8, y + 13), 7, fill=color)
        d.text((x - w / 2, y - 9), label, font=f_pkt, fill="white")
    if mark:
        (x, y), ok, text = mark
        c = OK if ok else BAD
        d.ellipse((x - 16, y - 16, x + 16, y + 16), fill=c)
        d.text((x - 7, y - 11), "✓" if ok else "✗", font=f_box, fill="white")
        if text:  # 写在圆圈上方，别压住 NAT 方框
            w = d.textlength(text, font=f_sub)
            d.text((x - w / 2, y - 44), text, font=f_sub, fill=c)
    return im


frames, durations = [], []


def still(s, ms, **kw):
    frames.append(render(s, **kw))
    durations.append(ms)


def lerp(a, b, t):
    return (a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t)


def move(s, packets, steps=7, ms=45):
    """packets: [(路径, 标签, 颜色, {点序号: 回调(s)})]，同时出发，按段走，经过某个点时触发回调改状态。"""
    nseg = max(len(p[0]) - 1 for p in packets)
    for seg in range(nseg):
        for k in range(steps):
            t = k / steps
            pos = []
            for path, label, color, _ in packets:
                if seg < len(path) - 1:
                    pos.append((lerp(path[seg], path[seg + 1], t), label, color))
            still(s, ms, pkt=pos)
        for path, _, _, events in packets:
            if seg + 1 in events:
                events[seg + 1](s)


def clear_marks(s):
    for key in ("na", "nb", "srv"):
        s[key] = [(t, "") for t, _ in s[key]]


s = state0()
s["cap"] = "UDP 打洞：A、B 都在 NAT 后面，谁也连不进谁"
s["sub"] = "中间只有一台公网服务器负责牵线。注意：A、B 从头到尾只用一个 socket（5000 / 6000）"
still(s, 3000)

# ① A 注册
s["cap"], s["sub"] = "① A 向服务器注册", "包出 NAT A 时被改写源地址，NAT A 记下映射，并只放行服务器的回包"
move(s, [(P_A_SRV, "REG", SIG, {
    2: lambda s: s["na"].extend([("内网 :5000 ⇄ 公网 :40000", "new"), ("✓ 放行 服务器", "new")]),
    4: lambda s: s["srv"].append((f"A = {A_PUB}", "new")),
})])
still(s, 2200)
clear_marks(s)

# ② B 注册
s["cap"], s["sub"] = "② B 也注册", "服务器看到的是 NAT 改写后的地址——这就是 STUN 做的事：一面镜子"
move(s, [(P_B_SRV, "REG", SIG, {
    2: lambda s: s["nb"].extend([("内网 :6000 ⇄ 公网 :50000", "new"), ("✓ 放行 服务器", "new")]),
    4: lambda s: s["srv"].append((f"B = {B_PUB}", "new")),
})])
still(s, 2200)
clear_marks(s)

# ③ 交换地址
s["cap"], s["sub"] = "③ 服务器把地址互相转告（信令）", "服务器的回包能进来，因为两边 NAT 都有「放行 服务器」这一行"


def know_a(s):
    s["a_knows"] = f"对方 = {B_PUB}"


def know_b(s):
    s["b_knows"] = f"对方 = {A_PUB}"


move(s, [(P_SRV_A, "PEER", SIG, {4: know_a}), (P_SRV_B, "PEER", SIG, {4: know_b})])
still(s, 2200)

# ④ A 先打洞，被 NAT B 丢掉
s["cap"], s["sub"] = "④ A 往 B 的公网地址打洞", "包出 NAT A 时，NAT A 多了一行「放行 B」——自己发的包是给自己开门的"
move(s, [(P_A_B[:5], "PUNCH", PUNCH, {2: lambda s: s["na"].append((f"✓ 放行 {B_PUB}", "new"))})])
s["sub"] = "NAT B 的表里没有「放行 A」→ 丢弃。没关系，A 这边的门已经开了"
still(s, 3000, mark=((NB_L[0] - 34, NB_L[1]), False, "没有匹配的行"))
clear_marks(s)

# ⑤ B 打洞，进来了
s["cap"], s["sub"] = "⑤ B 也往 A 打洞", "出 NAT B 时多了一行「放行 A」；到 NAT A 时，正好匹配刚才开的门"


def hit_a(s):
    s["na"] = [(t, "hit" if "198.51" in t else "") for t, _ in s["na"]]


move(s, [(P_B_A, "PUNCH", PUNCH, {2: lambda s: s["nb"].append((f"✓ 放行 {A_PUB}", "new")), 5: hit_a})])
still(s, 2600, mark=(A_OUT, True, ""))
clear_marks(s)

# ⑥ A 回 ACK
s["cap"], s["sub"] = "⑥ A 回 ACK，也进了 NAT B —— 洞通了", "两边 NAT 都认为对方的包是「回包」"


def hit_b(s):
    s["nb"] = [(t, "hit" if "203.0" in t else "") for t, _ in s["nb"]]


move(s, [(P_A_B, "ACK", PUNCH, {4: hit_b})])
still(s, 2400, mark=(B_IN, True, ""))
clear_marks(s)

# ⑦ 直连
s["direct"] = True
s["cap"], s["sub"] = "之后数据直连，不再经过服务器", "p2pcp 在这同一个 socket 上跑 QUIC：加密、可靠、多路流"
for _ in range(2):
    move(s, [(P_A_B, "DATA", DATA, {}), (P_B_A, "DATA", DATA, {})], steps=5, ms=40)
still(s, 3000)

out = Path(__file__).with_name("punch.gif")
pal = [f.convert("P", palette=Image.ADAPTIVE, colors=64) for f in frames]
pal[0].save(out, save_all=True, append_images=pal[1:], duration=durations, loop=0, optimize=True)
print(out, len(frames), "帧", f"{sum(durations) / 1000:.1f} 秒", f"{out.stat().st_size / 1024:.0f} KB")
