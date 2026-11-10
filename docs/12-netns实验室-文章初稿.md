# 别背 NAT1~4 了：用 netns 免 root 搭一个 NAT 实验室，把 UDP 打洞亲手测一遍

> 本文所有矩阵、日志和抓包都是在自己机器上跑出来的（2026-11-10 重跑过一遍），实验室脚本不需要 root，读者可以原样复现。文中 `203.0.113.x`、`198.51.100.x`、`192.0.2.x` 是 RFC 5737 规定的文档地址，实验室里就用这三段。

## 0. 先看一个「不该失败」的失败

两台机器，都在端口限制锥（俗称 NAT3）后面。网上那张 NAT1~4 矩阵表里，NAT3 ↔ NAT3 是一个大大的 ✅。

我在自己搭的实验室里跑了 5 次，0/5。在两个 NAT 中间的路由器上抓包：

```
会合服务器看到的映射：
id=A pub=203.0.113.1:42288
id=B pub=198.51.100.2:41227

 0.794  vA  203.0.113.1:42288  → 198.51.100.2:41227  PUNCH A
 0.794  vB  ICMP 端口不可达 198.51.100.2 → 203.0.113.1   原包 203.0.113.1:42288 → 198.51.100.2:41227
 0.895  vB  198.51.100.2:42146 → 203.0.113.1:42288   PUNCH B     ← 41227 呢？
 0.895  vA  ICMP 端口不可达 203.0.113.1 → 198.51.100.2   原包 198.51.100.2:42146 → 203.0.113.1:42288
 ……（此后每 200ms 重复一遍，直到超时）
```

B 用的是同一个 socket：发给会合服务器的包从 41227 出去，发给 A 的打洞包却从 **42146** 出去。A 的 NAT 只认 41227，于是丢掉。

NAT 给 B 换了端口，起因是 **A 自己的第一个打洞包**。后面会把这个过程拆开讲。

这篇文章做三件事：

1. 给一个能**自己推出** NAT1~4 矩阵的模型，不用背表；
2. 用 Linux network namespace 搭一个免 root 的 NAT 实验室，四种 NAT 各用几行 nft 规则实现，矩阵亲手测一遍；
3. 讲矩阵表里没有的东西：开头这个坑（两种死法）、低 TTL 预热、对称 NAT 的生日攻击，每一条都配抓包或实测数字。

## 1. 模型：NAT 是一张表

内网的 `10.0.1.2:37471` 往外发一个 UDP 包，NAT 改写源地址，并在表里记一行：

```
内网 10.0.1.2:37471  ⇄  公网 203.0.113.1:37471    （为谁开的：……）
```

所谓 NAT 类型，就是这张表的两个旋钮（RFC 4787 的说法）：

- **映射**：分配公网端口的时候，看不看目的地址？
  - 不看（EIM，端点无关映射）：这个内网端口发往谁，出去都是同一个公网端口。
  - 看（EDM，端点相关映射）：每换一个目的地址就分一个新端口，这就是「对称型」。
- **过滤**：外面来的包要核对哪些字段才放进来？
  - 不核对（EIF）：谁都能进。
  - 核对源 IP（ADF）：这个端口发去过的 IP 才能进，端口无所谓。
  - 核对源 IP + 端口（APDF）：必须是发去过的那个 IP:端口。

把俗称对上去：

| 俗称 | 映射 | 过滤 | 实验室里的档位 |
|---|---|---|---|
| NAT1 全锥 | EIM | EIF | `full` |
| NAT2 受限锥 | EIM | ADF | `restricted` |
| NAT3 端口受限锥 | EIM | APDF | `port` |
| NAT4 对称 | EDM | APDF | `sym` |

打洞要解决的也是两件事，打不通的时候先问是哪一件出了问题：

- **寻址**：我往对方哪个 IP:端口 发？会合服务器（或 STUN）是一面镜子，告诉你它看到的源地址，再把这个地址转告对方。这个地址只在 **EIM** 时对别人也有效；对称 NAT 给镜子开的端口，给别人用就是错的。
- **放行**：我的包到达对方 NAT 时，表里有没有一行能匹配上它？没有就丢。表里的行只能由对方**往外发包**写进去。所以打洞两边都要发包：**自己发的包是给自己开门的，真正穿过去的是对方的包。**

记住这两件事就够了，后面的矩阵每一格都能用它们推出来。

## 2. 实验室：五个 namespace，一个脚本

```
peerA 10.0.1.2 ── natA [10.0.1.1 | 203.0.113.1] ──┐
                                                  ├── inet 路由器（192.0.2.1 跑会合服务器）
peerB 10.0.2.2 ── natB [10.0.2.1 | 198.51.100.2] ─┘
```

五台「机器」都是 network namespace，用 veth 连起来。整个实验跑在 `unshare -Urnm` 起的私有命名空间里：

- `-U` user namespace，把当前用户映射成命名空间里的「假 root」，有权限建网卡、写 nft 规则；
- `-n` 一个空的网络栈，碰不到宿主机的真实网络；
- `-m` mount namespace，用来在 `/run` 上挂 tmpfs（`ip netns add` 要写 `/run/netns`）。

脚本退出，进程没了，所有命名空间自动消失，不留垃圾。

**inet 必须是路由器，不能是网桥。** 两个 NAT 之间至少隔一跳，后面低 TTL 预热的包才有地方被丢掉。

### 2.1 四种 NAT，每种几行 nft

```bash
nat_rules() { # <ns> <内网主机 IP> <类型>
	local ns=$1 inner=$2 type=$3
	local nft="ip netns exec $ns nft"
	$nft add table ip nat
	$nft add chain ip nat post '{ type nat hook postrouting priority 100; }'
	$nft add chain ip nat pre '{ type nat hook prerouting priority -100; }'
	case $type in
	port) $nft add rule ip nat post oifname wan masquerade ;;
	sym) $nft add rule ip nat post oifname wan masquerade fully-random ;;
	full)
		$nft add rule ip nat post oifname wan masquerade
		$nft add rule ip nat pre iifname wan udp dport 1024-65535 dnat to "$inner"
		;;
	restricted)
		$nft add rule ip nat post oifname wan masquerade
		$nft add set ip nat contacted '{ type ipv4_addr . inet_service; flags dynamic,timeout; timeout 120s; }'
		$nft add chain ip nat outbound '{ type filter hook forward priority 0; }'
		$nft add rule ip nat outbound iifname lan oifname wan meta l4proto udp update @contacted '{ ip daddr . udp sport }'
		$nft add rule ip nat pre iifname wan ip saddr . udp dport @contacted dnat to "$inner"
		;;
	esac
	# 像真实路由器一样，在 INPUT 丢掉 WAN 上不请自来的包（--no-fw 时不加，第 4 节要用）
	if [[ $NO_FW != 1 ]]; then
		$nft add table ip filter
		$nft add chain ip filter input '{ type filter hook input priority 0; }'
		$nft add rule ip filter input iifname wan ct state new drop
	fi
}
```

逐个对照第 1 节的两个旋钮：

- **port**：Linux 的 MASQUERADE 默认尽量保留源端口，同一个内网端口发往不同目的地，出去的端口一样，这就是 EIM。回来的包要按 conntrack 的五元组匹配，五元组里有对方的 IP 和端口，这就是 APDF。所以**什么都不配的 Linux 路由器就是 NAT3**。
- **sym**：`fully-random` 让每条新连接（新的目的地址）随机分一个端口，就是 EDM。
- **full**：入方向的 UDP 全部 DNAT 给内网主机。严格说这比全锥还宽（更像 DMZ 主机），但对打洞来说效果一样。
- **restricted**：Linux 没有现成的 ADF，用一个动态集合实现。出包时在 forward 链记下「对方 IP . 本方端口」，入包的「源 IP . 目的端口」在集合里就放进来，不看源端口。forward 链上看到的是 SNAT 之前的内网端口，入包核对的是公网端口，两者能对上是因为 MASQUERADE 保留了端口。这是实验室的简化，够用。

### 2.2 打洞工具

`lab/punch` 是一个几百行的 Go 程序：一个会合服务器加两个对等端，协议是纯文本 UDP：

```
peer→server  REG <room> <id> <内网候选>
server→peer  PEER <对方 id> <对方公网映射> <对方内网候选>
peer↔peer    PUNCH / ACK / PING / PONG
```

服务器看到的 `REG` 源地址就是对方 NAT 分配的公网映射，所以它顺手做了 STUN 的事。两端拿到对方地址后每 200ms 往对方发一个 `PUNCH`，收到 `PUNCH` 回 `ACK`，谁先收到谁就算连上。**注册和打洞用同一个 socket**：换 socket 就换了本地端口，NAT 会开一行新映射，会合服务器告诉对方的地址就不再指向你了。

跑一次：

```bash
bash lab/netns/lab.sh run port port              # 一次，输出连上没有、走哪个地址
bash lab/netns/lab.sh run restricted sym -v      # 打印双方和服务器的完整日志
bash lab/netns/lab.sh matrix --repeat 5          # 4×4 全组合各跑 5 次
```

单次大约 3 秒，打不通的组合要等 6 秒超时。

### 2.3 先确认 NAT 真的是那一档

实验结论全靠 NAT 行为对，所以先验货。`filter-check.sh` 让 peerA 在 5000 端口收包，外面依次敲四次门：

```bash
bash lab/netns/lab.sh script restricted port filter-check.sh
```

| 敲门 | full | restricted | port |
|---|---|---|---|
| 阶段 1（A 只联系过 inet:9）：inet:7777，同 IP、换端口 | ✅ | ✅ | ❌ |
| 阶段 1：B:4000，陌生 IP | ✅ | ❌ | ❌ |
| 阶段 2（A 又往 B:4000 发了包）：B:4000，联系过的 IP + 端口 | ✅ | ✅ | ✅ |
| 阶段 2：B:4001，同 IP、换端口 | ✅ | ✅ | ❌ |

EIF / ADF / APDF 三种过滤和定义一致。

## 3. 基线矩阵：自己推，再测一遍

NAT 带防火墙（INPUT 丢弃 WAN 来的未请求包），不加任何技巧，每格 5 次：

| A \ B | full | restricted | port | sym |
|---|---|---|---|---|
| **full** | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 |
| **restricted** | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 |
| **port** | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 | ❌ 0/5 |
| **sym** | ✅ 5/5 | ✅ 5/5 | ❌ 0/5 | ❌ 0/5 |

和网上那张理论表一样。但是用寻址 + 放行，每一格都能自己推出来，不用背。最值得推的是这两格：**restricted ↔ sym 通，port ↔ sym 不通**。restricted 和 port 的映射一模一样，只差在过滤，结果却相反。

看 restricted ↔ sym 的日志（`lab.sh run restricted sym -v`）：

```
会合服务器:  id=B pub=198.51.100.2:20818                         ← B 问会合服务器时的映射
A:          peer B: pub=198.51.100.2:20818
A:          ✔ connected to B via 198.51.100.2:63933 [收到对方 PUNCH]  ← 真正连上的是 63933
```

- **寻址**：B 在对称 NAT 后面，20818 是 NAT 专门为会合服务器开的端口。B 往 A 发包时，NAT 换了个新端口 63933。A 拿到的 20818 是错的，往那里发的包到了 natB，表里没有匹配的行，丢弃。
- **放行**：B 从 63933 发来的包到达 natA。A 之前往 B 的 IP 发过包（虽然端口错了），ADF 只核对 IP，放行。A 从这个包的源地址学到了 B 的真实端口 63933（ICE 管这叫 peer-reflexive 候选），往 63933 回包。natB 的 63933 本来就是为 A 开的，回包放行，打通。

换成 port：B 从 63933 打来的包，APDF 要核对源端口，A 只往 20818 发过，63933 不在名单上，丢。A 永远学不到 63933。

一句话：**对称端的真实端口，只有它主动发来的包能告诉你。ADF 放这个包进来，APDF 只认你事先登记的那个错端口。**

其他格子：

- 有 full 的行和列全通：full 那端什么都放，另一端的包只要能送到，full 端就能学到真实地址并回包。
- 三种 EIM 互打全通：寻址没问题（镜子给的地址就是真实地址），两边同时发包，各自给自己开门。
- sym ↔ sym 不通：两边的地址都是错的，放行也就无从谈起。

到这里，网上的矩阵我们推了一遍，也测了一遍。下面是那张表里没有的东西。

## 4. 坑：对方路由器不设防，NAT3 ↔ NAT3 也会死

把 NAT 的 INPUT 丢弃规则去掉（`--no-fw`），模拟一台「什么防火墙都没配」的 Linux 路由器，再跑一遍矩阵：

| A \ B | full | restricted | port | sym |
|---|---|---|---|---|
| **full** | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 |
| **restricted** | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 | ✅ 5/5 |
| **port** | ✅ 5/5 | **❌ 0/5** | **❌ 0/5** | ❌ 0/5 |
| **sym** | ✅ 5/5 | **❌ 0/5** | ❌ 0/5 | ❌ 0/5 |

多死了三格，而且**矩阵不再对称**：port ↔ restricted 死了，restricted ↔ port 还活着。不对称的原因是先后顺序：实验室里 A 总比 B 早约 100ms 开始发 `PUNCH`，所以总是 A 的包先到对方 NAT。

`lab/netns/conflict-demo.sh` 把这个过程抓出来了（它让 B 晚 0.5 秒启动，保证 A 先到）。下面分两种死法讲。

### 4.1 死法一：换端口（port ↔ port）

就是开头那段抓包，逐步拆开：

1. **A 的第一个 PUNCH 到达 natB**：`203.0.113.1:42288 → 198.51.100.2:41227`。natB 的表里，41227 那行是 B 为会合服务器开的，五元组对不上，于是当成一条新连接。没有 DNAT 规则，目的地址又是 natB 自己，包被交给 natB 本机。本机没人监听这个端口，回了一个 ICMP 端口不可达（抓包里那行 `ICMP 端口不可达` 就是它）。

   关键在于：**这个包走完了 INPUT，conntrack 把它确认成了一条记录**：
   ```
   原方向  203.0.113.1:42288 → 198.51.100.2:41227
   回方向  198.51.100.2:41227 → 203.0.113.1:42288
   ```

2. **B 往 A 发 PUNCH**：`10.0.2.2:x → 203.0.113.1:42288`。MASQUERADE 想保留端口 41227，那么这条新连接的回方向就是 `203.0.113.1:42288 → 198.51.100.2:41227`，和第 1 步那条记录的原方向撞了。内核（`nf_nat_used_tuple`）判定这个五元组已被占用，只好换一个端口：42146。

3. **B 的包到达 natA**：natA 是端口受限锥，它为 B 开的那行是 41227，42146 不认识，丢弃（同样回 ICMP，抓包里第二种 ICMP 就是这个）。

4. **僵死**：A 每 200ms 发一个 PUNCH，每次都刷新第 1 步那条记录。UDP 未应答记录的默认超时是 30 秒，它永远等不到过期。B 永远出不了 41227，A 永远只认 41227。

### 4.2 死法二：吞包（sym ↔ restricted）

这一格没有换端口的问题。A 在对称 NAT 后面，打洞包从新端口 55961 出去，和会合服务器登记的 20624 不同，所以 B 往 A 发包时，五元组不会和那条记录撞上，B 的端口一直是 42871。但还是死了：

```
会合服务器:  id=A pub=203.0.113.1:20624     id=B pub=198.51.100.2:42871

 0.794  vA  203.0.113.1:55961 → 198.51.100.2:42871  PUNCH A
 0.794  vB  ICMP 端口不可达 198.51.100.2 → 203.0.113.1   原包 203.0.113.1:55961 → 198.51.100.2:42871
 0.896  vB  198.51.100.2:42871 → 203.0.113.1:20624  PUNCH B       ← B 已经往 A 的 IP 发过包了
 0.896  vA  ICMP 端口不可达 203.0.113.1 → 198.51.100.2   原包 198.51.100.2:42871 → 203.0.113.1:20624
 0.994  vA  203.0.113.1:55961 → 198.51.100.2:42871  PUNCH A
 0.994  vB  ICMP 端口不可达 198.51.100.2 → 203.0.113.1   原包 203.0.113.1:55961 → 198.51.100.2:42871   ← 还是被 natB 收下
 ……
```

按第 3 节的推理，0.896 之后 B 已经往 A 的 IP 发过包，restricted 的集合里有了 A 的 IP，A 从 55961 打来的包应该被 DNAT 给 B。可是抓包显示，natB 一直在回 ICMP 端口不可达：A 的包全被 natB 自己收下了。

原因是 **NAT 规则只对一条连接的第一个包求值**，后续的包直接按 conntrack 记录走。A 的第一个包已经被确认成一条「送往本机」的记录，之后 A 再发多少包，都顺着这条记录进 natB 本机，DNAT 规则根本没机会看到它们。

（B 发往 20624 的包被 natA 拒绝，那是正常的寻址错误：A 是对称 NAT，20624 只对会合服务器有效。）

### 4.3 为什么带防火墙就没事

conntrack 的记录在包刚进来时就建好了，但要等包**走完** netfilter 的钩子（INPUT 或 POSTROUTING 的最后）才「确认」，正式插进表里。防火墙在 INPUT 链的 priority 0 就把包丢了，这条未确认的记录随包一起释放，不留痕迹。

带防火墙时抓包里一个 ICMP 都没有。对照着看：

```bash
bash lab/netns/lab.sh script port port conflict-demo.sh --no-fw          # 僵死
bash lab/netns/lab.sh script port port conflict-demo.sh                  # 打通
bash lab/netns/lab.sh script sym restricted conflict-demo.sh --no-fw     # 吞包
```

再回头看不对称：A 是 full 或 restricted 时，B 被换了端口也无所谓，EIF/ADF 不看端口，B 的新端口照样进得去，A 学到它再回包。只有 A 是 port 或 sym 时（只认登记过的端口，或者只能靠自己的包打进对方），对方 NAT 上那条记录才致命。

我手上的两个真实 NAT（家用路由器、电信 5G 的 CGNAT）收到陌生包都是直接丢、不留状态，没有这个问题（第 7 节）。但打洞时你**没法知道对方是什么路由器**，所以不能指望对方帮忙。

> 题外话：那些 ICMP 端口不可达会回到发送方。Linux 上没有 connect 过的 UDP socket 会忽略它们；Windows 上则可能让下一次 `recvfrom` 返回 `WSAECONNRESET`（10054），需要用 `SIO_UDP_CONNRESET` 关掉。在 Windows 上写打洞程序的，收包循环里别一看到错误就退出。

## 5. 对策一：低 TTL 预热

既然问题出在「我的包先落到了对方 NAT 上」，那就让头几个包**到不了**对方 NAT：只要出得了自家 NAT，表里就记下了「允许对方回来」那一行，这是打洞包的全部工作。

`punch -ttl 2`：前 3 轮（600ms）的 PUNCH 用 TTL=2 发。从 peer 出发，过自家 NAT 减到 1，到 inet 时再减就是 0，inet 丢弃。之后恢复正常 TTL。

```bash
PUNCH_EXTRA="-ttl 2" bash lab/netns/lab.sh script port port conflict-demo.sh --no-fw
```

```
 0.793  vA  ttl=1   203.0.113.1:33983 → 198.51.100.2:53771  PUNCH A   ← 到 inet 时 TTL 只剩 1，不会再被转发
 0.894  vB  ttl=1   198.51.100.2:53771 → 203.0.113.1:33983  PUNCH B   ← B 的端口没被换，还是 53771
 0.993  vA  ttl=1   203.0.113.1:33983 → 198.51.100.2:53771  PUNCH A
 ……
 1.393  vA  ttl=63  203.0.113.1:33983 → 198.51.100.2:53771  PUNCH A   ← 预热结束，正常 TTL
 1.393  vB  ttl=63  198.51.100.2:53771 → 203.0.113.1:33983  ACK B     ← 同一毫秒打通
```

预热结束时，双方 NAT 上都已经有「自己往对方发过包」的那一行，对方正常 TTL 的包到达时匹配的是这一行，被当成回包转进内网，不会再在本机留下记录。B 晚了 0.5 秒启动，仍在 600ms 的预热窗口之内；如果两端开始的时刻差得比预热窗口还长，先到的正常 TTL 包照样会留下记录。

实现上有一个容易踩的坑：**TTL 是 socket 级选项**，不是每个包单独带的。接收循环收到对方的 PUNCH 要回 ACK，如果这个 ACK 恰好在预热窗口里发出，就会被带成低 TTL，到不了对方。所以「改 TTL → 发包」要放在一把锁里：

```go
func (s *sock) send(msg string, to *net.UDPAddr, ttl int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl == 0 {
		ttl = normalTTL
	}
	if ttl != s.ttl {
		setTTL(s.UDPConn, ttl) // setsockopt(IPPROTO_IP, IP_TTL)
		s.ttl = ttl
	}
	s.WriteToUDP([]byte(msg), to)
}
```

TTL 取多少：要能走出自家**所有**层 NAT，又到不了对方的 NAT。实验室里是 2。真实网络上我测过（第 7 节）：家宽 PC 要 2，5G 手机要 5（CGNAT 在第 4 跳），而到对方 NAT 要 11 跳以上，中间余量很大。所以不要写死成 2，先 traceroute 量出自家公网出口在第几跳再加 1，或者取一个偏保守的值，比如 6。

## 6. 对策二：生日攻击（port ↔ sym）

基线矩阵里 port ↔ sym 是死的：对称端的真实端口只有它自己的包能告诉你，而 APDF 不放这个包进来。

port 端改不了自家 NAT 的规则，唯一能做的是**往外发包写表**。「允许 B 的所有端口进来」等于「往 B 的每个端口都发一个包」，暴力扫六万多个端口太慢、太像扫描，还会撑爆 NAT 表。于是两边各出一份力：

- **对称端**（`-bday-sockets 256`）：额外开 256 个 socket，各自往 port 端的公网地址发包。对称 NAT 为它们开出 256 个随机端口的映射，每个都是专门为 port 端的 IP:端口 开的。
- **port 端**（`-bday-probes N`）：往对称端公网 IP 的 N 个随机端口发 PUNCH。port 端是 EIM，所有探测都从同一个公网端口出去，正好就是那 256 个映射等的地址。只要有一个探测打中了 256 个端口之一，两个方向同时放行：这个探测被对称端 NAT 当成回包放进去；同时 port 端的表里也有了「允许对方这个端口回来」的那一行。

两边各自随机撒一把，在中间相遇，所以叫生日攻击（思路来自 Tailscale 那篇《How NAT Traversal Works》）。单个探测命中的概率是 256/64512（对称端口落在 1024~65535），N 个探测至少中一个：`1 - (1 - 256/64512)^N`。

实测（每档 20 次，两次独立跑；今天那组是防火墙开着、不预热）：

| 探测预算 N | 今天 | 上个月 | 合计 | 理论 |
|---|---|---|---|---|
| 256 | 14/20 | 12/20 | 65% | 64% |
| 512 | 15/20 | 17/20 | 80% | 87% |
| 1024 | 20/20 | 20/20 | 100% | 98% |

工程上要注意三点：

- **N 是总预算，发完就停。** 重复探测同一批端口不会提高命中率，只会多发包。`punch` 限速每 200ms 发 128 个，1024 个约 1.6 秒发完。
- **永远是概率。** 所以一定要配中继兜底。
- **不预热的话，对方路由器不设防时生日攻击也会失效。** 对称端那 256 个包先到 port 端的 NAT，留下 256 条记录，port 端随后打向这些端口的探测全部撞上第 4.1 节的冲突、被换了端口。无防火墙 + 生日攻击、不预热，port ↔ sym 是 0/5。所以开着预热时，对称端那 256 个 socket **一直用低 TTL**：它们只负责在自家 NAT 上开映射，真正的连通靠对面的探测打进来。

## 7. 全家福，以及和真实网络对一下

无防火墙 + 低 TTL 预热 + 生日攻击，每格 10 次：

| A \ B | full | restricted | port | sym |
|---|---|---|---|---|
| **full** | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 |
| **restricted** | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 |
| **port** | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 | ✅ 10/10 |
| **sym** | ✅ 10/10 | ✅ 10/10 | ⚠ 9/10 | ❌ 0/10 |

在最不友好的条件下（对方路由器不设防），除了 sym ↔ sym，全部能通。sym ↔ port 那一格两次全量跑都是 9/10，比 1024 预算的理论失败率（约 1.7%）略高，样本太少，我没有深究。

**sym ↔ sym 为什么没救**：生日攻击成立的前提是有一端是固定靶子。EIM 端不管探测哪个端口，都从同一个公网端口出去，对称端 256 个 socket 瞄准它就行。两端都对称时，A 每换一个目的端口，自己的出口端口也跟着变，靶子在动；命中要求 A 从 a 发往 B:b 的同时，B 恰好有一个专为 A:a 开的映射 b，等于两边同时猜中对方为自己准备的那一个端口。实验室 0/10。现实里的出路是：端口可预测的对称 NAT（按顺序分配）可以猜，IPv6 没有 NAT，UPnP / PCP 让路由器主动开门，剩下的只能中继。

实验室终究是 Linux netfilter。我把同一套工具拿到真实网络上测了一对：家里 Wi-Fi 下的 PC，和走电信 5G 的手机（CGNAT 后面），这里只列和本文相关的结论，细节留给下一篇：

- **两端都是 EIM + APDF（NAT3）**，用同一个 socket 问 4 个不同 IP 的 STUN 服务器，映射都一样；过滤测试同 IP 换端口进不来。
- **30 多次同时开始，全部打通**。让一方先单独发 2 秒，再让另一方开始，双方看到的对端源地址都等于 STUN 映射，没有换端口：这两个 NAT 收到陌生包都是直接丢、不留状态，**第 4 节的坑在这一对网络上不存在**，但换个路由器还得测。
- **低 TTL 的取值**：PC 需要 TTL ≥ 2，手机需要 TTL ≥ 5；到服务器分别是 11、12 跳。
- **映射寿命**：静默后外面还能进来的时间，家用路由器 60~90 秒，CGNAT 2~3 分钟。家用路由器反而忘得更快，所以连接上**至少每 30 秒要有一个包**。
- **CGNAT 按端口块分端口**：连开 64 个 socket，映射全落在两段几百个端口的块里（类似 RFC 7422 的做法）。如果碰上对称型的 CGNAT，生日攻击要猜的只是块里的几百个端口，不是整个 64K。

## 8. 复现

需要：Linux，允许非特权 user namespace，`iproute2`、`nft`、Go、Python 3。不需要 root，也不需要 tcpdump：抓包用的是 `sniff.py`，三十来行 Python，用 `AF_PACKET` 原始套接字读包。

```bash
git clone 【TODO：仓库地址】 && cd 【TODO】/lab/netns
bash lab.sh run restricted sym -v                         # 第 3 节的日志
bash lab.sh matrix --repeat 5                             # 基线矩阵
bash lab.sh matrix --no-fw --repeat 5                     # 第 4 节
bash lab.sh script port port conflict-demo.sh --no-fw     # 第 4.1 节的抓包
bash lab.sh run port sym --bday-probes 512                # 生日攻击
bash lab.sh matrix --no-fw --bday --punch-args "-ttl 2" --repeat 10   # 全家福
```

Ubuntu 23.10 以后默认用 AppArmor 限制非特权 user namespace，`unshare -U` 会失败，需要 `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`（实验完记得改回来）。

局限：

- 四种 NAT 都是用 netfilter 模拟的，真实的 CGNAT 设备行为会更复杂，比如端口块、端口分配不保持；
- 还没有「对称、端口递增」这一档（netfilter 原生不支持），也没有双层 NAT；
- 实验室里两端的时差是固定的 100ms，真实网络里两端拿到对方地址的时间差可能更大，预热窗口要不要跟着调还没测。

## 9. 小结

- NAT 是一张表，类型就是两个旋钮：分配端口时看不看目的地（映射），回包核对哪些字段（过滤）。
- 打洞是两个问题：寻址（地址对不对）和放行（表里有没有对应的行）。矩阵里的每一格都能用这两点推出来。
- 打洞包的工作只是**给自己开门**。它要是落到了对方 NAT 上，在不设防的 Linux 路由器上会留下一条 conntrack 记录，造成换端口或吞包两种死法。低 TTL 预热让它只开门、不留脚印。
- 对称 NAT 的真实端口只有它自己的包能告诉你；对方是 APDF 时靠生日攻击，但它永远是概率，要有预算，也要有中继兜底。

【TODO：下一篇预告 / 结尾】
