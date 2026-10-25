//go:build windows

package osutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleCP   = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procGetConsoleCP   = kernel32.NewProc("GetConsoleCP")
	shell32            = syscall.NewLazyDLL("shell32.dll")
	procIsUserAnAdmin  = shell32.NewProc("IsUserAnAdmin")
	user32             = syscall.NewLazyDLL("user32.dll")
	procShowWindow     = user32.NewProc("ShowWindow")
	procMessageBox     = user32.NewProc("MessageBoxW")
	procGetConsoleWnd  = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleList = kernel32.NewProc("GetConsoleProcessList")
	procShellExecuteW  = shell32.NewProc("ShellExecuteW")
)

// ConsoleUTF8 把控制台输入/输出代码页切成 UTF-8（65001），避免中文乱码。
func ConsoleUTF8() {
	_, _, _ = procSetConsoleCP.Call(65001)
	// 输入代码页也切一下，否则中文路径粘贴进来会变问号
	if k32 := kernel32.NewProc("SetConsoleCP"); k32.Find() == nil {
		_, _, _ = k32.Call(65001)
	}
}

// IsElevated 判断当前进程是否管理员。
func IsElevated() bool {
	r, _, _ := procIsUserAnAdmin.Call()
	return r != 0
}

// RelaunchElevated 以管理员身份重新启动自己（UAC 弹窗）。
func RelaunchElevated(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	params := make([]string, 0, len(args))
	for _, a := range args {
		params = append(params, quoteWin(a))
	}
	cwd, _ := os.Getwd()

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	argp, _ := syscall.UTF16PtrFromString(strings.Join(params, " "))
	dirp, _ := syscall.UTF16PtrFromString(cwd)

	const swShowNormal = 1
	r, _, e := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(argp)),
		uintptr(unsafe.Pointer(dirp)),
		swShowNormal)
	if r <= 32 {
		return fmt.Errorf("请求管理员权限失败（ShellExecute 返回 %d）: %v", r, e)
	}
	return nil
}

// quoteWin 给含空格的参数加双引号。
func quoteWin(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// GameRunning 判断游戏是否正在运行：Windows 下运行中的 exe 无法以写方式打开。
func GameRunning(installDir string) (bool, string) {
	for _, name := range []string{"love.exe", "KingdomRushDove.exe", "王国保卫战Dove版.exe"} {
		p := filepath.Join(installDir, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.ERROR_ACCESS_DENIED && !IsElevated() {
				continue // 大概率只是没权限，交给后续提权流程判断
			}
			return true, p
		}
		_ = f.Close()
	}
	return false, ""
}

// EnsureStdoutIsConsole 在 Windows 上把 stdout/stderr 绑到控制台（GUI 启动时也能看到输出）。
func EnsureStdoutIsConsole() {
	// 双击启动时既可能带控制台也可能没有；没有控制台时 attach 一次即可
	k32 := kernel32.NewProc("GetConsoleWindow")
	if hwnd, _, _ := k32.Call(); hwnd == 0 {
		if attach := kernel32.NewProc("AttachConsole"); attach.Find() == nil {
			const attachParentProcess = ^uintptr(0) // (DWORD)-1
			_, _, _ = attach.Call(attachParentProcess)
		}
	}
	ConsoleUTF8()
}

// OpenBrowser 用系统默认浏览器打开 url。
func OpenBrowser(url string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(url)
	r, _, e := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0, 0, 1)
	if r <= 32 {
		return fmt.Errorf("打开浏览器失败（ShellExecute 返回 %d）: %v", r, e)
	}
	return nil
}

// HideConsoleIfOwned 在进程独占控制台时把控制台窗口藏起来，双击启动就是这种情况。
// 从已有终端里启动时不藏，否则会把用户的终端一起藏掉。
func HideConsoleIfOwned() bool {
	var buf [8]uint32
	n, _, _ := procGetConsoleList.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n != 1 {
		return false
	}
	hwnd, _, _ := procGetConsoleWnd.Call()
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(hwnd, 0) // SW_HIDE
	return true
}

// MessageBox 弹一个系统对话框。控制台藏起来以后，出错信息只能这么给。
func MessageBox(title, text string) {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	b, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procMessageBox.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), 0x40) // MB_ICONINFORMATION
}
