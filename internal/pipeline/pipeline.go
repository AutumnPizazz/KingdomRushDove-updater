// Package pipeline 把「查提交 -> 差量资源 -> 下载 -> 落地」串成一条可复用的流程。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"krd-updater/internal/assetindex"
	"krd-updater/internal/config"
	"krd-updater/internal/osutil"
	"krd-updater/internal/remote"
	"krd-updater/internal/ui"
)

// Plan 是一次更新的完整计划，先算出来再决定要不要执行。
type Plan struct {
	Site          string
	LocalHash     string
	RemoteHash    string
	Commits       []remote.Commit
	CodeFiles     []string
	DeletedFiles  []string
	AssetNeeds    []assetindex.Need
	AssetTotal    int64
	AlreadyLatest bool
	LocalAssets   int // 本地索引条数，0 表示本地没有索引（会全量同步资源）
}

// Options 是执行期的开关（命令行/配置投影过来的）。
type Options struct {
	DryRun     bool
	SkipAssets bool
	NoDelete   bool
	Backup     bool
	BackupDir  string
	KeepTemp   bool
	Verbose    bool
}

// Result 汇总一次执行的结果。
type Result struct {
	AlreadyLatest bool
	CodeFiles     int
	AssetFiles    int
	DeletedFiles  int
	Bytes         int64
	BackupDir     string
	Elapsed       time.Duration
	SkippedAssets bool
	TempLeft      string
}

var hashRe = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)

// ReadLocalHash 读取本地版本标记。
func ReadLocalHash(cfg config.Config) (string, error) {
	p := filepath.Join(cfg.InstallDir, cfg.HashFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("读不到版本标记 %s：%w\n提示：请先完整安装一次本体，或用 `krd-updater set-hash <提交号>` 手动写入", p, err)
	}
	h := strings.TrimSpace(string(raw))
	if !hashRe.MatchString(h) {
		return "", fmt.Errorf("版本标记内容不是 40 位提交号: %q（文件 %s）\n提示：用 `krd-updater set-hash <提交号>` 修正", h, p)
	}
	return strings.ToLower(h), nil
}

// WriteLocalHash 写入版本标记。
func WriteLocalHash(cfg config.Config, hash string) error {
	if !hashRe.MatchString(hash) {
		return fmt.Errorf("提交号必须是 40 位十六进制: %q", hash)
	}
	return writeHashFile(filepath.Join(cfg.InstallDir, cfg.HashFile), strings.ToLower(hash))
}

// Check 计算更新计划（只读网络与本地索引，不改动任何文件）。
func Check(ctx context.Context, cfg config.Config, log *ui.Logger, client *remote.Client, skipAssets bool) (*Plan, error) {
	localHash, err := ReadLocalHash(cfg)
	if err != nil {
		return nil, err
	}
	site, err := client.SelectSite(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := client.Commits(ctx, localHash)
	if err != nil {
		return nil, err
	}
	plan := &Plan{
		Site:       site,
		LocalHash:  localHash,
		RemoteHash: resp.MasterCommitHash,
		Commits:    resp.Commits,
	}
	if strings.EqualFold(resp.MasterCommitHash, localHash) {
		plan.AlreadyLatest = true
		return plan, nil
	}
	plan.CodeFiles = append(plan.CodeFiles, resp.AddedOrModifiedFiles...)
	plan.DeletedFiles = append(plan.DeletedFiles, resp.DeletedFiles...)

	if skipAssets || cfg.SkipAssets {
		plan.AssetNeeds = nil
		return plan, nil
	}
	if err := fillAssetNeeds(ctx, cfg, log, client, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func fillAssetNeeds(ctx context.Context, cfg config.Config, log *ui.Logger, client *remote.Client, plan *Plan) error {
	body, err := client.AssetIndex(ctx, cfg.IndexFile)
	if err != nil {
		return fmt.Errorf("下载资源索引失败: %w", err)
	}
	remoteIdx := assetindex.Parse(body)
	if len(remoteIdx) == 0 {
		return fmt.Errorf("资源索引解析为空，服务端内容可能有变")
	}
	localPath := filepath.Join(cfg.InstallDir, filepath.FromSlash(cfg.IndexFile))
	localIdx, err := assetindex.ParseFile(localPath)
	if err != nil {
		log.Warn("本地资源索引不可用（%v），将按全量同步资源处理", err)
		localIdx = assetindex.Index{}
	}
	plan.LocalAssets = len(localIdx)
	plan.AssetNeeds = assetindex.Diff(filepath.Join(cfg.InstallDir, "_assets"), localIdx, remoteIdx)
	plan.AssetTotal = assetindex.TotalSize(plan.AssetNeeds)
	return nil
}

// Apply 执行计划：下载到临时目录，全部成功后再落盘，最后写版本标记。
func Apply(ctx context.Context, cfg config.Config, log *ui.Logger, client *remote.Client, plan *Plan, opt Options) (*Result, error) {
	start := time.Now()
	res := &Result{AlreadyLatest: plan.AlreadyLatest}
	if plan.AlreadyLatest {
		res.Elapsed = time.Since(start)
		return res, nil
	}
	if plan.Site == "" {
		return nil, errors.New("未选择更新源")
	}

	if opt.DryRun {
		res.CodeFiles = len(plan.CodeFiles)
		res.AssetFiles = len(plan.AssetNeeds)
		res.DeletedFiles = len(plan.DeletedFiles)
		res.Bytes = plan.AssetTotal
		res.SkippedAssets = len(plan.AssetNeeds) == 0 && cfg.SkipAssets
		res.Elapsed = time.Since(start)
		return res, nil
	}

	if running, exe := osutil.GameRunning(cfg.InstallDir); running {
		return nil, fmt.Errorf("游戏正在运行（%s），请先退出游戏再更新", exe)
	}

	tmp := cfg.TempDir
	if tmp == "" {
		tmp = os.TempDir()
	}
	workDir, err := os.MkdirTemp(tmp, "krd-update-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	if opt.KeepTemp {
		res.TempLeft = workDir
	} else {
		defer os.RemoveAll(workDir)
	}

	codeDir := filepath.Join(workDir, "code")
	assetDir := filepath.Join(workDir, "assets")

	// 1) 代码/数据
	if len(plan.CodeFiles) > 0 {
		log.Step("下载代码/数据（%d 个文件）", len(plan.CodeFiles))
		prog := newProgress(cfg, "代码", 0)
		batches := chunkByCount(plan.CodeFiles, cfg.MaxBundleFiles)
		for i, batch := range batches {
			log.Info("  第 %d/%d 批：%d 个文件", i+1, len(batches), len(batch))
			if err := fetchBundle(ctx, cfg, log, client, "code", batch, nil, codeDir, filepath.Join(workDir, fmt.Sprintf("code-%d.zip", i)), prog); err != nil {
				return nil, fmt.Errorf("下载代码批次失败: %w", err)
			}
		}
		prog.Finish()
	}

	// 2) 资源
	if len(plan.AssetNeeds) > 0 {
		log.Step("下载美术/音频资源（%d 个文件，共 %s）", len(plan.AssetNeeds), ui.HumanBytes(plan.AssetTotal))
		prog := newProgress(cfg, "资源", plan.AssetTotal)
		batches := chunkAssets(plan.AssetNeeds, cfg.MaxBundleBytes, cfg.MaxBundleFiles)
		for i, batch := range batches {
			log.Info("  第 %d/%d 批：%d 个文件，%s", i+1, len(batches), len(batch), ui.HumanBytes(assetindex.TotalSize(batch)))
			sizes := make(map[string]int64, len(batch))
			files := make([]string, 0, len(batch))
			for _, n := range batch {
				sizes[n.Path] = n.Size
				files = append(files, n.Path)
			}
			zipPath := filepath.Join(workDir, fmt.Sprintf("assets-%d.zip", i))
			if err := fetchBundle(ctx, cfg, log, client, "assets", files, sizes, assetDir, zipPath, prog); err != nil {
				return nil, fmt.Errorf("下载资源批次失败: %w", err)
			}
		}
		prog.Finish()
	}

	// 3) 落地
	log.Step("写入安装目录")
	if cfg.Elevate && !osutil.CanWriteTree(cfg.InstallDir) && !osutil.IsElevated() {
		return nil, fmt.Errorf("安装目录不可写且当前不是管理员：请以管理员身份运行，或把 elevate 设为 true")
	}

	if opt.Backup {
		backupDir := opt.BackupDir
		if backupDir == "" {
			backupDir = filepath.Join(filepath.Dir(cfg.InstallDir),
				"KRD_backup_"+time.Now().Format("20060102-150405"))
		}
		n, err := backupFiles(cfg.InstallDir, plan.CodeFiles, backupDir)
		if err != nil {
			return nil, err
		}
		assetRels := make([]string, 0, len(plan.AssetNeeds))
		for _, n2 := range plan.AssetNeeds {
			assetRels = append(assetRels, path.Join("_assets", n2.Path))
		}
		// 资源在 _assets 下，路径要带上前缀再备份
		m, err := backupFiles(cfg.InstallDir, assetRels, backupDir)
		if err != nil {
			return nil, err
		}
		res.BackupDir = backupDir
		log.Info("  已备份 %d 个旧文件到 %s", n+m, backupDir)
	}

	if len(plan.CodeFiles) > 0 {
		n, bytesWritten, err := copyTree(codeDir, cfg.InstallDir)
		if err != nil {
			return nil, err
		}
		res.CodeFiles = n
		res.Bytes += bytesWritten
		log.Info("  代码/数据：%d 个文件，%s", n, ui.HumanBytes(bytesWritten))
	}
	if len(plan.AssetNeeds) > 0 {
		n, bytesWritten, err := copyTree(assetDir, filepath.Join(cfg.InstallDir, "_assets"))
		if err != nil {
			return nil, err
		}
		res.AssetFiles = n
		res.Bytes += bytesWritten
		log.Info("  资源：      %d 个文件，%s", n, ui.HumanBytes(bytesWritten))
	}

	if len(plan.DeletedFiles) > 0 {
		if opt.NoDelete {
			log.Warn("按配置跳过删除 %d 个已废弃文件", len(plan.DeletedFiles))
		} else {
			n, err := deleteFiles(cfg.InstallDir, plan.DeletedFiles)
			if err != nil {
				return nil, err
			}
			res.DeletedFiles = n
			log.Info("  删除已废弃文件：%d 个", n)
		}
	}

	if err := WriteLocalHash(cfg, plan.RemoteHash); err != nil {
		return nil, fmt.Errorf("更新完成但写入版本标记失败: %w", err)
	}
	log.Info("  版本标记已更新为 %s", plan.RemoteHash)
	res.Elapsed = time.Since(start)
	return res, nil
}

func newProgress(cfg config.Config, label string, total int64) *ui.Progress {
	enabled := ui.ProgressEnabled(cfg.Progress, cfg.JSONOutput)
	return ui.NewProgress(os.Stdout, enabled, label, total)
}

func chunkByCount(files []string, max int) [][]string {
	if max <= 0 {
		max = 200
	}
	var out [][]string
	for i := 0; i < len(files); i += max {
		j := i + max
		if j > len(files) {
			j = len(files)
		}
		out = append(out, files[i:j])
	}
	return out
}

func chunkAssets(need []assetindex.Need, maxBytes int64, maxCount int) [][]assetindex.Need {
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	if maxCount <= 0 {
		maxCount = 200
	}
	var out [][]assetindex.Need
	var cur []assetindex.Need
	var curBytes int64
	for _, n := range need {
		tooBig := len(cur) >= maxCount || (len(cur) > 0 && curBytes+n.Size > maxBytes)
		if tooBig {
			out = append(out, cur)
			cur = nil
			curBytes = 0
		}
		cur = append(cur, n)
		curBytes += n.Size
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// fetchBundle 优先走打包接口；失败则回退逐文件下载。
func fetchBundle(ctx context.Context, cfg config.Config, log *ui.Logger, client *remote.Client,
	kind string, files []string, sizes map[string]int64, destRoot, zipPath string, prog *ui.Progress) error {

	allow := make(map[string]bool, len(files))
	for _, f := range files {
		allow[f] = true
	}

	info, err := client.Bundle(ctx, kind, files)
	if err == nil {
		url := client.BundleURL(kind, info.BundleID)
		log.Debug("打包成功：%d 个文件，%s -> %s", info.FileCount, ui.HumanBytes(info.Size), url)
		if err := client.Download(ctx, url, zipPath, 0, prog); err != nil {
			log.Warn("打包下载失败（%v），回退逐文件下载", err)
		} else if _, written, xerr := extractZipTo(zipPath, destRoot, allow); xerr != nil {
			log.Warn("解压失败（%v），回退逐文件下载", xerr)
		} else {
			_ = written
			missing := missingFiles(files, destRoot)
			if len(missing) == 0 {
				return nil
			}
			log.Warn("压缩包缺少 %d 个文件，回退逐文件下载补齐", len(missing))
			return fetchPerFile(ctx, cfg, client, kind, missing, sizes, destRoot, prog)
		}
	} else {
		log.Debug("打包接口不可用（%v），回退逐文件下载", err)
	}
	return fetchPerFile(ctx, cfg, client, kind, files, sizes, destRoot, prog)
}

func missingFiles(files []string, destRoot string) []string {
	var missing []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(destRoot, filepath.FromSlash(f))); err != nil {
			missing = append(missing, f)
		}
	}
	return missing
}

func fetchPerFile(ctx context.Context, cfg config.Config, client *remote.Client, kind string,
	files []string, sizes map[string]int64, destRoot string, prog *ui.Progress) error {

	isAsset := kind == "assets"
	workers := cfg.ParallelDownloads
	if workers < 1 {
		workers = 4
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				if ctx.Err() != nil {
					return
				}
				url := client.FileURL(rel, isAsset)
				dst := filepath.Join(destRoot, filepath.FromSlash(rel))
				var expected int64
				if sizes != nil {
					expected = sizes[rel]
				}
				if err := client.Download(ctx, url, dst, expected, prog); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", rel, err)
					}
					mu.Unlock()
					continue
				}
				if expected > 0 {
					if st, err := os.Stat(dst); err != nil || st.Size() != expected {
						mu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("%s: 下载后大小不符", rel)
						}
						mu.Unlock()
					}
				}
			}
		}()
	}
	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

// copyTree 把 src 下的所有文件复制进 dst，返回文件数与字节数。
func copyTree(src, dst string) (int, int64, error) {
	var files int
	var total int64
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := copyFile(p, target); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", rel, err)
		}
		files++
		total += info.Size()
		return nil
	})
	return files, total, err
}
