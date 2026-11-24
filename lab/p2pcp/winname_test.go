package main

import "testing"

func TestWinSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"照片.jpg":         "照片.jpg",
		"a:b.txt":        "a：b.txt",
		`问?号*"<x>|.md`:   `问？号＊＂＜x＞｜.md`,
		"结尾点. .":         "结尾点",
		"...":            "_",
		"CON":            "_CON",
		"nul.txt":        "_nul.txt",
		"com1.tar.gz":    "_com1.tar.gz",
		"CONSOLE.txt":    "CONSOLE.txt",
		"lpt9 .log":      "_lpt9 .log",
		"tab\there":      "tab_here",
		"report (1).pdf": "report (1).pdf",
		"w:eird.bin":     "w：eird.bin",
		"../../x":        "..／..／x",
		`C:\Windows\a`:   `C：＼Windows＼a`,
	} {
		if got := winSafeName(in); got != want {
			t.Errorf("winSafeName(%q) = %q，要 %q", in, got, want)
		}
	}
}
