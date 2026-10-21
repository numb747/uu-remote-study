## 2026-10-21 · a85cb98 → a85cb98+dirty

- 新增端口转发 `p2pcp expose` / `forward`（tunnel.go）：打洞后在一条 QUIC 连接上，每个 TCP 连接对应一条流，用来让 rsync / ssh 不走 RustDesk 的中继。新图 32-p2p-tunnel 画两台机器、会合服务器和流怎么接到 sshd；模块图新增 p2p_tunnel；总览的 p2pcp 节点改名为「打洞直连核心」，加了一条用户经 forward 跑 rsync 的边。
- 打洞后让 QUIC 接管 socket 的步骤收成 `dialPeer` / `acceptPeer`（transfer.go），传文件和隧道共用。进程收到 SIGTERM / SIGINT 时，`exitGracefully` → `closeLive` 主动关闭所有连接。影响模块图 p2p_xfer、24 号图 quic 节点（锚点从 `tr.Dial` 那一行改成 `func dialPeer`）。
- daemon 和 expose 共用常驻循环 `keepServing`（main.go）：只在 32 号图 exp 节点里体现，24 / 31 号图没有改。
- 为腾名额：模块图里的安卓 App 和电脑端合成「P2P 直传软件 ×2」节点（锚点指电脑端 p2p-direct），细节在 31 号图，并加了 `@link` 过去。
- 疑点：`recv` 某条子连接失败后，不会取消其余还在会合服务器上等对方的子连接，要等它们 30 秒超时才报错（docs/09 第 9.4 节）。隧道和传文件一样不校验证书，房间名是唯一的门槛，只适合转发 ssh 这类自带认证的服务。

## 2026-10-17 · 6778bda → a85cb98

- 会合服务器一个进程同时听 7000 和 7001，房间共享，定期清理过期成员和空房间：总览、24 号图、31 号图的 rdv，模块图 punch_server。
- p2pcp 的 `-server` 改成一串地址，每轮换一个。等待中会合回执断了就换 socket（失联），失败时分层诊断。新增 `p2pcp check` 体检，收进 p2p_connect 节点，没有单独画。影响 24 号图的 you / retry / reg、模块图 p2p_connect、总览 p2pcp。
- 电脑端改用 `-bind auto`，换网络不用重启：31 号图 pc_core、模块图 app_pc。
- App 换网后重启接收：31 号图 app_ui、模块图 app_android、总览 apps。

## 2026-10-17 · c4179a4+dirty → 6778bda

- 新图 31-p2p-direct：已落地产品「P2P 直传」的部署结构。三台机器（手机 App、电脑、99-server）各自的组件，以及「① 会合只交换地址 → ② 打洞 → ③ QUIC 直连」三段分别走哪条路。总览的 apps 节点改为链到这张图。
- p2pcp 支持 IPv6：有全局 IPv6 时另开一个 IPv6 socket，`[v6]:port` 作为自报候选，两条路同时打洞，发送端用 /PN 提名（接收端有两个 socket 时等提名）；新增 global6、-4 / -6。影响 24 号图的 reg / punch / chosen 节点，新增 nom 节点；模块图 p2p_connect。
- 默认拥塞控制改为 bbr、连接数改为 2：24 号图的 rooms / cc，模块图 applyCC 的边。
- 安卓 App 的 -lan 也带上全局 IPv6（跳过 VPN 网卡）：模块图 app_android。
- 视图优化：
  - 总览分成「已落地：P2P 直传」和「研究实验室」两组，p2pcp 从打洞工具集里拆出来单独成节点，nftables 并进 netns 实验室节点（节点数仍是 10）。
  - 24 号图的两个入口合成一个节点（细节在 31 号图），给提名节点腾出名额。
- 上一段的两个疑点已解决：docs/09 第 7 节已写（7000 端口问题的排查过程）；99-server 已新增 `uu-rendezvous-7001.service`。这是服务器上的部署，仓库里只有文档记录。

## 2026-10-17 · 8221501 → c4179a4+dirty

- 会合服务器默认端口 7000 → 7001（p2pcp、安卓 App、p2p-direct 三处一起改；注释说某些电信 CGNAT 下和 7000 通过信的端口打不了洞）：总览、24 号图的 rdv 节点。
- p2pcp 打洞包从 `\x01PP` / `\x01PA` 改成 `/PP` / `/PA`，补零到 48 字节，仍按前 3 字节识别：24 号图的 punch / trig 节点。
- 新增 lab/real/cc-bench.sh（本机 recv + adb 手机 send，逐组比拥塞控制 × 连接数）：和 pcphone.sh 合成「编排 ×2」节点，总览和模块图各改一处，模块图多一条到 send / recv 的边。
- 疑点：代码注释引用的「docs/09 第 7 节」（7000 端口问题）在 docs/09 里还不存在；99-server 上会合服务是否已改听 7001，仓库里看不出来。

## 2026-10-16 · 6b8cbe2 → 8221501

- 新增 p2pcp（打洞传文件）：总览的工具集加上 p2pcp，模块图加 connect / send·recv 两个节点，新流程图 24-flow-p2pcp（会合 → 打洞 → QUIC 接管同一个 socket）。
- 新增 apps（安卓 App + 电脑端 p2p-direct）：总览、模块图各加入口，24 号图从它们开始。
- 新增 third_party/hyscc（Hysteria 的 BBR / Brutal），p2pcp 改用 apernet/quic-go，经 applyCC 选择拥塞控制：模块图、24 号图。
- punch 会合服务器加了 UNREG 和 YOU 回执：模块图的边、20 号图的 srv_see；总览新增常驻的会合服务器 8.140.18.211:7000。
- p2pcp 的打洞多了坏端口检测（低端口 / 无回执）和 4 秒超时换端口重试（最多 6 轮）：24 号图。
- natprobe peer 的 HIACK 带上回应方时间戳，算两端时钟偏差；pcphone.sh 新增 xfer 子命令复用打洞端口跑视频流：23 号图、模块图。
- 为腾名额合并节点：模块图里 punch 的 socket / 候选收集并入 punch_peer，natprobe 的服务端 / STUN 编解码并入 np_tests；总览去掉了手机节点（写进边标签）。21 号图改从 20 号图的 punch_all 进入。
- 疑点：REG 里的房间名就是共享密钥，明文发给会合服务器；QUIC 不校验证书（InsecureSkipVerify），会合服务器理论上能做中间人（24 号图边标签已注）。

## 2026-10-16 · 首次生成 · 6b8cbe2

- 以打洞为主线画地图：总览、模块图、4 条流程（punch 打洞、对称 NAT 手段、NAT 画像、手工打洞）、netns NAT 细节图。
- 传输层（transport / fakeenc / xfer）只在总览和模块图里收成一个节点，没有展开。
