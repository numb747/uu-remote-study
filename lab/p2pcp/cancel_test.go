package main

import (
	"testing"
	"time"
)

func TestCanceledSIDs(t *testing.T) {
	if wasCanceled("") {
		t.Fatal("空发送号（旧版发送端）不能算取消过")
	}
	rememberCanceled("aaaa")
	if !wasCanceled("aaaa") || wasCanceled("bbbb") {
		t.Fatal("取消过的发送号没记住，或者认错了")
	}
	canceledMu.Lock()
	canceledSIDs["aaaa"] = time.Now().Add(-canceledTTL - time.Second)
	canceledMu.Unlock()
	if wasCanceled("aaaa") {
		t.Fatal("过期的发送号还算取消过：同一个文件过一阵重发会被拒")
	}
}

func TestCancelXfers(t *testing.T) {
	s := newXfer("send:1", "send", "小B")
	r1 := newXfer("recv:小B", "recv", "小B")
	r2 := newXfer("recv:小C", "recv", "小C")
	defer s.end()
	defer r1.end()
	defer r2.end()
	r1.setSID("s1")
	if n := cancelXfers(func(x *xfer) bool { return x.dir == "recv" && x.friend == "小B" }); n != 1 {
		t.Fatalf("取消了 %d 个，应为 1", n)
	}
	if !r1.canceled() || r2.canceled() || s.canceled() {
		t.Fatal("取消错了对象")
	}
	if !wasCanceled("s1") {
		t.Fatal("接收端取消时没记下发送号")
	}
	r1.cancel() // 重复取消不出事
	var nilX *xfer
	nilX.cancel()
	if nilX.canceled() {
		t.Fatal("nil 不该算取消")
	}
}
