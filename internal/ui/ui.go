// Package ui 提供日志、进度条与人类可读的数字格式。
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Logger 是带级别的终端日志，可选同时写入文件。
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	file    *os.File
	verbose bool
	json    bool
}

// NewLogger 创建日志器；jsonMode 下所有人类日志走 stderr，stdout 留给 JSON 结果。
func NewLogger(verbose bool, logFile string, jsonMode bool) (*Logger, error) {
	out := io.Writer(os.Stdout)
	if jsonMode {
		out = os.Stderr
	}
	return NewLoggerTo(out, verbose, logFile, jsonMode)
}

// NewLoggerTo 同上，但把人类可读日志写到 w（GUI 界面用来收集日志）。
func NewLoggerTo(w io.Writer, verbose bool, logFile string, jsonMode bool) (*Logger, error) {
	l := &Logger{out: w, verbose: verbose, json: jsonMode}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("打开日志文件失败: %w", err)
		}
		l.file = f
	}
	return l, nil
}

// Close 关闭日志文件。
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// Stdout 返回日志使用的输出流（JSON 模式下是 stderr）。
func (l *Logger) Stdout() io.Writer { return l.out }

// JSONMode 表示结果要按 JSON 输出（此时人类日志已改走 stderr）。
func (l *Logger) JSONMode() bool { return l.json }

// ProgressEnabled 依据 progress 配置与终端情况决定要不要画进度条。
func ProgressEnabled(mode string, jsonMode bool) bool {
	switch mode {
	case "none":
		return false
	case "plain":
		return true
	case "auto":
		if jsonMode {
			return false
		}
		return IsTerminal(os.Stdout)
	default:
		return false
	}
}

func (l *Logger) writeLine(prefix, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	line := prefix + msg + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = io.WriteString(l.out, line)
	}
	if l.file != nil {
		_, _ = io.WriteString(l.file, time.Now().Format("2006-01-02 15:04:05 ")+line)
	}
}

// Info 普通信息。
func (l *Logger) Info(format string, a ...any) { l.writeLine("", format, a...) }

// Warn 警告（不中断）。
func (l *Logger) Warn(format string, a ...any) { l.writeLine("[警告] ", format, a...) }

// Error 错误。
func (l *Logger) Error(format string, a ...any) { l.writeLine("[错误] ", format, a...) }

// Debug 仅 --verbose 时输出。
func (l *Logger) Debug(format string, a ...any) {
	if l.verbose {
		l.writeLine("[调试] ", format, a...)
	}
}

// Step 打一个分组标题，便于阅读长流程。
func (l *Logger) Step(format string, a ...any) {
	l.writeLine("", "%s", "== "+fmt.Sprintf(format, a...))
}

// Progress 是单行刷新的进度显示，非终端环境下自动退化成稀疏的百分比行。
type Progress struct {
	mu       sync.Mutex
	out      io.Writer
	label    string
	total    int64
	done     int64
	start    time.Time
	last     time.Time
	lastPct  int
	enabled  bool
	finished bool
}

// NewProgress 创建进度条；total<=0 表示总量未知。
func NewProgress(out io.Writer, enabled bool, label string, total int64) *Progress {
	return &Progress{out: out, label: label, total: total, start: time.Now(), enabled: enabled}
}

// SetTotal 在拿到 Content-Length 后补上总量。
func (p *Progress) SetTotal(total int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if total > 0 {
		p.total = total
	}
}

// Add 累加已完成的字节数。
func (p *Progress) Add(n int64) {
	if p == nil || !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	p.renderLocked(false)
}

func (p *Progress) renderLocked(force bool) {
	now := time.Now()
	if !force && now.Sub(p.last) < 100*time.Millisecond {
		return
	}
	p.last = now
	elapsed := now.Sub(p.start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	speed := float64(p.done) / elapsed

	var pct int
	var head string
	switch {
	case p.total > 0:
		pct = int(float64(p.done) * 100 / float64(p.total))
		if pct > 100 {
			pct = 100
		}
		head = fmt.Sprintf("%5.1f%%  %s / %s", float64(pct), HumanBytes(p.done), HumanBytes(p.total))
	default:
		head = fmt.Sprintf("      %s", HumanBytes(p.done))
	}
	tail := fmt.Sprintf("  %s/s", HumanBytes(int64(speed)))
	if p.total > 0 && speed > 0 && p.done < p.total {
		eta := time.Duration(float64(p.total-p.done)/speed) * time.Second
		tail += "  剩余 " + HumanDuration(eta)
	}
	line := "\r" + padRight(p.label, 14) + " " + head + tail
	if !force {
		// 非终端只能按整数百分比打点，避免刷屏
		if p.total > 0 && pct == p.lastPct && pct != 100 {
			return
		}
		p.lastPct = pct
	}
	_, _ = io.WriteString(p.out, line+"\x1b[K")
}

// Finish 结束进度显示。
func (p *Progress) Finish() {
	if p == nil || !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	p.renderLocked(true)
	_, _ = io.WriteString(p.out, "\n")
}

func padRight(s string, n int) string {
	w := 0
	for range s {
		w++
	}
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// HumanBytes 把字节数格式化成 MB/MB 习惯的单位。
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}

// HumanDuration 输出 01:23 形式的时长。
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d > time.Hour {
		return fmt.Sprintf("%d小时%02d分", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// IsTerminal 判断是否是交互式终端（用于决定要不要画进度条）。
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeCharDevice != 0 {
		return true
	}
	// Windows 上管道/重定向也走这条；CONOUT$ 之类特性交给上层开关
	return false
}
