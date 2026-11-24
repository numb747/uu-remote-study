//go:build windows

package main

// Windows 桌面集成（docs/arch/DESIGN.md 第 5 步，决策 37~40）。朋友拿到的只有一个 P2P直传.exe（-H windowsgui，没有黑框）：
//
//   - 双击（不带参数）= launch：不在安装目录里就先装（复制到 %LOCALAPPDATA%\Programs\P2P直传\，建开机自启、开始菜单、
//     「发送到」三个快捷方式，在「应用和功能」里登记卸载，登记通知用的 AppUserModelID），再从安装目录启动 app。
//     已经装过、版本一样就只补快捷方式；不一样先让正在跑的旧版退出（POST /api/quit）再覆盖。都不要管理员权限。
//   - P2P直传.exe uninstall：「应用和功能」里点卸载走这里。停掉 app、删快捷方式和登记、删安装目录；
//     设备密钥、好友（%AppData%\p2pcp）和收到的文件（下载\P2P直传）留着。
//
// 快捷方式、注册表都交给 PowerShell（WScript.Shell 建 .lnk 最省事），脚本用 -EncodedCommand 传，中文路径不怕代码页。

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed appicon.png
var appIconPNG []byte

const (
	appUserModelID = "P2PDirect.App"
	exeName        = "P2P直传.exe"
	uninstallKey   = `Software\Microsoft\Windows\CurrentVersion\Uninstall\P2PDirect`
	aumidKey       = `Software\Classes\AppUserModelId\` + appUserModelID
)

func installDir() string {
	d, err := os.UserCacheDir() // Windows 上是 %LOCALAPPDATA%
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "Programs", "P2P直传")
}

func defaultOut() string {
	if d, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0); err == nil {
		return filepath.Join(d, "P2P直传")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "P2P直传")
}

func aumidRegistered() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, aumidKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// powershell 跑一段脚本（不弹窗），回它的输出
func powershell(script string) (string, error) {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	hide(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

const createNoWindow = 0x08000000

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// startDetached 在后台另起一个自己：不带控制台、不跟着当前进程退出
func startDetached(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return startExe(exe, args...)
}

func startExe(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	// 不能设 HideWindow：子进程第一次 ShowWindow 会照父进程给的 SW_HIDE 来，窗口建好了却一直看不见
	// （2026-11-24 实机：双击后只有后台进程、没有窗口）。P2P直传.exe 是 GUI 程序，本来就没有黑框要藏
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// msgBox：没有控制台的 exe 出错时只能这样告诉用户
func msgBox(text string, isErr bool) {
	flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
	if isErr {
		flags = windows.MB_OK | windows.MB_ICONERROR
	}
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("P2P 直传")
	windows.MessageBox(0, t, c, flags)
}

// cmdLaunch：双击 exe。出错弹框（GUI 程序没有控制台）
func cmdLaunch(o opts) error {
	self, err := os.Executable()
	if err != nil {
		msgBox(err.Error(), true)
		return err
	}
	dst := filepath.Join(installDir(), exeName)
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(dst)) {
		if err := install(o, self, dst); err != nil {
			msgBox("安装失败："+err.Error(), true)
			return err
		}
		if err := startExe(dst, "app"); err != nil {
			msgBox("启动失败："+err.Error(), true)
			return err
		}
		return nil
	}
	return cmdApp(o, false)
}

func fileSum(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return h.Sum(nil)
}

// stopApp：有 app 在跑就让它退出，等它真退了（exe 解锁了才能覆盖、删除）
func stopApp(o opts) {
	a, ok := runningApp(configDir(o))
	if !ok {
		return
	}
	a.post("/api/quit", nil, nil)
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(200 * time.Millisecond) {
		if _, ok := runningApp(configDir(o)); !ok {
			break
		}
	}
	if p, err := os.FindProcess(a.PID); err == nil { // /ping 不回了，进程可能还在收尾；等它退出
		done := make(chan struct{})
		go func() { p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

// stopInstalled：停掉所有从安装目录运行的 P2P直传.exe（app 先好好退，窗口进程、卡住的旧进程直接结束），
// 不然 exe 被占着，覆盖、删除都不行
func stopInstalled(o opts, dst string) {
	stopApp(o)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snap)
	var hs []windows.Handle
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == uint32(os.Getpid()) || !strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), exeName) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, e.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil && strings.EqualFold(windows.UTF16ToString(buf[:n]), dst) {
			windows.TerminateProcess(h, 1)
			hs = append(hs, h)
			continue
		}
		windows.CloseHandle(h)
	}
	for _, h := range hs {
		windows.WaitForSingleObject(h, 3000)
		windows.CloseHandle(h)
	}
}

func install(o opts, self, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !bytes.Equal(fileSum(self), fileSum(dst)) {
		stopInstalled(o, dst) // 旧版在跑：exe 被占着，覆盖不了
		data, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		var werr error
		for range 20 { // 进程刚退出时文件可能还锁着一会儿
			tmp := dst + ".new"
			if werr = os.WriteFile(tmp, data, 0o755); werr == nil {
				if werr = os.Rename(tmp, dst); werr == nil {
					break
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		if werr != nil {
			return fmt.Errorf("覆盖 %s：%w（P2P 直传还开着？在任务管理器里结束它再试）", dst, werr)
		}
	}
	ico := filepath.Join(dir, "P2P直传.ico")
	if err := os.WriteFile(ico, pngToICO(appIconPNG), 0o644); err != nil {
		return err
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$exe = %s; $dir = %s; $ico = %s
$s = New-Object -ComObject WScript.Shell
function lnk($folder, $arg, $desc) {
  $l = $s.CreateShortcut((Join-Path ([Environment]::GetFolderPath($folder)) 'P2P 直传.lnk'))
  $l.TargetPath = $exe; $l.Arguments = $arg; $l.IconLocation = $ico; $l.Description = $desc; $l.WorkingDirectory = $dir
  $l.Save()
}
lnk 'Startup' 'app -no-browser' 'P2P 直传：开机后在后台接收好友发来的文件'
lnk 'Programs' 'app' 'P2P 直传：打洞直连传文件'
lnk 'SendTo' 'sendto' 'P2P 直传：发给好友'
$k = 'HKCU:\%s'
New-Item -Path $k -Force | Out-Null
Set-ItemProperty -Path $k -Name DisplayName -Value 'P2P 直传'
Set-ItemProperty -Path $k -Name DisplayVersion -Value %s
Set-ItemProperty -Path $k -Name Publisher -Value 'uu-remote-study'
Set-ItemProperty -Path $k -Name DisplayIcon -Value $ico
Set-ItemProperty -Path $k -Name InstallLocation -Value $dir
Set-ItemProperty -Path $k -Name UninstallString -Value ('"' + $exe + '" uninstall')
New-ItemProperty -Path $k -Name NoModify -Value 1 -PropertyType DWord -Force | Out-Null
New-ItemProperty -Path $k -Name NoRepair -Value 1 -PropertyType DWord -Force | Out-Null
$a = 'HKCU:\%s'
New-Item -Path $a -Force | Out-Null
Set-ItemProperty -Path $a -Name DisplayName -Value 'P2P 直传'
Set-ItemProperty -Path $a -Name IconUri -Value $ico
`, psQuote(dst), psQuote(dir), psQuote(ico), uninstallKey, psQuote(version), aumidKey)
	if out, err := powershell(script); err != nil {
		return fmt.Errorf("建快捷方式：%v %s", err, out)
	}
	return nil
}

// cmdUninstall：「应用和功能」里点卸载
func cmdUninstall(o opts) error {
	dir := installDir()
	stopInstalled(o, filepath.Join(dir, exeName))
	script := fmt.Sprintf(`foreach ($f in 'Startup', 'Programs', 'SendTo') {
  Remove-Item -LiteralPath (Join-Path ([Environment]::GetFolderPath($f)) 'P2P 直传.lnk') -ErrorAction SilentlyContinue
}
Remove-Item -Path 'HKCU:\%s' -Recurse -ErrorAction SilentlyContinue
Remove-Item -Path 'HKCU:\%s' -Recurse -ErrorAction SilentlyContinue
`, uninstallKey, aumidKey)
	if out, err := powershell(script); err != nil {
		msgBox(fmt.Sprintf("卸载时出错：%v %s", err, out), true)
		return err
	}
	msgBox("P2P 直传已卸载。\n\n设备密钥和好友（"+configDir(o)+"）、收到的文件（"+defaultOut()+"）都还在，不要了可以自己删。", false)
	// 自己正在运行，删不掉自己：交给一个隐藏的 cmd，等这边退出后再删安装目录
	if self, err := os.Executable(); err == nil && strings.EqualFold(filepath.Dir(filepath.Clean(self)), filepath.Clean(dir)) {
		// 命令行要原样交给 cmd（CmdLine）：Go 默认把参数里的引号转义成 \"，cmd 不认
		cmd := exec.Command("cmd.exe")
		cmd.Dir = os.TempDir() // 别让 cmd 的当前目录落在要删的目录里（从开始菜单进来时就是它）
		hide(cmd)
		cmd.SysProcAttr.CmdLine = `cmd.exe /c ping -n 3 127.0.0.1 >nul & rmdir /s /q "` + dir + `"`
		cmd.SysProcAttr.CreationFlags = windows.DETACHED_PROCESS
		cmd.Start()
	} else {
		os.RemoveAll(dir)
	}
	return nil
}

// pngToICO：Vista 起 .ico 里可以直接放 PNG，一张 256×256 的就够，系统自己缩
func pngToICO(png []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1}) // 保留、类型 1 = 图标、1 张
	b.Write([]byte{0, 0, 0, 0})                               // 宽高 0 = 256，调色板 0，保留
	binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})   // 颜色平面、每像素位数
	binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(png)), 22})
	b.Write(png)
	return b.Bytes()
}
