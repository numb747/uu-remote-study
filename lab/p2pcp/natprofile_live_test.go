package main

import (
	"os"
	"testing"
)

// 真实网络上测一次本机画像：P2PCP_LIVE=1 go test -run Live -v
func TestLiveProfile(t *testing.T) {
	if os.Getenv("P2PCP_LIVE") == "" {
		t.Skip("P2PCP_LIVE=1 才跑")
	}
	p := natProfile(opts{stun: defaultSTUN, bind: "auto"})
	t.Log(p)
}
