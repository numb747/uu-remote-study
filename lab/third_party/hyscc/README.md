# hyscc：Hysteria 2 的拥塞控制

来自 [apernet/hysteria](https://github.com/apernet/hysteria) `core/internal/congestion/{bbr,brutal,common}`，
提交 ca8fbd874fd6ca82413948de6dc5924405e103e1（2026-10-04），MIT 许可证（见 LICENSE.md）。
只改了 import 路径、去掉了测试文件；bbr/windowed_filter.go 里的 golang.org/x/exp/constraints 换成了标准库 cmp 和等价的类型集合。

需要 Hysteria 的 quic-go 分支 `github.com/apernet/quic-go`（`Conn.SetCongestionControl`），
所以用到它的 p2pcp 引用的是这个分支，其余程序（xfer）仍用上游 quic-go。
