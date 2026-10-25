package assetindex

import (
	"os"
	"path/filepath"
	"testing"
)

// 期望值由游戏内置更新器的 Lua 实现（LuaJIT + bit 库）算出，
// 用来锁死「LuaJIT 的 floor 取模 + double 乘法舍入」这套语义。
func TestFileHashMatchesLua(t *testing.T) {
	cases := []struct {
		size int
		want string
	}{
		{0, "048d78f8"},
		{1000, "a8a33404"},
		{9000, "b87f3a04"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		data := make([]byte, c.size)
		for i := range data {
			data[i] = byte((i*37 + 11) % 256)
		}
		p := filepath.Join(dir, "f.bin")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := FileHash(p)
		if err != nil {
			t.Fatalf("size=%d: %v", c.size, err)
		}
		if got != c.want {
			t.Errorf("size=%d: FileHash=%s, 期望 %s", c.size, got, c.want)
		}
	}
}

func TestParseIndex(t *testing.T) {
	src := []byte(`return {
	["a/b.png"]={3120,"3d759765"},
	["c/d.ogg"]={111791,},
	["e/f.lua"]={17},
}`)
	idx := Parse(src)
	if len(idx) != 3 {
		t.Fatalf("条目数 = %d, 期望 3", len(idx))
	}
	if e := idx["a/b.png"]; e.Size != 3120 || e.Hash != "3d759765" {
		t.Errorf("a/b.png 解析错误: %+v", e)
	}
}
