//go:build windows

package main

import (
	"encoding/xml"
	"log"
	"strings"
	"sync"
)

// osNotify（Windows）：经 PowerShell 弹 toast。每弹一次都要起一个 PowerShell（半秒多），所以进度（pct ≥ 0）不弹，
// 只弹完成、失败、取消、加上好友这些。点通知打开网页界面（toastLaunch，app 启动时填）：地址带令牌，会进浏览器历史，
// 但令牌每次启动都换、本机的程序本来就读得到 app.json，不带的话浏览器关过之后点通知会被 guard 拦下。
// 用安装时登记的 AppUserModelID（desktop_windows.go），通知上显示「P2P 直传」；没装过就借 PowerShell 的
func osNotify(title, body string, pct int, replace string) (string, bool) {
	if pct >= 0 {
		return replace, false
	}
	esc := func(s string) string {
		var b strings.Builder
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	launch := ""
	if toastLaunch != "" {
		launch = ` activationType="protocol" launch="` + esc(toastLaunch) + `"`
	}
	doc := `<toast` + launch + `><visual><binding template="ToastGeneric"><text>` + esc(title) + `</text><text>` + esc(body) + `</text></binding></visual></toast>`
	aumid := appUserModelID
	if !aumidRegistered() {
		aumid = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`
	}
	script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.UI.Notifications.ToastNotification, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime] | Out-Null
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml(` + psQuote(doc) + `)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier(` + psQuote(aumid) + `).Show([Windows.UI.Notifications.ToastNotification]::new($x))`
	go func() { // 别让传输等 PowerShell
		toastMu.Lock()
		defer toastMu.Unlock()
		if out, err := powershell(script); err != nil {
			log.Printf("弹通知失败：%v %s", err, out)
		}
	}()
	return "", true
}

var toastMu sync.Mutex // 一个一个弹，别同时起好几个 PowerShell
