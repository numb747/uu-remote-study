//go:build windows

package main

// Windows 的界面窗口（docs/arch/DESIGN.md 决策 43）：同一个网页，装进系统自带的 WebView2（Edge 内核）里，
// 而不是在浏览器里开一个标签页。窗口有自己的标题和图标，没有地址栏。
//
// 窗口是单独的进程（p2pcp window <地址>）：后台常驻的 app 不碰窗口消息循环，关窗口只是这个进程退出，
// 不影响收文件；WebView2 初始化失败时库会直接 os.Exit，放在单独的进程里也只会关掉窗口。
// 已经开着一个窗口时（按窗口类名找），再打开只把它提到前面。
// 没有 WebView2 运行时（极少数精简版系统）或初始化失败：退回浏览器打开。
//
// WebView2 的 Go 绑定用 github.com/wailsapp/go-webview2（2026-11-24 审过 v1.0.23：不联网、不写文件和注册表，
// 默认用纯 Go 重写的加载器，库里带的 WebView2Loader.dll 不编进来）。

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

const windowClass = "P2PDirectWindow"

var (
	user32                     = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procShowWindow             = user32.NewProc("ShowWindow")
	procUpdateWindow           = user32.NewProc("UpdateWindow")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procFindWindowW            = user32.NewProc("FindWindowW")
	procIsIconic               = user32.NewProc("IsIconic")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procLoadCursorW            = user32.NewProc("LoadCursorW")
	procGetDpiForSystem        = user32.NewProc("GetDpiForSystem")
	procSetProcessDpiAwareCtx  = user32.NewProc("SetProcessDpiAwarenessContext")
	procAllowSetForegroundWnd  = user32.NewProc("AllowSetForegroundWindow")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW           = user32.NewProc("SendMessageW")
)

const (
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmClose          = 0x0010
	wmSetIcon        = 0x0080
	wsOverlapped     = 0x00CF0000 // WS_OVERLAPPEDWINDOW
	cwUseDefault     = 0x80000000
	swShow           = 5
	swShowNormal     = 1
	swRestore        = 9
	idcArrow         = 32512
	colorWindow      = 5
	dpiPerMonitorV2  = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	asfwAny          = ^uintptr(0) // ASFW_ANY = (DWORD)-1
	lrDefaultColor   = 0
	iconBig, iconSml = 1, 0
)

type wndClassEx struct {
	size, style                        uint32
	wndProc                            uintptr
	clsExtra, wndExtra                 int32
	instance, icon, cursor, background windows.Handle
	menuName, className                *uint16
	iconSm                             windows.Handle
}

type msg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

func findWindow() uintptr {
	cls, _ := windows.UTF16PtrFromString(windowClass)
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	return h
}

func raise(h uintptr) {
	if iconic, _, _ := procIsIconic.Call(h); iconic != 0 {
		procShowWindow.Call(h, swRestore)
	} else if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
		procShowWindow.Call(h, swShow) // 被藏起来的（旧版的窗口进程就会这样）也要显示出来
	}
	procSetForegroundWindow.Call(h)
}

// openUI：打开界面。已经开着窗口就提到前面（右键「发送到」交来的文件，网页经 SSE 自己会刷出来）；
// 否则起一个窗口进程。用户点开始菜单 / 右键时是这个进程拿着「前台」权限，放行给窗口进程，它才能跳到最前面
func openUI(url string) {
	if h := findWindow(); h != 0 {
		raise(h)
		return
	}
	procAllowSetForegroundWnd.Call(asfwAny)
	if err := startDetached("window", url); err != nil {
		log.Printf("开不了窗口（%v），用浏览器打开", err)
		openBrowser(url)
	}
}

// iconFromPNG：Vista 起图标资源里可以直接放 PNG
func iconFromPNG(png []byte, size int) windows.Handle {
	h, _, _ := procCreateIconFromResource.Call(uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)), 1, 0x00030000,
		uintptr(size), uintptr(size), lrDefaultColor)
	return windows.Handle(h)
}

// cmdWindow：p2pcp window <地址>，开窗口、跑消息循环，关了就退出
func cmdWindow(url string) error {
	if h := findWindow(); h != 0 {
		raise(h)
		return nil
	}
	runtime.LockOSThread() // 窗口、WebView2 的回调都要在建窗口的这个线程上
	procSetProcessDpiAwareCtx.Call(dpiPerMonitorV2)

	var chromium *edge.Chromium
	var hwnd uintptr
	wndProc := syscall.NewCallback(func(h, m, wp, lp uintptr) uintptr {
		switch m {
		case wmSize:
			if chromium != nil {
				chromium.Resize()
			}
		case wmClose:
			if chromium != nil {
				chromium.ShuttingDown()
			}
		case wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(h, m, wp, lp)
		return r
	})

	var inst windows.Handle
	windows.GetModuleHandleEx(0, nil, &inst)
	cls, _ := windows.UTF16PtrFromString(windowClass)
	title, _ := windows.UTF16PtrFromString("P2P 直传")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	big, small := iconFromPNG(appIconPNG, 256), iconFromPNG(appIconPNG, 32)
	wc := wndClassEx{wndProc: wndProc, instance: inst, cursor: windows.Handle(cursor), icon: big, iconSm: small,
		background: colorWindow + 1, className: cls}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fallbackBrowser(url, err)
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	w, h := 880*dpi/96, 960*dpi/96
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsOverlapped,
		cwUseDefault, cwUseDefault, w, h, 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		return fallbackBrowser(url, err)
	}
	procSendMessageW.Call(hwnd, wmSetIcon, iconBig, uintptr(big))
	procSendMessageW.Call(hwnd, wmSetIcon, iconSml, uintptr(small))

	chromium = edge.NewChromium()
	if d, err := os.UserCacheDir(); err == nil {
		chromium.DataPath = filepath.Join(d, "p2pcp", "webview") // 浏览数据放缓存目录，别撒进 %AppData%
	}
	chromium.SetErrorCallback(func(err error) {
		// 库在这之后会 os.Exit(1)：先退回浏览器，界面别打不开
		fallbackBrowser(url, err)
	})
	chromium.MessageCallback = func(m string, _ *edge.ICoreWebView2, _ *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		if m == "close" { // 网页上点了「退出」：窗口跟着关
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
	}
	procShowWindow.Call(hwnd, swShowNormal)
	procUpdateWindow.Call(hwnd)
	log.Printf("开窗口 %s", strings.SplitN(url, "?", 2)[0])
	procSetForegroundWindow.Call(hwnd)
	chromium.SetBackgroundColour(0x1a, 0x1b, 0x26, 0xff) // 和网页底色一样，加载时不闪白
	if !chromium.Embed(hwnd) {
		return fallbackBrowser(url, nil)
	}
	if s, err := chromium.GetSettings(); err == nil {
		s.PutAreDevToolsEnabled(false)
		s.PutIsStatusBarEnabled(false)
		s.PutIsZoomControlEnabled(false)
	}
	chromium.Resize()
	chromium.Navigate(url)
	chromium.Focus()

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

func fallbackBrowser(url string, err error) error {
	log.Printf("WebView2 窗口开不了（%v），用浏览器打开", err)
	openBrowser(url)
	return nil
}
