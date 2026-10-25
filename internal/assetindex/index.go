// Package assetindex 解析服务端/本地的 _assets/assets_index.lua，并做增量比对。
//
// assets_index.lua 的形态是 `return { ["相对路径"] = {字节数, "fnv1a-32-哈希"}, ... }`，
// 哈希算法与游戏内置更新器 (dove_modules/updater/update_manager.lua 的 file_hash) 完全一致：
// FNV-1a 32 位：先读前 4096 字节，文件大于 8192 字节时再读末 4096 字节，最后混入文件大小。
package assetindex

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
)

// Entry 是索引里的一条资源记录。
type Entry struct {
	Size int64
	Hash string // 小写 8 位十六进制；本地索引可能没有
}

// Index 是「相对 _assets 的路径 -> 记录」。
type Index map[string]Entry

// Need 表示一条需要下载的资源。
type Need struct {
	Path string
	Size int64
}

// entryRe 容忍 {大小} 与 {大小,"哈希"} 两种写法，以及多余空白。
var entryRe = regexp.MustCompile(`\["((?:[^"\\]|\\.)*)"\]\s*=\s*\{\s*(-?\d+)\s*(?:,\s*(?:"([0-9a-fA-F]*)"\s*)?)?\}`)

// Parse 解析索引正文。
func Parse(data []byte) Index {
	idx := make(Index, 1024)
	for _, m := range entryRe.FindAllSubmatch(data, -1) {
		key := unescapeLuaString(m[1])
		var e Entry
		fmt.Sscanf(string(m[2]), "%d", &e.Size)
		if len(m) > 3 && len(m[3]) > 0 {
			e.Hash = lowerASCII(m[3])
		}
		idx[key] = e
	}
	return idx
}

// ParseFile 读取并解析一个索引文件。
func ParseFile(path string) (Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	idx := Parse(data)
	if len(idx) == 0 {
		return nil, fmt.Errorf("%s 解析结果为空（可能不是 assets_index 文件）", path)
	}
	return idx, nil
}

func unescapeLuaString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	// 只处理索引里真实可能出现的两种转义
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) {
			switch b[i+1] {
			case '"', '\\':
				out = append(out, b[i+1])
				i++
				continue
			case 'n':
				out = append(out, '\n')
				i++
				continue
			}
		}
		out = append(out, b[i])
	}
	return string(out)
}

func lowerASCII(b []byte) string {
	out := make([]byte, len(b))
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

const (
	fnvOffset32 = 2166136261
	fnvPrime32  = 16777619
	// 注意：服务端（Lua 版更新器）用的是 % 0xFFFFFFFF 而不是 mod 2^32，
	// 且 bit.bxor 的结果有符号，所以这里必须按 Lua 的语义逐位复刻，
	// 不能用 uint32 自然溢出，否则算出来的哈希与索引里存的完全对不上。
	luaModulus  = 0xFFFFFFFF
	hashChunk   = 4096
	hashTailMin = 8192
)

// luaStep 复刻 Lua 的那一行：
//
//	hash = (bit.bxor(hash, b) * prime) % 0xFFFFFFFF
//
// 两点必须照抄：bit.bxor 是有符号 32 位；乘法是 double 乘法（乘积可达 2^56，
// 会被舍入到 53 位尾数），所以这里用 float64 而不是 int64，否则哈希对不上。
func luaStep(hash float64, b byte) float64 {
	x := float64(int32(uint32(hash)) ^ int32(uint32(b)))
	return luaMod(x * float64(fnvPrime32))
}

// luaMod 复刻 LuaJIT 的 `%`：a - floor(a/b)*b（不是 fmod）。
// 两者在 a 为负、量级超过 2^53 时结果会差 1，服务端的哈希正是这种写法算出来的。
func luaMod(v float64) float64 {
	return v - math.Floor(v/float64(luaModulus))*float64(luaModulus)
}

// FileHash 复刻服务端的 file_hash 算法，返回 8 位小写十六进制。
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()

	hash := float64(fnvOffset32)
	head := make([]byte, hashChunk)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	for _, b := range head[:n] {
		hash = luaStep(hash, b)
	}
	if size > hashTailMin {
		if _, err := f.Seek(size-hashChunk, io.SeekStart); err != nil {
			return "", err
		}
		tail := make([]byte, hashChunk)
		n, err := io.ReadFull(f, tail)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return "", err
		}
		for _, b := range tail[:n] {
			hash = luaStep(hash, b)
		}
	}
	x := float64(int32(uint32(hash)) ^ int32(uint32(size)))
	hash = luaMod(x * float64(fnvPrime32))
	return fmt.Sprintf("%08x", uint32(hash)), nil
}

// Diff 比对远端索引与本地索引，返回需要下载的资源（按路径排序）。
// assetsRoot 用于「本地索引缺哈希」时现场算文件哈希（与内置更新器行为一致）。
func Diff(installAssetsRoot string, local, remote Index) []Need {
	need := make([]Need, 0, 64)
	for path, want := range remote {
		have, ok := local[path]
		if !ok {
			need = append(need, Need{Path: path, Size: want.Size})
			continue
		}
		if have.Size != want.Size {
			need = append(need, Need{Path: path, Size: want.Size})
			continue
		}
		if have.Hash == "" {
			// 本地索引没记哈希：现场算（与内置更新器一致，避免误判为「已最新」）
			if want.Hash == "" {
				continue
			}
			localPath := joinPath(installAssetsRoot, path)
			got, err := FileHash(localPath)
			if err != nil || got != want.Hash {
				need = append(need, Need{Path: path, Size: want.Size})
			}
			continue
		}
		if want.Hash != "" && have.Hash != want.Hash {
			need = append(need, Need{Path: path, Size: want.Size})
		}
	}
	sort.Slice(need, func(i, j int) bool { return need[i].Path < need[j].Path })
	return need
}

// Verify 校验磁盘上资源的实际哈希，返回不一致/缺失的路径（用于 `verify` 子命令）。
func Verify(installAssetsRoot string, remote Index) (mismatched []string, checked int) {
	for path, want := range remote {
		p := joinPath(installAssetsRoot, path)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			mismatched = append(mismatched, path)
			continue
		}
		checked++
		if st.Size() != want.Size {
			mismatched = append(mismatched, path)
			continue
		}
		if want.Hash == "" {
			continue
		}
		got, err := FileHash(p)
		if err != nil || got != want.Hash {
			mismatched = append(mismatched, path)
		}
	}
	sort.Strings(mismatched)
	return mismatched, checked
}

func joinPath(root, rel string) string {
	if root == "" {
		return rel
	}
	return root + string(os.PathSeparator) + rel
}

// TotalSize 汇总一批资源的大小，用于展示。
func TotalSize(list []Need) int64 {
	var n int64
	for _, it := range list {
		n += it.Size
	}
	return n
}

// WriteFile 是给测试/工具用的便捷函数。
func WriteFile(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Flush()
}
