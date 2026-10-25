// Package osutil 屏蔽平台差异：控制台编码、管理员权限、安装目录探测、游戏是否在运行。
package osutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// IsWindows 是否运行在 Windows 上。
func IsWindows() bool { return runtime.GOOS == "windows" }

// InstallCandidates 返回自动探测安装目录的候选列表（不保证存在）。
// 顺序即优先级；Windows 下按盘符展开，Linux/WSL 下补充挂载点写法。
func InstallCandidates() []string {
	const folder = "王国保卫战Dove版"
	var out []string
	if IsWindows() {
		for _, drive := range []string{"D:", "E:", "C:", "F:"} {
			out = append(out,
				fmt.Sprintf(`%s\Program Files\%s\KingdomRushDove`, drive, folder),
				fmt.Sprintf(`%s\Program Files (x86)\%s\KingdomRushDove`, drive, folder),
				fmt.Sprintf(`%s\Games\%s\KingdomRushDove`, drive, folder),
				fmt.Sprintf(`%s\%s\KingdomRushDove`, drive, folder),
			)
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			out = append(out, filepath.Join(pf, folder, "KingdomRushDove"))
		}
	} else {
		for _, root := range []string{"/mnt/d", "/mnt/e", "/mnt/c", "/mnt/f"} {
			out = append(out,
				filepath.Join(root, "Program Files", folder, "KingdomRushDove"),
				filepath.Join(root, "Program Files (x86)", folder, "KingdomRushDove"),
				filepath.Join(root, "Games", folder, "KingdomRushDove"),
				filepath.Join(root, folder, "KingdomRushDove"),
			)
		}
	}
	// Steam / 自定义安装的补充猜测
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, folder, "KingdomRushDove"))
	}
	return out
}

// IsInstallDir 判断目录是否是游戏安装目录（有 main.lua 且目录名像本作或含 _assets）。
func IsInstallDir(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "main.lua")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "_assets")); err == nil {
		return true
	}
	return strings.Contains(strings.ToLower(dir), "kingdomrush")
}

// DetectInstallDir 自动探测安装目录，返回第一个命中的候选。
func DetectInstallDir() (string, error) {
	for _, c := range InstallCandidates() {
		if IsInstallDir(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("未能自动定位安装目录，请用 --dir 指定（形如 D:\\Program Files\\王国保卫战Dove版\\KingdomRushDove）")
}

// IsWritable 尝试在目录里创建并删除一个临时文件。
func IsWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".krd_write_test_*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// CanWriteTree 检查目录及其下已存在的文件能否写入（用于提前判断是否需要提权）。
func CanWriteTree(dir string) bool {
	if !IsWritable(dir) {
		return false
	}
	// 逐个探子目录会太慢，抽查几个关键路径
	for _, rel := range []string{"_assets", "_assets/kr1-desktop", "dove_modules", "kr1"} {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && st.IsDir() && !IsWritable(p) {
			return false
		}
	}
	return true
}
