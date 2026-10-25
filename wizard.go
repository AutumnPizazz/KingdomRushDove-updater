// 控制台交互向导，供没有图形环境、或者想一步步确认的人用。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"krd-updater/internal/config"
	"krd-updater/internal/osutil"
	"krd-updater/internal/pipeline"
	"krd-updater/internal/ui"
)

func runWizard(ctx context.Context) int {
	fs := flag.NewFlagSet("wizard", flag.ContinueOnError)
	var c commonOpts
	addCommon(fs, &c)
	_ = fs.Parse(os.Args[1:])

	cfg, err := loadConfig(fs, &c)
	if err != nil {
		return fail(exitConfig, err)
	}

	fmt.Printf("王国保卫战 Dove 版 更新器 %s\n", Version)
	fmt.Println(strings.Repeat("─", 46))

	dir, ok := askInstallDir(cfg)
	if !ok {
		fmt.Println("已取消。")
		return exitOK
	}
	cfg.InstallDir = dir

	hash, herr := pipeline.ReadLocalHash(cfg)
	localHash := hash
	if herr != nil {
		localHash = ""
	}

	fmt.Println()
	fmt.Printf("安装目录: %s\n", cfg.InstallDir)
	fmt.Printf("本地版本: %s\n", localVersionID(cfg.InstallDir))
	if localHash != "" {
		fmt.Printf("本地提交: %s\n", localHash)
	} else {
		fmt.Println("本地提交: （缺失）")
	}
	fmt.Println()
	fmt.Println("请选择操作：")
	fmt.Println("  1) 检查并更新（默认）")
	fmt.Println("  2) 打开图形界面调整配置 / 查看进度")
	fmt.Println("  3) 校验本地资源是否完整")
	fmt.Println("  0) 退出")
	choice := askLine("请输入序号", "1")

	switch strings.TrimSpace(choice) {
	case "0", "q", "Q":
		return exitOK
	case "2":
		return launchGUI(ctx, cfg)
	case "3":
		return wizardVerify(ctx, cfg)
	}

	log, err := ui.NewLogger(cfg.Verbose, cfg.LogFile, false)
	if err != nil {
		return fail(exitConfig, err)
	}
	defer log.Close()

	client, err := newClient(cfg, log)
	if err != nil {
		return fail(exitConfig, err)
	}

	fmt.Println()
	log.Step("正在检查更新……")
	plan, err := pipeline.Check(ctx, cfg, log, client, false)
	if err != nil {
		log.Error("%v", err)
		return fail(classify(err), err)
	}
	printPlan(log, plan, cfg)

	if plan.AlreadyLatest {
		fmt.Println()
		fmt.Println("已经是最新版本，无需更新。")
		return exitOK
	}

	fmt.Println()
	if running, exe := osutil.GameRunning(cfg.InstallDir); running {
		fmt.Printf("检测到游戏正在运行（%s），请先关闭游戏。\n", exe)
		return exitOK
	}
	if !confirm("开始更新？", true) {
		fmt.Println("已取消。")
		return exitOK
	}

	if relaunched, eerr := maybeElevate(cfg, log); eerr != nil {
		return fail(exitPermission, eerr)
	} else if relaunched {
		return exitOK
	}

	fmt.Println()
	res, err := pipeline.Apply(ctx, cfg, log, client, plan, pipeline.Options{
		SkipAssets: cfg.SkipAssets,
		NoDelete:   !cfg.DeleteRemoved,
		Backup:     cfg.Backup.Enabled,
		BackupDir:  cfg.Backup.Dir,
		KeepTemp:   cfg.KeepTemp,
		Verbose:    cfg.Verbose,
	})
	if err != nil {
		log.Error("%v", err)
		return fail(classify(err), err)
	}

	fmt.Println()
	log.Step("更新完成")
	log.Info("代码/数据 %d 个，资源 %d 个，删除 %d 个，共写入 %s，用时 %s",
		res.CodeFiles, res.AssetFiles, res.DeletedFiles, ui.HumanBytes(res.Bytes), ui.HumanDuration(res.Elapsed))
	if res.BackupDir != "" {
		log.Info("旧文件已备份到: %s", res.BackupDir)
	}
	fmt.Println()
	return exitOK
}

// askInstallDir 确认安装目录：能自动探测就给默认值，探测不到就要求手动输入。
func askInstallDir(cfg config.Config) (string, bool) {
	if cfg.InstallDir != "" && osutil.IsInstallDir(cfg.InstallDir) {
		fmt.Printf("检测到安装目录: %s\n", cfg.InstallDir)
		if confirm("使用这个目录？", true) {
			return cfg.InstallDir, true
		}
	}
	if cfg.InstallDir == "" {
		if d, err := osutil.DetectInstallDir(); err == nil {
			fmt.Printf("自动检测到安装目录: %s\n", d)
			if confirm("使用这个目录？", true) {
				return d, true
			}
		}
	} else {
		fmt.Printf("目录不可用: %s\n", cfg.InstallDir)
	}

	for i := 0; i < 5; i++ {
		in := askLine("请输入游戏安装目录（含 main.lua 与 _assets 的那一层，可直接把文件夹拖进来）", "")
		in = strings.TrimSpace(in)
		in = strings.Trim(in, `"`)
		in = strings.TrimSpace(in)
		if in == "" {
			continue
		}
		if abs, err := filepath.Abs(in); err == nil {
			in = abs
		}
		if osutil.IsInstallDir(in) {
			return in, true
		}
		fmt.Printf("这个目录里没有 main.lua / _assets：%s\n", in)
	}
	return "", false
}

func wizardVerify(ctx context.Context, cfg config.Config) int {
	log, err := ui.NewLogger(cfg.Verbose, cfg.LogFile, false)
	if err != nil {
		return fail(exitConfig, err)
	}
	defer log.Close()
	client, err := newClient(cfg, log)
	if err != nil {
		return fail(exitConfig, err)
	}
	log.Step("正在校验资源……")
	body, err := client.AssetIndex(ctx, cfg.IndexFile)
	if err != nil {
		return fail(exitNetwork, err)
	}
	bad, checked := verifyIndex(cfg, body)
	log.Info("校验 %d 个资源，异常 %d 个", checked, len(bad))
	for i, p := range bad {
		if i >= 30 {
			log.Info("  …… 其余 %d 个省略", len(bad)-i)
			break
		}
		log.Info("  ! %s", p)
	}
	if len(bad) == 0 {
		fmt.Println("全部资源完整。")
		return exitOK
	}
	fmt.Println("存在缺失/损坏的资源，重新执行一次「检查并更新」即可补全。")
	return exitDownload
}

func launchGUI(ctx context.Context, cfg config.Config) int {
	return runGUIWith(ctx, cfg, "")
}

// askLine 必须按整行读：安装路径里常有空格（D:\Program Files\...），
// 用 fmt.Scanln 会在空格处截断。
func askLine(prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

var stdinReader = bufio.NewReader(os.Stdin)
