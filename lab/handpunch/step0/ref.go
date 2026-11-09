//go:build ignore

// 第 0 步参考原件：收包打印。只看不改，对照着在同目录的 main.go 里写。
// 第一行的 go:build ignore 让编译这个包时跳过本文件，所以它和 main.go 各有一个 main 也不冲突。
//
// 单独运行原件：cd lab && go run ./handpunch/step0/ref.go
// 另开终端：ncat -u 127.0.0.1 9000，随便打几行字

package main // 能运行的程序必须叫 main 包

import (
	"fmt"
	"net"
)

func main() {
	// := 表示声明并赋值，类型自动推出来。Go 的函数常常返回「结果, 错误」两个值
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 9000})
	if err != nil { // Go 没有异常，每一步都要手动检查错误
		panic(err)
	}
	fmt.Println("在", conn.LocalAddr(), "等包")

	buf := make([]byte, 1500) // 一块 1500 字节的缓冲区，接近一个包的最大尺寸
	for {                     // Go 只有 for，不加条件就是死循环
		n, from, err := conn.ReadFromUDP(buf) // n 是收到的字节数，from 是对方的 IP:端口
		if err != nil {
			panic(err)
		}
		fmt.Printf("收到 %s 的: %s\n", from, buf[:n]) // buf[:n] 是切片，只取前 n 个字节
	}
}
