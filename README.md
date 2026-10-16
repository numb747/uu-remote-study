# uu-remote-study —— 远程 P2P 打洞 / 低延迟传输研究

主线：**打洞原理 + 良好的网络传输**；中继只作兜底。从拆解 UU 远程出发，最终自研打洞引擎与传输协议。

- 进度、待决事项、服务器信息 → [PROGRESS.md](PROGRESS.md)
- 知识文档 → `docs/`
  - [01 UU 远程原理](docs/01-UU远程原理.md)
  - [02 NAT 穿透原理](docs/02-NAT穿透原理.md)
  - [03 实测验证方案](docs/03-实测验证方案.md)（抓包结果放 `captures/`）
  - [04 自研路线图](docs/04-自研路线图.md)
  - [05 打洞与传输实现方案](docs/05-打洞与传输实现方案.md)（**主线**，里程碑 M1~M6）
  - [06 netns 实验记录](docs/06-netns实验记录.md)（M2：打洞成功矩阵、conntrack 冲突的坑、低 TTL 预热与生日攻击）
  - [07 传输层 v1 实验记录](docs/07-传输层v1实验记录.md)（M4：自研 vs QUIC vs TCP 在弱网下的对比）
  - [08 真实网络打洞实测](docs/08-真实网络打洞实测.md)（PC 家宽 ↔ 手机 5G：两端 NAT 行为、CGNAT 端口块、建连耗时与触发检查）
  - [09 打洞传文件](docs/09-打洞传文件.md)（p2pcp：手机 5G → PC，打洞后在同一 socket 上跑 QUIC；单连接 18Mbps、4 连接 45Mbps 跑满 5G 上行）
  - [参考资料](docs/参考资料.md)
  - [00 知识归档原件](docs/00-知识归档-原件.md)（2026-10-16 从旧机器迁来的单文件版，只读留档）
- 软件「P2P 直传」→ `apps/`（手机 App + 电脑端常驻服务，见 docs/09 第 5 节）
- 实验代码 → `lab/`（Go；除 xfer 的 QUIC 对照组用 quic-go 外只用标准库）

```bash
cd lab
go run ./stunprobe                                               # 测 NAT 映射类型（开着 TUN 时要让 STUN 直连）
go run ./punch server -listen :7000                              # 会合服务器
go run ./punch peer -server 8.140.18.211:7000 -room demo -id A   # 两端分别在不同网络里跑

# netns 实验室（不需要 root）
./netns/lab.sh matrix                                            # 所有 NAT 组合的打洞成功矩阵
./netns/lab.sh matrix --repeat 10 --bday --punch-args "-ttl 2"   # 加上低 TTL 预热 + 生日攻击，每格跑 10 次
./netns/lab.sh run port sym --netem-a "delay 20ms loss 2%" -v    # 单个组合 + 弱网
./netns/lab.sh shell port port                                   # 进实验宇宙手动折腾

# 传输层对比（M4）
./netns/lab.sh xfer own --netem-a "delay 20ms rate 50mbit loss 1%" --xfer-args "-dur 10s"
./netns/xfer-bench.sh 20s                                        # own/quic/tcp × 4 种弱网，输出表格

# 真实网络（PC + USB 调试的手机走移动网络）
go run ./natprobe stun -bind <wlan0 地址> -servers ip:3478,ip:3478  # 映射 + 端口分配
./real/pcphone.sh deploy                                         # 交叉编译并推到手机
./real/pcphone.sh setup "-trigger -interval 20ms" 10             # 不用会合服务器直接打洞，统计建连耗时
go run ./p2pcp recv -bind <wlan0 地址> -room demo -o ~/Downloads   # 打洞传文件：接收端
# 手机上：/data/local/tmp/p2pcp send -room demo -conns 4 文件         # 发送端（会合服务器默认 8.140.18.211:7000）
```
