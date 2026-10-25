// Package config 负责「默认值 <- 配置文件 <- 环境变量 <- 命令行参数」四层配置合并。
//
// 配置文件是 JSONC（JSON 加注释和尾逗号），先用 hujson 标准化再解析，
// 写注释、多一个尾逗号都不会导致解析失败。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/tailscale/hujson"
)

// DefaultSites 是官方更新镜像，按顺序探测，第一个可用的胜出。
var DefaultSites = []string{
	"https://krdovedownload4.crazyspotteddove.top/",
	"https://krdovedownload6.crazyspotteddove.top:52000/",
}

// Backup 控制覆盖前的备份行为。
type Backup struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"` // 空 = <安装目录的父目录>/KRD_backup_<时间>
}

// Config 是合并后的最终配置。
type Config struct {
	InstallDir string   `json:"install_dir"`
	Sites      []string `json:"sites"`

	TimeoutSeconds      int   `json:"timeout_seconds"`
	Retries             int   `json:"retries"`
	RetryBackoffSeconds int   `json:"retry_backoff_seconds"`
	ParallelDownloads   int   `json:"parallel_downloads"`
	MaxBundleBytes      int64 `json:"max_bundle_bytes"`
	MaxBundleFiles      int   `json:"max_bundle_files"`

	SkipAssets    bool   `json:"skip_assets"`
	DeleteRemoved bool   `json:"delete_removed"`
	Backup        Backup `json:"backup"`

	TempDir     string `json:"temp_dir"`
	KeepTemp    bool   `json:"keep_temp"`
	LogFile     string `json:"log_file"`
	UserAgent   string `json:"user_agent"`
	Proxy       string `json:"proxy"`
	InsecureTLS bool   `json:"insecure_tls"`

	HashFile  string `json:"hash_file"`
	IndexFile string `json:"index_file"`

	Elevate    bool   `json:"elevate"`
	Progress   string `json:"progress"` // auto | plain | none
	JSONOutput bool   `json:"json_output"`
	AssumeYes  bool   `json:"assume_yes"`
	Verbose    bool   `json:"verbose"`

	// 运行期信息，不参与配置文件
	ConfigPath string `json:"-"`
	ConfigDir  string `json:"-"`
}

// Default 返回内置默认值，保证「什么都不配」也能跑。
func Default() Config {
	return Config{
		InstallDir:          "",
		Sites:               append([]string(nil), DefaultSites...),
		TimeoutSeconds:      60,
		Retries:             4,
		RetryBackoffSeconds: 2,
		ParallelDownloads:   4,
		MaxBundleBytes:      20 << 20,
		MaxBundleFiles:      200,
		SkipAssets:          false,
		DeleteRemoved:       true,
		Backup:              Backup{Enabled: false},
		Progress:            "auto",
		Elevate:             true,
		HashFile:            "current_version_commit_hash.txt",
		IndexFile:           "_assets/assets_index.lua",
		UserAgent:           "KRD-Dove-Updater-Go/1.0",
	}
}

// fileConfig 用指针区分「没写这一项」和「写成了零值」。
type fileConfig struct {
	InstallDir *string   `json:"install_dir"`
	Sites      *[]string `json:"sites"`

	TimeoutSeconds      *int   `json:"timeout_seconds"`
	Retries             *int   `json:"retries"`
	RetryBackoffSeconds *int   `json:"retry_backoff_seconds"`
	ParallelDownloads   *int   `json:"parallel_downloads"`
	MaxBundleBytes      *int64 `json:"max_bundle_bytes"`
	MaxBundleFiles      *int   `json:"max_bundle_files"`

	SkipAssets    *bool         `json:"skip_assets"`
	DeleteRemoved *bool         `json:"delete_removed"`
	Backup        *backupConfig `json:"backup"`

	TempDir     *string `json:"temp_dir"`
	KeepTemp    *bool   `json:"keep_temp"`
	LogFile     *string `json:"log_file"`
	UserAgent   *string `json:"user_agent"`
	Proxy       *string `json:"proxy"`
	InsecureTLS *bool   `json:"insecure_tls"`

	HashFile  *string `json:"hash_file"`
	IndexFile *string `json:"index_file"`

	Elevate    *bool   `json:"elevate"`
	Progress   *string `json:"progress"`
	JSONOutput *bool   `json:"json_output"`
	AssumeYes  *bool   `json:"assume_yes"`
	Verbose    *bool   `json:"verbose"`
}

type backupConfig struct {
	Enabled *bool   `json:"enabled"`
	Dir     *string `json:"dir"`
}

// CandidatePaths 返回配置文件搜索路径（按优先级）。
func CandidatePaths(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	var out []string
	if v := os.Getenv("KRD_CONFIG"); v != "" {
		out = append(out, v)
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "krd-updater.jsonc"), filepath.Join(wd, "krd-updater.json"))
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		out = append(out, filepath.Join(d, "krd-updater.jsonc"), filepath.Join(d, "krd-updater.json"))
	}
	out = append(out, filepath.Join(userConfigDir(), "config.jsonc"), filepath.Join(userConfigDir(), "config.json"))
	return out
}

func userConfigDir() string {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "krd-updater")
		}
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "krd-updater")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "krd-updater")
	}
	return "."
}

// Load 读取配置文件并合并到默认值上；path 为空时自动搜索，找不到就用默认值。
// 返回的 Config 已完成路径展开与校验。
func Load(path string) (Config, error) {
	cfg := Default()
	used := ""
	for _, p := range CandidatePaths(path) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		used = p
		break
	}
	if used == "" {
		if path != "" {
			return cfg, fmt.Errorf("配置文件不存在: %s", path)
		}
		cfg.ConfigDir, _ = os.Getwd()
		return cfg, cfg.Validate()
	}
	cfg.ConfigPath = used
	cfg.ConfigDir = filepath.Dir(used)

	raw, err := os.ReadFile(used)
	if err != nil {
		return cfg, fmt.Errorf("读取配置失败: %w", err)
	}
	std, err := hujson.Standardize(raw)
	if err != nil {
		return cfg, fmt.Errorf("解析配置失败（%s）: %w", used, err)
	}
	var fc fileConfig
	dec := json.NewDecoder(strings.NewReader(string(std)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return cfg, fmt.Errorf("解析配置失败（%s）: %w", used, err)
	}
	cfg.applyFile(fc)
	return cfg, cfg.Validate()
}

func (c *Config) applyFile(fc fileConfig) {
	pickStr := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	pickInt := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	pickBool := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}

	pickStr(&c.InstallDir, fc.InstallDir)
	if fc.Sites != nil {
		c.Sites = append([]string(nil), (*fc.Sites)...)
	}
	pickInt(&c.TimeoutSeconds, fc.TimeoutSeconds)
	pickInt(&c.Retries, fc.Retries)
	pickInt(&c.RetryBackoffSeconds, fc.RetryBackoffSeconds)
	pickInt(&c.ParallelDownloads, fc.ParallelDownloads)
	if fc.MaxBundleBytes != nil {
		c.MaxBundleBytes = *fc.MaxBundleBytes
	}
	pickInt(&c.MaxBundleFiles, fc.MaxBundleFiles)
	pickBool(&c.SkipAssets, fc.SkipAssets)
	pickBool(&c.DeleteRemoved, fc.DeleteRemoved)
	if fc.Backup != nil {
		pickBool(&c.Backup.Enabled, fc.Backup.Enabled)
		pickStr(&c.Backup.Dir, fc.Backup.Dir)
	}
	pickStr(&c.TempDir, fc.TempDir)
	pickBool(&c.KeepTemp, fc.KeepTemp)
	pickStr(&c.LogFile, fc.LogFile)
	pickStr(&c.UserAgent, fc.UserAgent)
	pickStr(&c.Proxy, fc.Proxy)
	pickBool(&c.InsecureTLS, fc.InsecureTLS)
	pickStr(&c.HashFile, fc.HashFile)
	pickStr(&c.IndexFile, fc.IndexFile)
	pickBool(&c.Elevate, fc.Elevate)
	pickStr(&c.Progress, fc.Progress)
	pickBool(&c.JSONOutput, fc.JSONOutput)
	pickBool(&c.AssumeYes, fc.AssumeYes)
	pickBool(&c.Verbose, fc.Verbose)
}

// ApplyEnv 用 KRD_* 环境变量覆盖配置（优先级高于配置文件）。
func (c *Config) ApplyEnv() error {
	if v := os.Getenv("KRD_INSTALL_DIR"); v != "" {
		c.InstallDir = v
	}
	if v := os.Getenv("KRD_SITES"); v != "" {
		var list []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		if len(list) > 0 {
			c.Sites = list
		}
	}
	intVar := func(name string, dst *int) error {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s=%q 不是整数", name, v)
			}
			*dst = n
		}
		return nil
	}
	for _, it := range []struct {
		name string
		dst  *int
	}{
		{"KRD_TIMEOUT", &c.TimeoutSeconds},
		{"KRD_RETRIES", &c.Retries},
		{"KRD_PARALLEL", &c.ParallelDownloads},
		{"KRD_MAX_BUNDLE_FILES", &c.MaxBundleFiles},
	} {
		if err := intVar(it.name, it.dst); err != nil {
			return err
		}
	}
	boolVar := func(name string, dst *bool) error {
		if v := os.Getenv(name); v != "" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s=%q 不是布尔值", name, v)
			}
			*dst = b
		}
		return nil
	}
	for _, it := range []struct {
		name string
		dst  *bool
	}{
		{"KRD_SKIP_ASSETS", &c.SkipAssets},
		{"KRD_DELETE_REMOVED", &c.DeleteRemoved},
		{"KRD_BACKUP", &c.Backup.Enabled},
		{"KRD_ELEVATE", &c.Elevate},
		{"KRD_JSON", &c.JSONOutput},
		{"KRD_YES", &c.AssumeYes},
		{"KRD_VERBOSE", &c.Verbose},
		{"KRD_INSECURE_TLS", &c.InsecureTLS},
	} {
		if err := boolVar(it.name, it.dst); err != nil {
			return err
		}
	}
	strVar := func(name string, dst *string) {
		if v := os.Getenv(name); v != "" {
			*dst = v
		}
	}
	strVar("KRD_TEMP_DIR", &c.TempDir)
	strVar("KRD_LOG_FILE", &c.LogFile)
	strVar("KRD_PROXY", &c.Proxy)
	strVar("KRD_USER_AGENT", &c.UserAgent)
	strVar("KRD_PROGRESS", &c.Progress)
	strVar("KRD_BACKUP_DIR", &c.Backup.Dir)
	return c.Validate()
}

// Finalize 做最后一步：展开路径变量、规范化更新源，然后校验。
func (c *Config) Finalize() error {
	c.InstallDir = c.resolvePath(c.InstallDir)
	c.Backup.Dir = c.resolvePath(c.Backup.Dir)
	c.TempDir = c.resolvePath(c.TempDir)
	c.LogFile = c.resolvePath(c.LogFile)
	for i, s := range c.Sites {
		s = strings.TrimSpace(s)
		if s != "" && !strings.HasSuffix(s, "/") {
			s += "/"
		}
		c.Sites[i] = s
	}
	if c.UserAgent == "" {
		c.UserAgent = Default().UserAgent
	}
	return c.Validate()
}

func (c *Config) resolvePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = os.ExpandEnv(p) // 支持 ${VAR} / $VAR（Windows 下也支持 %VAR% 需用户自己写好）
	if strings.HasPrefix(p, "~") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(os.PathSeparator)))
		}
	}
	if isForeignAbsPath(p) {
		// 另一套系统的绝对路径：既不拼 ConfigDir，也不做 Clean，
		// 否则 Windows 路径在 Linux 上会被拼成 /home/xx/D:\...
		return p
	}
	if !filepath.IsAbs(p) && c.ConfigDir != "" {
		p = filepath.Join(c.ConfigDir, p)
	}
	return filepath.Clean(p)
}

// isForeignAbsPath 判断这是不是另一个系统的绝对路径。
// 配置文件会在机器之间拷贝，Windows 写法出现在 Linux 上（或反过来）很常见。
func isForeignAbsPath(p string) bool {
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, `\\`)
	}
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return strings.HasPrefix(p, `\\`)
}

// Validate 检查明显写错的配置，尽早失败并给出人话提示。
func (c *Config) Validate() error {
	if c.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds 必须 > 0（当前 %d）", c.TimeoutSeconds)
	}
	if c.Retries < 0 {
		return fmt.Errorf("retries 不能为负（当前 %d）", c.Retries)
	}
	if c.RetryBackoffSeconds < 0 {
		return fmt.Errorf("retry_backoff_seconds 不能为负（当前 %d）", c.RetryBackoffSeconds)
	}
	if c.ParallelDownloads < 1 || c.ParallelDownloads > 32 {
		return fmt.Errorf("parallel_downloads 必须在 1..32 之间（当前 %d）", c.ParallelDownloads)
	}
	if c.MaxBundleBytes <= 0 {
		return fmt.Errorf("max_bundle_bytes 必须 > 0（当前 %d）", c.MaxBundleBytes)
	}
	if c.MaxBundleFiles < 1 {
		return fmt.Errorf("max_bundle_files 必须 >= 1（当前 %d）", c.MaxBundleFiles)
	}
	switch c.Progress {
	case "auto", "plain", "none":
	default:
		return fmt.Errorf("progress 只能是 auto/plain/none（当前 %q）", c.Progress)
	}
	var valid []string
	for _, s := range c.Sites {
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return fmt.Errorf("更新源必须以 http:// 或 https:// 开头: %q", s)
		}
		valid = append(valid, s)
	}
	if len(valid) == 0 {
		return fmt.Errorf("sites 不能为空")
	}
	c.Sites = valid
	return nil
}
