package pipeline

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// extractZipTo 把 zip 解压到 destRoot，只落地 allow 允许的相对路径（allow 为 nil 表示全部）。
// 返回实际写入的相对路径与字节数；zip 里出现 .. 或绝对路径会被直接拒绝。
func extractZipTo(zipPath, destRoot string, allow map[string]bool) ([]string, int64, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, 0, fmt.Errorf("打开压缩包失败: %w", err)
	}
	defer zr.Close()

	var written []string
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, err := sanitizeEntry(f.Name)
		if err != nil {
			return written, total, err
		}
		if allow != nil && !allow[rel] {
			continue
		}
		dst := filepath.Join(destRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, total, err
		}
		n, err := extractOne(f, dst)
		if err != nil {
			return written, total, fmt.Errorf("%s: %w", rel, err)
		}
		total += n
		written = append(written, rel)
	}
	return written, total, nil
}

func sanitizeEntry(name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("压缩包里有空路径条目")
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("压缩包里有非法路径: %s", name)
	}
	if strings.Contains(clean, ":") && filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("压缩包里有绝对路径: %s", name)
	}
	return clean, nil
}

func extractOne(f *zip.File, dst string) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	mode := f.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, rc)
	cerr := out.Close()
	if err != nil {
		return n, err
	}
	return n, cerr
}

// copyFile 复制单个文件（保留权限位），自动建目录。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := st.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// backupFiles 把已存在的文件复制到 backupDir 下同样的相对路径，返回备份数量。
func backupFiles(root string, rels []string, backupDir string) (int, error) {
	n := 0
	for _, rel := range rels {
		src := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(src)
		if err != nil || st.IsDir() {
			continue
		}
		dst := filepath.Join(backupDir, filepath.FromSlash(rel))
		if err := copyFile(src, dst); err != nil {
			return n, fmt.Errorf("备份 %s 失败: %w", rel, err)
		}
		n++
	}
	return n, nil
}

// deleteFiles 删除相对路径上的文件，顺带清理变空的父目录（到 root 为止）。
func deleteFiles(root string, rels []string) (int, error) {
	n := 0
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Remove(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return n, fmt.Errorf("删除 %s 失败: %w", rel, err)
		}
		n++
		pruneEmptyDirs(filepath.Dir(p), root)
	}
	return n, nil
}

func pruneEmptyDirs(dir, stop string) {
	for {
		if dir == "" || dir == stop || !strings.HasPrefix(dir, stop) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// writeHashFile 写入新的版本标记（与服务端约定的文件名）。
func writeHashFile(path, hash string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hash+"\n"), 0o644)
}
