# 2026-11-21 p2pcp vs sendme 原始数据

分析见 [docs/13](../../../../docs/13-p2pcp与sendme对比实测.md)，脚本 `lab/real/vs-sendme.sh` / `vs-sendme-parse.py`。

| 目录 | 内容 |
|---|---|
| `run1-50MB-n0/` | 50MB，p2pcp vs sendme（n0 公共中继），各方向 3 轮；外加 `p2pcp-1c-bbr` / `p2pcp-1c-cubic`（单连接，PC→手机 2 轮） |
| `run2-20MB-own-relay/` | 20MB，p2pcp / sendme（n0）/ sendme-own（99-server 上自建 iroh-relay 1.5.0）交替，各方向 3 轮 |

每个目录：

- `results.tsv`：工具、方向、轮、总时长（毫秒，后上线一端从启动到退出）、sha256 是否一致
- `summary.md`：`vs-sendme-parse.py` 的输出（逐轮 + 中位数）
- `<工具>-<方向>-<轮>.{send,recv}.log`：两端原始日志。sendme 的是 iroh 事件日志（`RUST_LOG=iroh::_events=debug,iroh::net_report=debug`）
- `pc.sum` / `ph.sum`：两端随机测试文件的 sha256 前 16 位（文件本身没存）

方向：`ph2pc` = 手机发、PC 收；`pc2ph` = PC 发、手机收。

处理：去掉了 ANSI 颜色；自建中继的访问 token 一律替换成 `<TOKEN>`。

环境：PC 家宽 `120.1.4.60`（sing-box TUN 开着，两个工具都绑 wlan0 `192.168.1.95`），手机电信 5G CGNAT（vivo V2238，adb 跑静态二进制），会合 / 中继服务器 99-server `8.140.18.211`。
