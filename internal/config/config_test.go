package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolvePathKeepsForeignAbsolute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这个用例针对 Linux 上的 Windows 路径")
	}
	c := Config{ConfigDir: "/home/me/.config/krd-updater"}
	got := c.resolvePath(`D:\Program Files\王国保卫战Dove版\KingdomRushDove`)
	want := `D:\Program Files\王国保卫战Dove版\KingdomRushDove`
	if got != want {
		t.Errorf("resolvePath = %q, 期望 %q", got, want)
	}
}

func TestResolvePathJoinsRelative(t *testing.T) {
	c := Config{ConfigDir: "/home/me/cfg"}
	if got, want := c.resolvePath("game"), filepath.Join("/home/me/cfg", "game"); got != want {
		t.Errorf("resolvePath = %q, 期望 %q", got, want)
	}
}
