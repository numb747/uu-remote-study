// 第 0 步：收包打印（你写的版本，对照同目录的 ref.go）
//
// 1. 照着原件写一遍，看懂一行写一行
// 2. 运行：cd lab && go run ./handpunch/step0
// 3. 另开终端：ncat -u 127.0.0.1 9000，随便打几行字
// 4. 改成镜子：每收到一个包，回一句「你是 <对方 IP:端口>」

package main

import (
	"fmt"
	"net"
)



func main() {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 9000})
	if err != nil {
		panic(err)
	}
	fmt.Println("在", conn.LocalAddr(), "等包")

	// 分配缓冲区
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			panic(err)
		}
		fmt.Printf("收到 %s 的: %s\n", from, buf[:n])
	}
}
