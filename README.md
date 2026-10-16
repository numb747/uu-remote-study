# uu-remote-study —— 远程 P2P 打洞 / 低延迟传输研究

主线：**打洞原理 + 良好的网络传输**；中继只作兜底。从拆解 UU 远程出发，最终自研打洞引擎与传输协议。

- 进度、待决事项、服务器信息 → [PROGRESS.md](PROGRESS.md)
- 知识文档 → `docs/`
  - [01 UU 远程原理](docs/01-UU远程原理.md)
  - [02 NAT 穿透原理](docs/02-NAT穿透原理.md)
  - [03 实测验证方案](docs/03-实测验证方案.md)（抓包结果放 `captures/`）
  - [04 自研路线图](docs/04-自研路线图.md)
  - [05 打洞与传输实现方案](docs/05-打洞与传输实现方案.md)（**主线**，里程碑 M1~M6）
  - [参考资料](docs/参考资料.md)
  - [00 知识归档原件](docs/00-知识归档-原件.md)（2026-10-16 从旧机器迁来的单文件版，只读留档）
- 实验代码 → `lab/`（Go，只用标准库）

```bash
cd lab
go run ./stunprobe                                               # 测 NAT 映射类型（开着 TUN 时要让 STUN 直连）
go run ./punch server -listen :7000                              # 会合服务器
go run ./punch peer -server 8.140.18.211:7000 -room demo -id A   # 两端分别在不同网络里跑
```
