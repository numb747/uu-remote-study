# 设计：p2pcp 先体检、再直传

图：[26-flow-preflight](26-flow-preflight.mmd)（主流程）、[33-nat-profile](33-nat-profile.mmd)（本机画像怎么测），总览里的 `preflight` 节点。

## 目标

给自己和熟人用的 p2pcp：每次传之前先弄清「这两台设备之间能不能点对点、不能的话卡在哪一端哪一层」，能通就直传，不能通就走中继并说清原因，网络变好了自动切回直连。
和 iroh / Tailscale netcheck 拉开差距的地方：**双端组合预测**、**过滤行为**、**国内网络的具体病因**（毒端口、CGNAT 低端口段）。它们只报单端的 EIM / EDM。

## 决策

1. **send / recv / daemon 自动体检，画像按网络缓存；另有 `check -room` 做完整双端体检。**（用户定，2026-11-21）画像和会合同时进行，不增加建连时间；`check -room` 不用缓存，打通后再测 RTT / 丢包和中继。
2. **预测打不通时：仍短打一轮（4 秒），通了就直传并记一笔「预测错了」；不通立刻走中继**，省掉现在「两轮不通才中继」的第二轮。（用户定）预测能通时，重试策略照旧。
3. **走中继时后台继续打洞，打通就切到直连。**（用户定）实现上不用 QUIC 连接迁移：打通后在直连上重开一个传输会话，靠现有的断点续传（have 只报缺的块）接着传，中继会话停掉。升级房间号由中继连接的 EKM 派生（同 tunnel.go 的迁移房间），只有两端知道。
4. **画像经 `/R2` 的标志字段交换**（`nat=类别.v6`，和 `relay` 逗号分隔），会合服务器的转发逻辑不用改。画像在线路上是明文：NAT 类型不算秘密。（默认）
5. **画像不阻塞注册**：第一次 `/R2` 可以不带 `nat=`，画像出来后的下一次注册（打洞期间每 500ms 一次）带上；服务器每收到注册就把双方最新信息再推一遍。预测只影响「第一轮失败之后怎么办」和报告，所以第一轮结束前（4 秒）拿到就够。（默认）
6. **映射行为用多台公开 STUN 测**：同一个 socket 依次问不同 IP 的服务器，比较映射。默认清单取 `lab/real/pcphone.sh` 的四台，`-stun` 可改。（默认）
7. **过滤行为只区分 ADF 和 APDF，用自己服务器的「同 IP、不同端口」测**：会合服务器新增 `/FT`，从收包端口和另一个端口各回一包。4×4 矩阵里 full 和 restricted 的结果相同（restricted ↔ sym 5/5，port ↔ sym 0/5，docs/06 第 1.1 节），所以不需要第二个 IP 去区分 EIF / ADF。（默认）
8. **分类与预测矩阵**：`open`（映射 IP = 本机 IP）/ `cone`（EIM + ADF 或 EIF）/ `port`（EIM + APDF）/ `sym`（EDM）。预测打不通的组合：port ↔ sym、sym ↔ sym，以及任一端 UDP 不通。两端都有全局 IPv6 时另报「IPv6 可能直连」（运营商的 IPv6 防火墙行为不确定，只作提示）。对端是旧版、没带 `nat=` 时按「未知」处理，走现有流程。（默认，矩阵来自 netns 实验）
9. **预测和实际结果都追加进 `~/.cache/p2pcp/history.jsonl`**：攒真实网络数据，用来修正矩阵，也是文章素材。（默认）
10. **手机 App 这次只接 `-progress` 的 `@CHECK` 行**，界面上怎么展示以后再做。（默认）
11. **STUN 至少两台回了才分类**：只回一台分不清 EIM / EDM，记为未知、写明原因；全都没回也是未知，不当成「UDP 不通」。会合能不能通由 connect 自己的分层诊断负责（第 1 步实现时定）。
12. **STUN 不傻等**：同时问所有服务器，已经有两台回了、又过了 600ms 就收工。公开 STUN 挂一台很常见，不能让每次体检多等 1.5 秒（第 1 步）。实测 PC 家宽 4/4 台都回，约 200ms。
13. **STUN 清单可用环境变量 `P2PCP_STUN` 改**（同 `P2PCP_SERVER`）。netns 实验室在 inet 上加 192.0.2.2 / .3 两个地址，各起一个绑定具体 IP 的小 STUN 应答端，所有用例都用它们（第 1 步）。
14. **地图的 `@commit` 暂不更新**（第 8 步已同步到 d578d66）：地图还停在 ac3a392，中间 6 个提交没同步；现在改成 HEAD 会让 arch-map 下次增量更新漏掉它们。第 8 步同步地图时一起更新。
15. **`/F2` 从另一个监听端口（7000）发**（原待定）：7000 在某台电信 CGNAT 下有毒，担心影响判断。实测手机 5G CGNAT 被判 APDF，和 docs/08 用「两个对端互测」（完全不经过 7000）得到的 EIM + APDF 一致，PC 家宽同样一致，不受影响（第 2 步）。
16. **服务器部署**：99-server `uu-rendezvous` 换成带 `/FT` 的版本，旧版备份在 `/usr/local/bin/uu-rendezvous.v2`；旧客户端照常注册（重启后电脑上旧版常驻服务 1 秒内重新注册上）。有过滤测试后画像约 600ms（APDF 要等满 500ms 确认 /F2 没来），第 3 步并行后不再计入建连时间。
17. **缓存（原待定）**：指纹 = 出口 IP + main 表默认路由的网卡和网关 + STUN / 会合清单，30 分钟过期，按指纹分条存（最多 16 条），`flock` 里重读合并再写（同机 daemon 和 send、netns 两端会同时写）。只存 IPv4 部分，IPv6 每次现查。拿不到缓存目录（adb shell 下 `HOME=/`）就不缓存（第 3 步）。
18. **打洞比体检快时怎么报**（第 3 步）：connect 一结束就不再注册，对方后测好的画像收不到。做法：第一次注册前最多等 50ms（命中缓存够用）；画像还没带上时等待循环 50ms 醒一次，测好马上补注册（daemon 平时 5 秒才注册一次）；打洞阶段 listen 也收服务器重推的 /P；传输会话结束时 `settle`（最多等 1.5 秒本机画像），对方的没到就报「没赶上」，不冒充旧版。实测：两端都命中缓存时两边都能预测；有一端第一次现测时，只有它自己能预测。
19. **顺手修的打洞竞态（第 3 步）**：cone ↔ sym 时，cone 端从入包学到对方的新端口后只补发一次 /PP，对方收到 /PA 就停止收包，这个 /PP 没人应，cone 端永远等不到 /PA（netns full ↔ sym 偶发，对方在旧路上等 15 秒 QUIC）。改成把入包源地址（peer-reflexive）加进打洞目标，每 20ms 都打（24 号图 punch 节点）。
20. **重试策略（第 4 步）**：预测打不通时，发送端一轮不通就请求中继（原来两轮）；「不确定」照「能通」处理。`-relay off` 时完全不变（netns sym ↔ sym 打满 6 轮报错，不走中继）。预测和实际对不上时打一行「预测错了」（`setActual` / `compare`），第 5 步写进预测记录。
21. **轮次同步（原待定「轮次错位」，第 4 步）**：双方打洞期间每 500ms 重新注册，服务器每次都把对方的最新注册推过来；listen 收到这个 /P 时，对方带了 `relay` 就中途停止打洞、跟进中继（先让 listen 都退出，免得和 joinRelay 抢包）；对方换了新 socket 就把新地址也加进打洞目标。netns `desync` 用例人为错开轮次（B 晚 1.5 秒拿到 /P，前 4.5 秒 A→B 全丢）：port ↔ port 4.5 秒直连、sym ↔ sym 4.0 秒进中继；去掉这两处同步，同样的用例分别是 14 秒（被迫走中继，就是真实网络那次 15 秒的症状）和 10 秒。
22. **结果行和预测记录（第 5 步）**：传输会话结束（或打洞彻底失败）时 `settle` 一次：结果行把「打洞（或进中继）」「QUIC 握手 + 认证」「从开始会合共计」分开（先启动的接收端「共计」里大半是等对方）；`history.jsonl` 每次一行（两端画像、预测、实际、是否预测错、各段毫秒、第几轮、对方地址）；`-progress` 时同样内容输出 `@CHECK <json>`。只有拿到过对方地址的失败才记（等不到对方不算一次连接）。
23. **进中继时继续注册（第 5 步抓到的原有竞态）**：先进新一轮的一方拿到对方上一轮的旧注册就去了中继、并立刻注销；对方新一轮注册时房间里找不到它、看不到 relay 标志，只能等它在中继上 6 秒超时再来一轮（port ↔ sym 偶发 10~14 秒）。改成凑齐之前继续每 500ms 注册、凑齐后再注销：netns port ↔ sym、sym ↔ sym 各 3 次，发送端都是 4.0 秒、接收端「先等的时间 + 4 秒」。
24. **check -room（第 6 步）**：两端跑同一条命令，各取随机角色名 `chkXXXXXX`（服务器只给角色不同的配对），配上后 id 大的一端发起（`isControlling`），中继请求两端都能发；房间号另由 `HMAC(密钥, "check")` 派生，不会配上同名房间里的 daemon。画像现测、测完刷新缓存，**第一次注册前等画像测完**（`preWait` 3 秒；诊断不在乎这一会儿，否则打洞比画像快、两端拿不到对方的）。打通后 QUIC + 认证，双向互 ping 3 秒（datagram，`/fin` 握手收尾，晚开始的一端不会被算丢包），再测中继，打完整报告。证书在会合之前生成（手机上生成要一两百毫秒，放在打通之后对方的握手包就干等着：真实网络握手 267~321ms → 74~191ms）。
25. **非发起方见到 /PN 就停止打洞（第 6 步查到的握手慢）**：原来只有「两个 socket 都在打洞」时才认 /PN，否则要等自己那份 /PA；这期间 listen 把紧跟着到的 QUIC Initial 读走扔掉，发起方等 PTO 重传约 200ms，连丢几次就是 docs/13 的「打洞 118ms、握手 4.4s」。改成非发起方见到 /PN（或者 QUIC Initial）就停，后到的包留在内核缓冲区给 QUIC：netns check -room full ↔ sym 握手 202ms → 1ms。
26. **后台升级的做法（第 7 步，原交接提示的「A / B 二选一」）**：照决策 3 选 A（关中继、直连上重开会话靠续传），但**接收端不挂在中继连接的 socket 上**，而是另开一个 socket 在升级房间里等（像 daemon 一样 5 秒注册一次，`connectOnce` 新增可取消的 `quit`），对方来了就正常打洞、在新洞上 Listen。原因：挂在中继 socket 上时新连接会落到原 Listener，被 accept 循环当成同一会话的额外数据连接，要另做区分；另开 socket 就是一次普通的重开会话，还能走 IPv6。`recvSession` 改成循环：中继会话断开后 3 秒内拿到升级链接就在它上面接着收（发送端是先接上直连、再关中继），一次性的 `recv` 不会因为中继断开就退出。间隔照待定的倾向：15 秒起、失败翻倍、最长 2 分钟（`upgrade.go`）。
27. **切换时机**：发送端在 `end` → `ok` 之间不切（这时切，接收端可能已经校验完、改名成正式文件，`ok` 却没送到，重发会多出一份 `a (1)`），最后一个文件发出 `end` 之后也不切。实现是 `upgrader` 的锁：`sendFile` 在 end → ok 之间拿着它，`switchTo` 在锁里检查并关掉中继。断在半路的块已经写下的部分照常记进续传状态。预测记录 `actual` 仍记「中继」，另补 `upgraded_after_ms` / `upgraded_via`，结果行加「走中继 X 后升级到直连 …」。
28. **`-relay always` 不算预测错（第 7 步顺手修）**：原来强制中继时预测「能直连」会被记成预测错，污染 history（第 7 步验证全靠 `-relay always`，正好撞上）。发送端 `-relay always` 时在注册标志里加 `forced`（会合服务器原样转发，旧版不认识、忽略），两端都据此不比对，记录里写 `forced: true`，结果行写「强制走中继，不比对」。

## 待定

- **EDM 再往下细分**（端口保持 / 递增 / 随机，能不能做端口预测）：倾向这次不做，统一归 `sym`；history 里先记下端口分配的观测值。

## 明确不做（这次）

熟人通讯录 / 设备密钥配对、一对多传输、生日攻击 / 端口预测、走 TCP 443 的中继、App 里的体检界面。

## 实现顺序

2026-11-21 定稿。待定里的四条按「倾向」做，实测推翻了再改。

- [x] **1. 曳光弹：画像 → 带 nat= 会合 → 预测 → 一行报告。**（2026-11-21 完成：单元测试 4 组；netns sym↔port / port↔port / sym↔sym / restricted↔sym 的 preflight 用例全过，port↔port 全部回归 18/18；真实网络 PC ↔ 手机两个方向都报出画像和预测，但手机当时连着家里 Wi-Fi，5G 下的画像还没测） 只测映射（公开 STUN）、IPv6、会合端口通不通，还不测过滤，所以分类先只有 `open / eim / sym / udp-blocked`，预测对 `eim ↔ sym` 报「不确定」。
  - 节点：33 的 `start`、`p_stun`、`classify`、`out_flag`；26 的 `profile`、`reg`、`predict`、`report`（只有启动时那一行）。
  - 文件：新建 `lab/p2pcp/natprofile.go`（`natProfile(o) profile`、`probeSTUN`、`classify`、`(profile) flag() string`、`parseNat(string) profile`），新建 `predict.go`（`predict(me, peer profile) verdict`，`verdict` 带一句原因）；`rdv.go` 的 `/R2` 标志加 `nat=`，`/P` 里取出对方的 `nat=` 放进 `session`；`send` / `recv` 启动时打印「体检」行。画像这一步先同步做（阻塞约 1 秒），第 3 步再改成并行。
  - 验证：`p2pcp_test.go` 加 `classify`、`predict`、`parseNat` 的表驱动测试（矩阵照 docs/06）；netns `lab.sh script sym port p2pcp-test.sh` 能看到两端互报画像、预测「不确定」；真实网络 PC ↔ 手机 send 一次，两端都打出体检行，传输照常成功。
- [x] **2. 过滤测试 `/FT`：分出 cone 和 port。**（2026-11-21 完成：单元测试 refine / probeFilter；netns full·restricted·port·sym × port·sym 8 组，分类 4/4、预测与实际 8/8 吻合；已部署 99-server；真实网络 PC 家宽和手机 5G 都报 port，与 docs/08 一致）
  - 节点：33 的 `p_ft`、`srv_ft`；`classify` 补全成 `open / cone / port / sym`。
  - 文件：`lab/punch/main.go` 的 `runServer` 加 `/FT <nonce>`：从收包端口和另一个监听端口各回 `/FT1` `/FT2`；`natprofile.go` 加 `probeFilter`。服务器兼容：旧客户端不发 `/FT`，不受影响。
  - 验证：netns 四种 NAT（full / restricted / port / sym）各跑一次画像，分类 4/4 对上（full 和 restricted 都报 `cone`）；部署到 99-server（`uu-rendezvous.service`，旧二进制留 `.v2` 备份），PC 和手机各测一次画像。
- [x] **3. 画像缓存 + 和会合并行。**（2026-11-21 完成：单元测试缓存命中 / 指纹变 / 过期 / 未知不存、preflight 时序，`-race` 通过；netns 8 组 preflight 全过，直连组合建连 现测 ≈ 缓存 ≈ 不体检（2ms 级，现测端第一次多 50ms）；port↔port 全部回归 21/21，sym↔sym 的 resume / reconn / tunnel 失败在改动前的 HEAD 上同样失败（它们按直连设计）；真实网络手机有缓存时两端都能预测，建连 61ms = 不体检）
  - 节点：26 / 33 的 `cache`；决策 5 的「不阻塞注册」。
  - 文件：`natprofile.go` 加 `loadCached` / `saveCache`（`~/.cache/p2pcp/profile.json`，指纹 = 绑定地址 + 默认网关 IP + 网卡名，30 分钟过期）；`connect` 里画像放 goroutine，`connectOnce` 每次注册时带上已经拿到的画像。
  - 验证：单元测试指纹变化会让缓存失效；连发两次，第二次报「缓存」；netns 里比较改动前后的建连时间（`p2pcp-test.sh` 的计时），没有变长。
- [x] **4. 按预测定重试策略。**（2026-11-21 完成，连同轮次同步：单元测试 predictedNo / setActual；netns 预测打不通的 4 组 4 秒进中继（原来 8 秒），`desync` 4 组全过且去掉同步后失败，`-relay off` 不变；port↔port 全部回归 23/23，sym↔sym 适用的 15 项全过；真实网络 PC ↔ 手机 5 次全部第一轮直连）
  - 节点：26 的 `policy`。
  - 文件：`rdv.go` 的 `connect`：预测「不通」时第一轮失败就带 `relay` 标志；预测「不通」却打通了，`session` 里记一个 `predictionWrong`。
  - 验证：netns `sym ↔ sym`：走中继的时间从约 8 秒降到约 4 秒；`port ↔ port`、`cone ↔ sym` 仍然直连；`-relay off` 时行为不变。
- [x] **5. 完整报告 + 预测记录 + `@CHECK`。**（2026-11-21：单元测试 wrongPrediction / settle 写记录和 @CHECK、只记一次、失败也记；netns preflight 检查结果行和 4 条记录全对，port↔port 全部回归 25/25，sym↔sym 17/17；真实网络手机 → PC 结果行、@CHECK、记录都对。电脑常驻服务和手机 App 都已更新；电脑 → App 发测试文件，电脑收到 App 报的画像 port + IPv6，预测与实际对上。App 不是 debuggable 构建，读不到它的环境变量和缓存目录，App 侧缓存是否生效没能直接确认） 安卓 App 启动 p2pcp 时加环境变量 `XDG_CACHE_HOME=getCacheDir()`（现在 `new ProcessBuilder(cmd)` 没设，手机上没有缓存，作发送端时对方总拿不到它的画像），重新打包安装。 打洞和 QUIC 握手分开计时（2026-11-21 实测有一轮打洞 118ms、握手 4.4s，docs/13 第 5 节，原来的「用时」看不出来）。
  - 节点：26 的 `report`、`history`；33 的 `out_report`。
  - 文件：新建 `checkreport.go`：启动时一行、结束时一行（实际路径、建连耗时、是否预测错），追加 `~/.cache/p2pcp/history.jsonl`；`-progress` 的 `@CHECK <json>` 加在现有 `report.go` 的 `reporter` 上（和 `@STATE` 等放在一起）。
  - 验证：单元测试 history 行的格式；安卓 `MainActivity` 是 `switch` 分发 `@` 行的，确认没有命中的分支时不会报错，`apps/pc/p2p-direct` 也看一遍；真实网络发一次，history 里多一行。
- [x] **6. `check -room`：完整双端体检。**（2026-11-21：单元测试 isControlling / pingStats / 回环 pingPong（晚开始的一端零丢包）；netns checkpair 5 种组合两端报告全对；port↔port 全部回归 27/27、sym↔sym 19/19、full↔sym 17/17；真实网络 PC ↔ 手机 5G 4 次全部直连，RTT 中位约 41ms、丢包 0、中继可用）
  - 节点：26 的 `entry_check`、`pairtest`。
  - 文件：`check.go`：带 `-room` 时不读缓存、现测画像，然后会合、预测、打洞，打通后在 QUIC 上发 3 秒 datagram 测 RTT 和丢包，再用现有的 `probeRelay` 测中继；两端都打印完整报告。不带 `-room` 时保持现在的单端体检。
  - 验证：netns 四种组合（cone ↔ sym、port ↔ sym、sym ↔ sym、port ↔ port）的报告和矩阵一致；真实网络 PC ↔ 手机跑一次，报告存进 `docs/09` 的新一节。
- [x] **7. 中继后台升级到直连。**（2026-11-21：单元测试切换时机（end → ok 之间等着、最后一个文件和会话结束后不切，去掉锁后用例失败）、结果行、强制中继不算错，`-race` 通过；netns 新增 `xferup`（中继限速 3Mbit，20MB 走中继 15 秒后升级，共 16 秒，全走中继约 55 秒；一次性 recv 退出码 0；直连上续传、开第 2 条连接）和 `xferblock`（前 20 秒封直连，第一次失败，49 秒升级，daemon 接着收）；port↔port、full↔sym 全部回归 40/40；port↔sym、sym↔sym 升级尝试失败后留在中继上传完，速度不受影响，只有 tunnel 用例失败，和改动前的 HEAD 一样（隧道迁移也要直连）。真实网络 `-relay always` 发 20MB 三次，都在走中继约 15.2 秒后升级、sha256 一致：手机连家里 Wi-Fi 时升级到局域网直连，剩下 13.6MB 用了 0.45 秒；手机 5G → 电脑、电脑 → 手机 5G 都升级到公网直连（打洞 30~48ms，握手 + 认证 91~154ms），中继 15 秒传 6.4~6.5MB（约 3.4Mbps），剩下 13.5~13.6MB 用了 2.2~2.8 秒（39~49Mbps），全程约 19 秒，全走中继要约 47 秒。手机作接收端那次，等升级的第一个 socket 被 CGNAT 分到低端口 `:33`，2 秒后换 socket 重等，没耽误升级）
  - 节点：26 的 `upgrade`。
  - 文件：`transfer.go`：发送端走中继时启动 `upgradeLoop`（15 秒起，失败翻倍，最长 2 分钟），在由 EKM 派生的升级房间里 `connectOnce`；打通后在直连上 `sendSession`，中继会话取消。接收端（recv / daemon）在中继连接所在的 socket 上挂在升级房间里（参照 tunnel.go 的 `helpMigrate`），接受新会话后，同一个文件的续传状态照常生效。
  - 验证：netns `-relay always` 发 40MB：约 15 秒后切到直连，sha256 一致，日志里有「升级到直连」；netns 先封掉直连 20 秒再放开，能在下一次尝试时升级；真实网络手机 `-relay always` 发一次。
  - 实施提示（2026-11-21 交接时读代码记下的，开工前先核对）：
    - 接收端照搬 `tunnel.go` 的 `helpMigrate`：在中继连接的 socket 上（QUIC 占着它，非 QUIC 报文经 `readNonQUIC` 进 `l.sig`）用 `l.tr.WriteTo` 在升级房间 `connSecret(l.conn, "upgrade")` 里以 `recv.` 身份每 5 秒注册；收到对方的 `/P` 就从这个 socket 往对方候选打 4 秒洞。这样新的直连路径终点还是接收端原来那个 `quic.Transport`。
    - 因此发送端打通后新开 socket 上 `newLink` + `dial()` 的连接，会落到接收端**原来的 Listener**，被 `recvSession` 里已有的 accept 循环（「拒绝一条额外连接」那段）当成同一会话的额外连接接住。两种做法二选一：A. 照决策 3，发送端取消中继会话、在直连上重开会话靠续传接着传（接收端要确认重开的会话能被接住、续传状态生效）；B. 不重开会话，把直连连接加进发送端的连接池、中继连接不再领新块（控制流留在中继上也够用）。B 不丢在途的块，但要看 `sendSession` 里「各连接抢着发」的调度怎么摘掉一条。开工时选定一种，记进决策。
    - 升级房间号由连接的 EKM 派生，daemon 同时服务多个发送端时互不干扰；发送端用 `send.` 身份、接收端 `recv.`，会合服务器照常配对。
    - 预测记录：`actual` 先记「中继」，升级成功后补字段（比如 `upgraded_after_ms`），别把「中继 → 直连」记成预测错。
    - netns 里中继不限速，看不出提速，验证以日志里的「升级到直连」和之后的连接地址为准；可参照 `p2pcp-test.sh` 的 `t_upgrade`（隧道版）写 `t_xferup`。
- [x] **8. 收尾。**（2026-11-21：电脑服务和手机 App 重装成最新版，电脑 → App `-relay always` 20MB 升级到直连、sha256 一致；地图同步到 d578d66，没有 planned 节点，check.py 通过；docs/09 加第 11 节（含真实网络 check -room 报告）；PROGRESS 加 0.5.12；`go test -race` 通过，netns 回归见第 7 步） 总览的 `preflight` 和所有 planned 节点转成正式节点并补上 `@src`；docs/09 加一节「先体检、再直传」；PROGRESS 加一条；顺便把地图同步到当前 HEAD（落后的 6 个提交）。
  - 验证：`check.py` 退出码 0，没有 planned 节点；`go test ./p2pcp/` 和 `p2pcp-test.sh` 全过。
