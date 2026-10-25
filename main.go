// krd-updater：王国保卫战 Dove 版增量更新器。
//
// 这是 KingdomRushDove（https://github.com/CrazySpottedDove/KingdomRushDove）的第三方更新器，
// 与游戏本体分开维护，不代表上游。协议细节见 internal/remote。
//
// 服务端协议跟游戏内置更新器一致，只是把检查、下载、落地拆成了独立命令：
// 不带参数打开图形界面，带参数按子命令执行。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"krd-updater/internal/assetindex"
	"krd-updater/internal/config"
	"krd-updater/internal/gui"
	"krd-updater/internal/osutil"
	"krd-updater/internal/pipeline"
	"krd-updater/internal/remote"
	"krd-updater/internal/ui"
)

// Version 由 Makefile 的 -ldflags -X main.Version 注入。
var Version = "1.0.0"

// 退出码：方便脚本判断失败原因。
const (
	exitOK         = 0
	exitUsage      = 2
	exitConfig     = 3
	exitNetwork    = 4
	exitInstall    = 5
	exitPermission = 6
	exitDownload   = 7
	exitApply      = 8
)

const helpText = `krd-updater %s：王国保卫战 Dove 版增量更新器（第三方，非官方）

用法：
  krd-updater                          打开图形界面（双击即可用）
  krd-updater wizard                   控制台交互向导（无浏览器环境用）
  krd-updater gui [--addr A] [--no-open]  只开图形界面
                   --idle-timeout 2m      页面关掉多久后退出，0 表示不退出
  krd-updater update [选项]            检查并更新
  krd-updater check  [选项]            只检查，不修改任何文件
  krd-updater status [选项]            显示本地/服务端版本与资源概况
  krd-updater verify [选项]            逐个比对本地资源哈希与远端索引
  krd-updater config init [--path X]   生成带注释的配置文件
  krd-updater config show              打印合并后的最终配置
  krd-updater set-hash <提交号>        手动写入本地版本标记

通用选项：
  --config PATH     指定配置文件（默认依次找 ./krd-updater.jsonc、exe 同目录、用户配置目录）
  --dir PATH        指定游戏安装目录（含 main.lua 与 _assets 的那一层）
  --site URL        指定更新源（可重复，覆盖配置里的 sites）
  --progress MODE   auto|plain|none，默认 auto
  --json            结果以 JSON 输出（进度与日志改走 stderr）
  --verbose         输出调试信息
  -h, --help        显示帮助

update/check 专用：
  --dry-run         只报告将要做什么，不下载不落地
  --yes             不再询问确认
  --backup          覆盖前备份旧文件
  --no-delete       不删除服务端标记为删除的文件
  --no-assets       只更新代码/数据，跳过美术音频
  --keep-temp       保留临时目录，便于排查问题

环境变量（优先级低于命令行、高于配置文件）：
  KRD_CONFIG KRD_INSTALL_DIR KRD_SITES KRD_TIMEOUT KRD_RETRIES KRD_PARALLEL
  KRD_SKIP_ASSETS KRD_DELETE_REMOVED KRD_BACKUP KRD_BACKUP_DIR KRD_TEMP_DIR
  KRD_LOG_FILE KRD_PROXY KRD_USER_AGENT KRD_PROGRESS KRD_JSON KRD_YES
  KRD_VERBOSE KRD_ELEVATE KRD_INSECURE_TLS KRD_MAX_BUNDLE_FILES

项目与说明：https://github.com/AutumnPizazz/KingdomRushDove-updater

退出码：0 成功/已最新  2 用法错误  3 配置错误  4 网络错误  5 安装目录错误
        6 权限不足  7 下载失败  8 落地失败
`

func main() {
	osutil.EnsureStdoutIsConsole()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := run(ctx, os.Args[1:])
	os.Exit(code)
}

// commonOpts 是所有子命令共享的开关。
type commonOpts struct {
	configPath string
	dir        string
	site       []string
	progress   string
	jsonOut    bool
	verbose    bool
	yes        bool
	backup     bool
	noDelete   bool
	noAssets   bool
	dryRun     bool
	keepTemp   bool
	outPath    string
}

type strList []string

func (s *strList) String() string { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func addCommon(fs *flag.FlagSet, c *commonOpts) {
	fs.StringVar(&c.configPath, "config", "", "配置文件路径")
	fs.StringVar(&c.dir, "dir", "", "游戏安装目录")
	fs.Var((*strList)(&c.site), "site", "更新源（可重复）")
	fs.StringVar(&c.progress, "progress", "", "auto|plain|none")
	fs.BoolVar(&c.jsonOut, "json", false, "JSON 输出")
	fs.BoolVar(&c.verbose, "verbose", false, "调试输出")
	fs.BoolVar(&c.yes, "yes", false, "不再确认")
	fs.BoolVar(&c.backup, "backup", false, "覆盖前备份")
	fs.BoolVar(&c.noDelete, "no-delete", false, "不删除废弃文件")
	fs.BoolVar(&c.noAssets, "no-assets", false, "跳过资源")
	fs.BoolVar(&c.dryRun, "dry-run", false, "只报告不执行")
	fs.BoolVar(&c.keepTemp, "keep-temp", false, "保留临时目录")
}

// loadConfig 汇总「默认值 <- 文件 <- 环境变量 <- 命令行」。
func loadConfig(fs *flag.FlagSet, c *commonOpts) (config.Config, error) {
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return cfg, err
	}
	if err := cfg.ApplyEnv(); err != nil {
		return cfg, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if set["dir"] && c.dir != "" {
		cfg.InstallDir = c.dir
	}
	if set["site"] && len(c.site) > 0 {
		cfg.Sites = append([]string(nil), c.site...)
	}
	if set["progress"] {
		cfg.Progress = c.progress
	}
	if set["json"] {
		cfg.JSONOutput = c.jsonOut
	}
	if set["verbose"] {
		cfg.Verbose = c.verbose
	}
	if set["yes"] {
		cfg.AssumeYes = c.yes
	}
	if set["backup"] {
		cfg.Backup.Enabled = c.backup
	}
	if set["no-delete"] {
		cfg.DeleteRemoved = !c.noDelete
	}
	if set["no-assets"] {
		cfg.SkipAssets = c.noAssets
	}
	return cfg, cfg.Finalize()
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		// 双击运行时直接进图形界面，改配置/看进度都在浏览器里。
		return runGUI(ctx, nil)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Printf(helpText, Version)
		return exitOK
	case "-v", "--version", "version":
		fmt.Printf("krd-updater %s\n", Version)
		return exitOK
	case "wizard", "console", "cli":
		return runWizard(ctx)
	case "update":
		return runUpdate(ctx, args[1:])
	case "check":
		return runCheck(ctx, args[1:])
	case "status":
		return runStatus(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	case "gui", "config-gui", "--gui":
		return runGUI(ctx, args[1:])
	case "config":
		return runConfig(args[1:])
	case "set-hash":
		return runSetHash(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", args[0])
		fmt.Printf(helpText, Version)
		return exitUsage
	}
}

// prepare 做好所有子命令都要的准备工作：配置、安装目录、日志、客户端。
func prepare(ctx context.Context, fs *flag.FlagSet, c *commonOpts, needClient bool) (config.Config, *ui.Logger, *remote.Client, int, error) {
	cfg, err := loadConfig(fs, c)
	if err != nil {
		return cfg, nil, nil, exitConfig, err
	}
	if cfg.InstallDir == "" {
		d, derr := osutil.DetectInstallDir()
		if derr != nil {
			return cfg, nil, nil, exitInstall, derr
		}
		cfg.InstallDir = d
	}
	if !osutil.IsInstallDir(cfg.InstallDir) {
		return cfg, nil, nil, exitInstall, fmt.Errorf("这个目录不像游戏安装目录（缺少 main.lua/_assets）: %s", cfg.InstallDir)
	}
	log, err := ui.NewLogger(cfg.Verbose, cfg.LogFile, cfg.JSONOutput)
	if err != nil {
		return cfg, nil, nil, exitConfig, err
	}
	if !needClient {
		return cfg, log, nil, exitOK, nil
	}
	client, err := remote.New(cfg, log)
	if err != nil {
		return cfg, log, nil, exitConfig, err
	}
	return cfg, log, client, exitOK, nil
}

func fail(code int, err error) int {
	fmt.Fprintf(os.Stderr, "[错误] %v\n", err)
	return code
}

// maybeElevate 在 Windows 上发现目录不可写且没有管理员权限时，自动以管理员身份重启自己。
func maybeElevate(cfg config.Config, log *ui.Logger) (bool, error) {
	if !osutil.IsWindows() || !cfg.Elevate || osutil.IsElevated() {
		return false, nil
	}
	if osutil.CanWriteTree(cfg.InstallDir) {
		return false, nil
	}
	log.Info("安装目录需要管理员权限，正在请求提权……")
	if err := osutil.RelaunchElevated(os.Args[1:]); err != nil {
		return false, err
	}
	return true, nil
}

func runUpdate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, log, client, code, err := prepare(ctx, fs, &c, true)
	if err != nil {
		return fail(code, err)
	}
	defer log.Close()

	log.Info("安装目录: %s", cfg.InstallDir)
	plan, err := pipeline.Check(ctx, cfg, log, client, c.noAssets)
	if err != nil {
		return fail(classify(err), err)
	}
	printPlan(log, plan, cfg)

	if plan.AlreadyLatest && !c.dryRun {
		log.Info("已经是最新版本（本地与服务端 master 无差异）。")
		return exitOK
	}
	if c.dryRun {
		printDryRun(log, plan, cfg)
		return exitOK
	}
	if !cfg.AssumeYes && !confirm("确认执行更新？", true) {
		log.Info("已取消。")
		return exitOK
	}
	if relaunched, eerr := maybeElevate(cfg, log); eerr != nil {
		return fail(exitPermission, eerr)
	} else if relaunched {
		return exitOK
	}

	res, err := pipeline.Apply(ctx, cfg, log, client, plan, pipeline.Options{
		SkipAssets: c.noAssets,
		NoDelete:   !cfg.DeleteRemoved,
		Backup:     cfg.Backup.Enabled,
		BackupDir:  cfg.Backup.Dir,
		KeepTemp:   c.keepTemp,
		Verbose:    cfg.Verbose,
	})
	if err != nil {
		return fail(classify(err), err)
	}
	log.Step("完成")
	log.Info("用了 %s；代码 %d 个，资源 %d 个，删除 %d 个，共写入 %s",
		ui.HumanDuration(res.Elapsed), res.CodeFiles, res.AssetFiles, res.DeletedFiles, ui.HumanBytes(res.Bytes))
	if res.BackupDir != "" {
		log.Info("旧文件备份在: %s", res.BackupDir)
	}
	if cfg.JSONOutput {
		emitJSON(map[string]any{
			"ok":            true,
			"install_dir":   cfg.InstallDir,
			"from":          plan.LocalHash,
			"to":            plan.RemoteHash,
			"code_files":    res.CodeFiles,
			"asset_files":   res.AssetFiles,
			"deleted_files": res.DeletedFiles,
			"bytes":         res.Bytes,
			"backup_dir":    res.BackupDir,
			"seconds":       res.Elapsed.Seconds(),
		})
	}
	return exitOK
}

func runCheck(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, log, client, code, err := prepare(ctx, fs, &c, true)
	if err != nil {
		return fail(code, err)
	}
	defer log.Close()

	plan, err := pipeline.Check(ctx, cfg, log, client, c.noAssets)
	if err != nil {
		return fail(classify(err), err)
	}
	printPlan(log, plan, cfg)
	if cfg.JSONOutput {
		emitJSON(planJSON(cfg, plan))
		return exitOK
	}
	if plan.AlreadyLatest {
		log.Info("已经是最新版本（本地与服务端 master 无差异）。")
	} else {
		printDryRun(log, plan, cfg)
	}
	return exitOK
}

func runStatus(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, log, client, code, err := prepare(ctx, fs, &c, true)
	if err != nil {
		return fail(code, err)
	}
	defer log.Close()

	localHash, herr := pipeline.ReadLocalHash(cfg)
	if herr != nil {
		localHash = "(未知)"
	}
	site, serr := client.SelectSite(ctx)
	if serr != nil {
		return fail(exitNetwork, serr)
	}
	plan, perr := pipeline.Check(ctx, cfg, log, client, c.noAssets)
	remoteHash := "(未知)"
	if perr == nil {
		remoteHash = plan.RemoteHash
	}
	log.Info("安装目录: %s", cfg.InstallDir)
	log.Info("本地版本: %s（提交 %s）", localVersionID(cfg.InstallDir), localHash)
	log.Info("服务端  : %s", site)
	log.Info("服务端 master: %s", remoteHash)
	if perr == nil {
		log.Info("待更新提交: %d 条，代码/数据 %d 个文件，资源 %d 个（%s）",
			len(plan.Commits), len(plan.CodeFiles), len(plan.AssetNeeds), ui.HumanBytes(plan.AssetTotal))
	} else {
		log.Warn("检查更新失败: %v", perr)
	}
	if cfg.JSONOutput {
		emitJSON(map[string]any{
			"install_dir": cfg.InstallDir,
			"version":     localVersionID(cfg.InstallDir),
			"local_hash":  localHash,
			"site":        site,
			"remote_hash": remoteHash,
			"up_to_date":  perr == nil && plan.AlreadyLatest,
			"pending_commits": func() int {
				if plan == nil {
					return 0
				}
				return len(plan.Commits)
			}(),
		})
	}
	return exitOK
}

func runVerify(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, log, client, code, err := prepare(ctx, fs, &c, true)
	if err != nil {
		return fail(code, err)
	}
	defer log.Close()

	body, err := client.AssetIndex(ctx, cfg.IndexFile)
	if err != nil {
		return fail(exitNetwork, err)
	}
	idx := assetindex.Parse(body)
	bad, checked := assetindex.Verify(filepath.Join(cfg.InstallDir, "_assets"), idx)
	log.Info("校验资源 %d 个，索引条目 %d 个，异常 %d 个", checked, len(idx), len(bad))
	for i, p := range bad {
		if i >= 50 {
			log.Info("  …… 其余 %d 个省略", len(bad)-i)
			break
		}
		log.Info("  ! %s", p)
	}
	if cfg.JSONOutput {
		emitJSON(map[string]any{"checked": checked, "total": len(idx), "bad": bad})
	}
	if len(bad) > 0 {
		return exitDownload
	}
	return exitOK
}

func runConfig(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法: krd-updater config <init|show|path>")
		return exitUsage
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("config init", flag.ContinueOnError)
		path := fs.String("path", "krd-updater.jsonc", "输出路径")
		if err := fs.Parse(args[1:]); err != nil {
			return exitUsage
		}
		if err := config.WriteExample(*path); err != nil {
			return fail(exitConfig, err)
		}
		abs, _ := filepath.Abs(*path)
		fmt.Printf("已生成配置文件: %s\n", abs)
		return exitOK
	case "show":
		fs := flag.NewFlagSet("config show", flag.ContinueOnError)
		var c commonOpts
		addCommon(fs, &c)
		if err := fs.Parse(args[1:]); err != nil {
			return exitUsage
		}
		cfg, err := loadConfig(fs, &c)
		if err != nil {
			return fail(exitConfig, err)
		}
		out, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(out))
		if cfg.ConfigPath != "" {
			fmt.Fprintf(os.Stderr, "配置来源: %s\n", cfg.ConfigPath)
		} else {
			fmt.Fprintln(os.Stderr, "配置来源: 内置默认值")
		}
		return exitOK
	case "path":
		cfg, err := config.Load("")
		_ = cfg
		if err != nil {
			return fail(exitConfig, err)
		}
		for _, p := range config.CandidatePaths("") {
			mark := "  "
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				mark = "* "
			}
			fmt.Println(mark + p)
		}
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知 config 子命令: %s\n", args[0])
		return exitUsage
	}
}

func runSetHash(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法: krd-updater set-hash <40 位提交号> [--dir 安装目录]")
		return exitUsage
	}
	hash := args[0]
	fs := flag.NewFlagSet("set-hash", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	cfg, log, _, code, err := prepare(context.Background(), fs, &c, false)
	if err != nil {
		return fail(code, err)
	}
	defer log.Close()
	if err := pipeline.WriteLocalHash(cfg, hash); err != nil {
		return fail(exitConfig, err)
	}
	log.Info("已把 %s 的版本标记写成 %s", cfg.InstallDir, strings.ToLower(hash))
	return exitOK
}

func printPlan(log *ui.Logger, plan *pipeline.Plan, cfg config.Config) {
	log.Info("安装目录: %s（本地版本 %s）", cfg.InstallDir, localVersionID(cfg.InstallDir))
	log.Info("本地提交: %s", plan.LocalHash)
	log.Info("服务端 master: %s（%s）", plan.RemoteHash, plan.Site)
	if plan.AlreadyLatest {
		return
	}
	log.Info("待更新提交 %d 条：", len(plan.Commits))
	for _, cm := range plan.Commits {
		msg := strings.SplitN(cm.Message, "\n", 2)[0]
		log.Info("  - %s", msg)
	}
	if len(plan.DeletedFiles) > 0 {
		log.Info("待删除文件: %d 个", len(plan.DeletedFiles))
	}
	log.Info("代码/数据文件: %d 个", len(plan.CodeFiles))
	if cfg.SkipAssets {
		log.Info("资源检查: 已按配置跳过")
		return
	}
	log.Info("资源: 索引 %d 条，需要更新 %d 个，共 %s",
		plan.LocalAssets, len(plan.AssetNeeds), ui.HumanBytes(plan.AssetTotal))
}

func printDryRun(log *ui.Logger, plan *pipeline.Plan, cfg config.Config) {
	if cfg.JSONOutput {
		return
	}
	log.Info("")
	log.Info("将要执行：")
	log.Info("  代码/数据 %d 个文件", len(plan.CodeFiles))
	log.Info("  资源      %d 个文件（%s）", len(plan.AssetNeeds), ui.HumanBytes(plan.AssetTotal))
	if len(plan.DeletedFiles) > 0 {
		log.Info("  删除      %d 个文件", len(plan.DeletedFiles))
	}
	if cfg.Verbose {
		for _, f := range plan.CodeFiles {
			log.Info("    + %s", f)
		}
		for _, n := range plan.AssetNeeds {
			log.Info("    ~ %s", n.Path)
		}
	}
}

var versionPathRe = regexp.MustCompile(`(?m)^\s*id\s*=\s*"([^"]+)"`)

func localVersionID(installDir string) string {
	raw, err := os.ReadFile(filepath.Join(installDir, "version.lua"))
	if err != nil {
		return "未知"
	}
	if m := versionPathRe.FindSubmatch(raw); len(m) > 1 {
		return string(m[1])
	}
	return "未知"
}

func confirm(prompt string, def bool) bool {
	suffix := " [Y/n] "
	if !def {
		suffix = " [y/N] "
	}
	fmt.Print(prompt + suffix)
	line, err := stdinReader.ReadString('\n')
	if strings.TrimSpace(line) == "" {
		return def
	}
	_ = err
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是":
		return true
	case "n", "no", "否":
		return false
	}
	return def
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func planJSON(cfg config.Config, plan *pipeline.Plan) map[string]any {
	commits := make([]map[string]string, 0, len(plan.Commits))
	for _, c := range plan.Commits {
		commits = append(commits, map[string]string{"hash": c.Hash, "message": c.Message, "date": c.Date})
	}
	return map[string]any{
		"install_dir":   cfg.InstallDir,
		"site":          plan.Site,
		"local_hash":    plan.LocalHash,
		"remote_hash":   plan.RemoteHash,
		"up_to_date":    plan.AlreadyLatest,
		"commits":       commits,
		"code_files":    len(plan.CodeFiles),
		"asset_files":   len(plan.AssetNeeds),
		"asset_bytes":   plan.AssetTotal,
		"deleted_files": len(plan.DeletedFiles),
	}
}

func classify(err error) int {
	var he *remote.HTTPError
	if errors.As(err, &he) {
		switch he.Code {
		case 401, 403:
			return exitPermission
		case 404:
			return exitNetwork
		}
		return exitNetwork
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "所有更新源都不可用"), strings.Contains(msg, "下载"), strings.Contains(msg, "请求"):
		return exitNetwork
	case strings.Contains(msg, "版本标记"):
		return exitInstall
	case strings.Contains(msg, "权限"), strings.Contains(msg, "管理员"):
		return exitPermission
	case strings.Contains(msg, "写入"), strings.Contains(msg, "解压"), strings.Contains(msg, "复制"):
		return exitApply
	}
	return exitNetwork
}

// newClient 是 remote.New 的一层薄封装。
func newClient(cfg config.Config, log *ui.Logger) (*remote.Client, error) {
	return remote.New(cfg, log)
}

// verifyIndex 用远端索引逐个校验本地资源。
func verifyIndex(cfg config.Config, body []byte) (bad []string, checked int) {
	idx := assetindex.Parse(body)
	return assetindex.Verify(filepath.Join(cfg.InstallDir, "_assets"), idx)
}

// runGUI 启动本地图形界面（零依赖：内置 HTTP 服务 + 默认浏览器）。
func runGUI(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	addr := fs.String("addr", "127.0.0.1:0", "监听地址，0 表示随机端口")
	noOpen := fs.Bool("no-open", false, "不自动打开浏览器")
	idle := fs.Duration("idle-timeout", 2*time.Minute, "页面没动静多久后退出，0 表示不退出")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// 双击启动时把附带的控制台窗口藏起来；在已有终端里启动则保持原样。
	hidden := osutil.HideConsoleIfOwned()
	cfg, err := loadConfig(fs, &c)
	if err != nil {
		return fail(exitConfig, err)
	}
	if cfg.InstallDir == "" || !osutil.IsInstallDir(cfg.InstallDir) {
		if d, derr := osutil.DetectInstallDir(); derr == nil {
			cfg.InstallDir = d
		}
	}
	log, err := ui.NewLogger(cfg.Verbose, "", false)
	if err != nil {
		return fail(exitConfig, err)
	}
	defer log.Close()

	srv := gui.New(cfg)
	if err := srv.Run(ctx, *addr, !*noOpen, *idle); err != nil {
		if hidden {
			osutil.MessageBox("王国保卫战 Dove 版更新器", err.Error())
		}
		return fail(exitNetwork, err)
	}
	return exitOK
}

// runGUIWith 打开图形界面并阻塞到窗口关闭。
func runGUIWith(ctx context.Context, cfg config.Config, addr string) int {
	osutil.HideConsoleIfOwned()
	srv := gui.New(cfg)
	if err := srv.Run(ctx, addr, true, 2*time.Minute); err != nil {
		return fail(exitNetwork, err)
	}
	return exitOK
}
