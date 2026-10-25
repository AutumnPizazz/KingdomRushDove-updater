package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Render 把配置写成带注释的 JSONC。`config init` 的模板和图形界面「保存配置」都走它，
// 注释只有这一份来源。
func Render(c Config) []byte {
	var b strings.Builder
	b.WriteString(`// 王国保卫战 Dove 版更新器配置（JSONC：可写注释，也能有多余的尾逗号）
// 优先级：命令行参数 > 环境变量 KRD_* > 本文件 > 内置默认值
// 相对路径都相对「本文件所在目录」解析，支持 ${ENV_VAR} 与 ~ 前缀。
{
  // 游戏安装目录，选到含 main.lua 与 _assets 的那一层。留空则自动探测常见位置。
  `)
	fmt.Fprintf(&b, "%s: %s,\n\n", quoteKey("install_dir"), quoteVal(c.InstallDir))

	b.WriteString("  // 更新源，按顺序探测，第一个可用的胜出（官方镜像，通常不用改）\n  \"sites\": [\n")
	for i, s := range c.Sites {
		comma := ","
		if i == len(c.Sites)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %s%s\n", quoteVal(s), comma)
	}
	b.WriteString("  ],\n\n")

	fmt.Fprintf(&b, "  \"timeout_seconds\": %d,        // 单次 HTTP 请求超时\n", c.TimeoutSeconds)
	fmt.Fprintf(&b, "  \"retries\": %d,                 // 网络失败重试次数（指数退避）\n", c.Retries)
	fmt.Fprintf(&b, "  \"retry_backoff_seconds\": %d,   // 退避基数：2s、4s、8s……\n", c.RetryBackoffSeconds)
	fmt.Fprintf(&b, "  \"parallel_downloads\": %d,      // 打包失效时逐文件下载的并发数\n\n", c.ParallelDownloads)

	b.WriteString("  // 服务端会把多个文件打成一个 zip 下发，文件多了自动分批\n")
	fmt.Fprintf(&b, "  \"max_bundle_bytes\": %d, // 每批的原始文件总大小上限（20MB）\n", c.MaxBundleBytes)
	fmt.Fprintf(&b, "  \"max_bundle_files\": %d,      // 每批文件个数上限\n\n", c.MaxBundleFiles)

	fmt.Fprintf(&b, "  \"skip_assets\": %t,         // true = 只更新代码和数据，跳过美术音频\n", c.SkipAssets)
	fmt.Fprintf(&b, "  \"delete_removed\": %t,       // 是否删除服务端标记为删除的文件\n\n", c.DeleteRemoved)

	b.WriteString("  \"backup\": {\n")
	fmt.Fprintf(&b, "    \"enabled\": %t,           // true = 覆盖前把旧文件备份一份（占空间，但能回滚）\n", c.Backup.Enabled)
	fmt.Fprintf(&b, "    \"dir\": %s                   // 空 = 安装目录旁的 KRD_backup_<时间>\n", quoteVal(c.Backup.Dir))
	b.WriteString("  },\n\n")

	fmt.Fprintf(&b, "  \"temp_dir\": %s,               // 下载与解压的临时目录，空 = 系统临时目录\n", quoteVal(c.TempDir))
	fmt.Fprintf(&b, "  \"keep_temp\": %t,           // 出问题时设为 true，保留临时文件便于排查\n", c.KeepTemp)
	fmt.Fprintf(&b, "  \"log_file\": %s,               // 空 = 只输出到终端\n", quoteVal(c.LogFile))
	fmt.Fprintf(&b, "  \"user_agent\": %s,\n", quoteVal(c.UserAgent))
	fmt.Fprintf(&b, "  \"proxy\": %s,                  // 例如 http://127.0.0.1:7890，空 = 依次看环境变量\n", quoteVal(c.Proxy))
	fmt.Fprintf(&b, "  \"insecure_tls\": %t,        // 只给用自签证书的私有镜像\n\n", c.InsecureTLS)

	b.WriteString("  // 以下两项一般不用动，除非服务端改了协议\n")
	fmt.Fprintf(&b, "  \"hash_file\": %s,\n", quoteVal(c.HashFile))
	fmt.Fprintf(&b, "  \"index_file\": %s,\n\n", quoteVal(c.IndexFile))

	fmt.Fprintf(&b, "  \"elevate\": %t,              // 目录不可写时自动请求管理员权限\n", c.Elevate)
	fmt.Fprintf(&b, "  \"progress\": %s,           // auto（终端里显示进度条）| plain | none\n", quoteVal(c.Progress))
	fmt.Fprintf(&b, "  \"json_output\": %t,         // true = 结果以 JSON 输出，便于脚本解析\n", c.JSONOutput)
	fmt.Fprintf(&b, "  \"assume_yes\": %t,          // true = 不再询问确认\n", c.AssumeYes)
	fmt.Fprintf(&b, "  \"verbose\": %t\n", c.Verbose)
	b.WriteString("}\n")
	return []byte(b.String())
}

func quoteKey(s string) string { return `"` + s + `"` }

func quoteVal(s string) string {
	if s == "" {
		return `""`
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// WriteExample 写一份默认配置。目标文件已存在时不覆盖。
func WriteExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("文件已存在，未覆盖: %s", path)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, Render(Default()), 0o644)
}

// Save 把配置写到 path（新建或覆盖）。
func Save(path string, c Config) error {
	if c.InstallDir != "" {
		// 写文件时保留原文，不做绝对化，方便换机器
		c.InstallDir = strings.TrimRight(c.InstallDir, `/\`)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, Render(c), 0o644)
}
