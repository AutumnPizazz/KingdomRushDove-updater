package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"krd-updater/internal/ui"
)

// Download 把 url 下载到 dest，支持断点续传（.part）、重试与进度显示。
// expected>0 时会校验最终大小。
func (c *Client) Download(ctx context.Context, url, dest string, expected int64, prog *ui.Progress) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// 已经是完整文件就跳过（重复执行时的快路径）
	if expected > 0 {
		if st, err := os.Stat(dest); err == nil && st.Size() == expected {
			if prog != nil {
				prog.Add(expected)
			}
			return nil
		}
	}

	part := dest + ".part"
	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		var offset int64
		if st, err := os.Stat(part); err == nil {
			offset = st.Size()
			// 已经下完但没改名（上次在 rename 前退出）
			if expected > 0 && offset == expected {
				return os.Rename(part, dest)
			}
			if expected > 0 && offset > expected {
				_ = os.Remove(part)
				offset = 0
			}
		}
		err := c.downloadOnce(ctx, url, part, offset, expected, prog)
		if err == nil {
			if expected > 0 {
				if st, serr := os.Stat(part); serr == nil && st.Size() != expected {
					err = fmt.Errorf("大小不符：期望 %d 字节，实际 %d 字节", expected, st.Size())
				}
			}
			if err == nil {
				return os.Rename(part, dest)
			}
		}
		lastErr = err
		if attempt < c.cfg.Retries {
			if c.log != nil {
				c.log.Warn("下载失败，%.0fs 后继续（%d/%d）: %v",
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
	return fmt.Errorf("下载 %s 失败: %w", url, lastErr)
}

func (c *Client) downloadOnce(ctx context.Context, url, part string, offset, expected int64, prog *ui.Progress) error {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout()+time.Duration(expected/(1<<20))*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 { // 服务端不支持 Range，重头来
			if prog != nil {
				prog.Add(-offset)
			}
			offset = 0
		}
	case http.StatusPartialContent:
		if offset == 0 {
			return fmt.Errorf("服务端返回 206 但未请求续传")
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// .part 比远端还大，删掉重来
		_ = os.Remove(part)
		return fmt.Errorf("续传区间无效，已重置")
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPError{Code: resp.StatusCode, URL: url, Detail: strings.TrimSpace(string(body))}
	}

	if expected <= 0 && resp.ContentLength > 0 {
		expected = offset + resp.ContentLength
		if prog != nil {
			prog.SetTotal(expected)
		}
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if offset > 0 && prog != nil {
		prog.Add(offset) // 续传：把已存在的 .part 大小补进进度，避免从 0% 开始跳
	}

	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			if prog != nil {
				prog.Add(int64(n))
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// FetchTo 是「取一个小文件到本地路径」的便捷封装（走 Download，无进度）。
func (c *Client) FetchTo(ctx context.Context, url, dest string) error {
	return c.Download(ctx, url, dest, 0, nil)
}
