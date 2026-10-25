// Package remote 封装更新服务端协议（提交查询、打包下载、单文件回退）。
//
// 协议与游戏内置更新器 dove_modules/updater/update_manager.lua 保持一致：
//
//	POST {site}commits            {"commit_hash":"<40位>"} -> {commits, added_or_modified_files, deleted_files, master_commit_hash}
//	GET  {site}file?file=<path>   取任意仓库文件（相对安装根目录）
//	GET  {site}_assets/<path>     取资源文件（相对 _assets 目录）
//	POST {site}bundle/{code|assets} {"files":[...]} -> {bundle_id, file_count, size}
//	GET  {site}bundle/{code|assets}?id=<id>        -> zip
package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"krd-updater/internal/config"
	"krd-updater/internal/ui"
)

// Commit 是一条待更新提交。
type Commit struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// CommitsResponse 是 POST /commits 的返回。
type CommitsResponse struct {
	Commits              []Commit `json:"commits"`
	AddedOrModifiedFiles []string `json:"added_or_modified_files"`
	DeletedFiles         []string `json:"deleted_files"`
	MasterCommitHash     string   `json:"master_commit_hash"`
}

// BundleInfo 是 POST /bundle/* 的返回。
type BundleInfo struct {
	BundleID  string `json:"bundle_id"`
	FileCount int    `json:"file_count"`
	Size      int64  `json:"size"`
}

// HTTPError 表示服务端返回了非 2xx。
type HTTPError struct {
	Code   int
	URL    string
	Detail string
}

func (e *HTTPError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("HTTP %d %s (%s)", e.Code, e.URL, e.Detail)
	}
	return fmt.Sprintf("HTTP %d %s", e.Code, e.URL)
}

// Client 是一个带重试与站点记录的 HTTP 客户端。
type Client struct {
	cfg    config.Config
	http   *http.Client
	log    *ui.Logger
	site   string
	failed map[string]error
}

// New 依据配置构造客户端。
func New(cfg config.Config, log *ui.Logger) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 8
	tr.ResponseHeaderTimeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy 配置不合法: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Transport: tr},
		log:    log,
		failed: map[string]error{},
	}, nil
}

// Site 返回当前使用的更新源。
func (c *Client) Site() string { return c.site }

func (c *Client) timeout() time.Duration {
	return time.Duration(c.cfg.TimeoutSeconds) * time.Second
}

// SelectSite 依次探测配置里的更新源，返回第一个可用的。
func (c *Client) SelectSite(ctx context.Context) (string, error) {
	if c.site != "" {
		return c.site, nil
	}
	var lastErr error
	for _, site := range c.cfg.Sites {
		// 探针用一个极小的静态文件；不能用全 0 的 commit_hash 打 /commits（服务端会 500）
		u := site + "file?file=" + url.QueryEscape("version.lua")
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		status, _, err := c.getOnce(reqCtx, u)
		cancel()
		if err == nil && status == http.StatusOK {
			c.site = site
			return site, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = &HTTPError{Code: status, URL: u}
		}
		if c.log != nil {
			c.log.Warn("更新源不可用，尝试下一个: %s（%v）", site, lastErr)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("配置里没有可用的更新源")
	}
	return "", fmt.Errorf("所有更新源都不可用: %w", lastErr)
}

func (c *Client) backoff(attempt int) time.Duration {
	base := time.Duration(c.cfg.RetryBackoffSeconds) * time.Second
	if base <= 0 {
		return 0
	}
	d := time.Duration(math.Min(float64(base)*math.Pow(2, float64(attempt)), float64(30*time.Second)))
	return d
}

func (c *Client) getOnce(ctx context.Context, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// get 带重试地 GET 整个响应体。
func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, c.timeout())
		status, body, err := c.getOnce(reqCtx, u)
		cancel()
		switch {
		case err != nil:
			lastErr = err
		case status >= 200 && status < 300:
			return body, nil
		case status == http.StatusBadRequest || status == http.StatusUnauthorized ||
			status == http.StatusForbidden || status == http.StatusNotFound:
			return nil, &HTTPError{Code: status, URL: u, Detail: snippet(body)}
		default:
			lastErr = &HTTPError{Code: status, URL: u, Detail: snippet(body)}
		}
		if attempt < c.cfg.Retries {
			if c.log != nil {
				c.log.Warn("请求失败，%.0fs 后重试（%d/%d）: %v",
					c.backoff(attempt).Seconds(), attempt+1, c.cfg.Retries, lastErr)
			}
			if d := c.backoff(attempt); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
	}
	return nil, fmt.Errorf("GET %s 失败: %w", u, lastErr)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// postJSON 带重试地 POST，并把返回解析进 out。
func (c *Client) postJSON(ctx context.Context, u string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, c.timeout())
		req, rerr := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(raw))
		if rerr != nil {
			cancel()
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", c.cfg.UserAgent)
		resp, derr := c.http.Do(req)
		var body []byte
		if derr == nil {
			body, derr = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if derr == nil {
				switch {
				case resp.StatusCode >= 200 && resp.StatusCode < 300:
					cancel()
					if out == nil {
						return nil
					}
					if err := json.Unmarshal(body, out); err != nil {
						return fmt.Errorf("解析 %s 返回失败: %w（原始内容: %s）", u, err, snippet(body))
					}
					return nil
				case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
					cancel()
					return &HTTPError{Code: resp.StatusCode, URL: u, Detail: snippet(body)}
				default:
					derr = &HTTPError{Code: resp.StatusCode, URL: u, Detail: snippet(body)}
				}
			}
		}
		cancel()
		lastErr = derr
		if attempt < c.cfg.Retries {
			if c.log != nil {
				c.log.Warn("请求失败，%.0fs 后重试（%d/%d）: %v",
					c.backoff(attempt).Seconds(), attempt+1, c.cfg.Retries, lastErr)
			}
			if d := c.backoff(attempt); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return fmt.Errorf("POST %s 失败: %w", u, lastErr)
}

// Commits 查询本地提交之后的待更新提交。
func (c *Client) Commits(ctx context.Context, localHash string) (*CommitsResponse, error) {
	site, err := c.SelectSite(ctx)
	if err != nil {
		return nil, err
	}
	var out CommitsResponse
	payload := map[string]string{"commit_hash": localHash}
	if err := c.postJSON(ctx, site+"commits", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AssetIndex 取远端资源索引正文。
func (c *Client) AssetIndex(ctx context.Context, indexFile string) ([]byte, error) {
	site, err := c.SelectSite(ctx)
	if err != nil {
		return nil, err
	}
	return c.get(ctx, site+"file?file="+url.QueryEscape(indexFile))
}

// Bundle 申请把若干文件打成一个 zip，返回打包信息。
// 服务端不支持（老版本镜像）时返回错误，调用方回退逐文件下载。
func (c *Client) Bundle(ctx context.Context, kind string, files []string) (*BundleInfo, error) {
	site, err := c.SelectSite(ctx)
	if err != nil {
		return nil, err
	}
	var out BundleInfo
	payload := map[string][]string{"files": files}
	if err := c.postJSON(ctx, site+"bundle/"+kind, payload, &out); err != nil {
		return nil, err
	}
	if out.BundleID == "" {
		return nil, fmt.Errorf("服务端未返回 bundle_id")
	}
	return &out, nil
}

// BundleURL 生成打包下载地址。
func (c *Client) BundleURL(kind, id string) string {
	return c.site + "bundle/" + kind + "?id=" + url.QueryEscape(id)
}

// FileURL 生成单文件下载地址（isAsset=true 走 _assets 静态服务）。
func (c *Client) FileURL(relPath string, isAsset bool) string {
	if isAsset {
		return c.site + "_assets/" + escapePath(relPath)
	}
	return c.site + "file?file=" + url.QueryEscape(relPath)
}

// escapePath 按段转义路径，保留斜杠（_assets 静态服务需要原始目录结构）。
func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
