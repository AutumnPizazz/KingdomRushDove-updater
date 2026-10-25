//go:build !windows

package osutil

import (
	"fmt"
	"os/exec"
	"runtime"
)

// ConsoleUTF8 在类 Unix 系统上本来就是 UTF-8，无需处理。
func ConsoleUTF8() {}

// IsElevated 在类 Unix 系统上返回 true（权限问题用 chmod/sudo 解决）。
func IsElevated() bool { return true }

// RelaunchElevated 只在 Windows 上有意义。
func RelaunchElevated(args []string) error {
	return fmt.Errorf("自动提权仅在 Windows 上支持；请用 sudo 或修正目录权限后重试")
}

// GameRunning 在类 Unix 系统上不做检测（文件锁行为不同）。
func GameRunning(installDir string) (bool, string) { return false, "" }

// EnsureStdoutIsConsole 无需处理。
func EnsureStdoutIsConsole() { ConsoleUTF8() }

// OpenBrowser 用系统默认浏览器打开 url。
func OpenBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开浏览器失败: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// HideConsoleIfOwned 在非 Windows 平台上没有控制台窗口可藏。
func HideConsoleIfOwned() bool { return false }

// MessageBox 在非 Windows 平台上没有系统对话框，直接返回。
func MessageBox(title, text string) {}
