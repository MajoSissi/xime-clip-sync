package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxLogFileBytes 单个日志文件上限的默认值（可在界面上改，0 表示不限制）。
	defaultMaxLogFileBytes = 2 << 20 // 2 MiB

	logFilePrefix = "xime-clip-sync-"
	logFileSuffix = ".log"
	logDateLayout = "2006-01-02"
)

// logFileDate 从日志文件名里取出日期部分。
//
// 名字不符合本程序的命名规则（前缀 + 日期 + .log）时返回 false——
// 这类文件一律不碰，因为可能是用户自己放进 logs/ 的东西。
func logFileDate(name string) (string, bool) {
	if !strings.HasSuffix(name, logFileSuffix) {
		return "", false
	}
	body := name[:len(name)-len(logFileSuffix)]
	if !strings.HasPrefix(body, logFilePrefix) {
		return "", false
	}
	return body[len(logFilePrefix):], true
}

// LogFileOptions 描述日志落盘策略。
//
// 零值（LogFileOptions{}）表示完全不落盘。
type LogFileOptions struct {
	// Dir 是日志目录；空字符串表示不落盘。
	Dir string
	// Enabled 为 false 时同样不落盘（对应配置里的「日志写入文件」开关）。
	Enabled bool
	// RetainDays 是保留天数（含今天）；<=0 表示永久保留、不自动清理。
	RetainDays int
	// MaxFileBytes 是**单个**日志文件的大小上限；<=0 表示不限制。
	//
	// 达到上限后当天不再写文件（内存日志不受影响），避免某天异常刷屏把磁盘写满。
	// 注意这是单文件上限：目录总占用大致是 MaxFileBytes × RetainDays。
	MaxFileBytes int64
}

// LogBuffer 是定长的内存日志环形缓冲，供配置界面实时查看运行情况，
// 可选地同时落盘。
//
// 落盘策略：**按天分文件**（logs/xime-clip-sync-YYYY-MM-DD.log），
// 跨天自动切换，并按保留天数定期清理旧文件。
// 这样做的原因：无控制台的桌面版出问题时日志是唯一线索，
// 但一个不断增长的单文件既不好查也不好清。
type LogBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	max   int
	subs  map[chan LogLine]struct{}

	// 落盘状态
	dir          string // 空表示不落盘
	retainDays   int
	maxFileBytes int64 // 单个文件上限；<=0 表示不限制
	file         *os.File
	fileDate     string // 当前文件对应的日期
	fileSize     int64
	capped       bool // 当天文件已达上限
}

// LogLine 是一条日志。
type LogLine struct {
	Time  string `json:"time"`
	Level string `json:"level"` // info | warn | error
	Text  string `json:"text"`
}

func NewLogBuffer(max int) *LogBuffer {
	return &LogBuffer{max: max, subs: map[chan LogLine]struct{}{}}
}

// SetLogFile 应用日志落盘策略：换目录、开关、保留天数与单文件上限都会立即生效。
// 设置后会立即打开当天文件并清理一次过期日志。
func (b *LogBuffer) SetLogFile(o LogFileOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closeLocked()
	b.dir = ""
	b.retainDays = o.RetainDays
	b.maxFileBytes = o.MaxFileBytes

	if !o.Enabled || o.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return err
	}
	b.dir = o.Dir
	now := time.Now()
	if _, err := b.openForDateLocked(now); err != nil {
		b.dir = ""
		return err
	}
	b.cleanupLocked(now)
	return nil
}

// Cleanup 立即按保留期清理过期日志文件（启动时与定期各调一次）。
func (b *LogBuffer) Cleanup() {
	b.mu.Lock()
	removed, retain := b.cleanupLocked(time.Now())
	b.mu.Unlock()
	if removed > 0 {
		b.Infof("已清理 %d 个过期日志文件（保留最近 %d 天）", removed, retain)
	}
}

// CurrentFile 返回当前正在写入的日志文件路径（未落盘时为空）。
func (b *LogBuffer) CurrentFile() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dir == "" || b.fileDate == "" {
		return ""
	}
	return filepath.Join(b.dir, logFilePrefix+b.fileDate+logFileSuffix)
}

// ---------------------------------------------------------------- 内部

// closeLocked 关闭当前文件（调用方需持有锁）。
func (b *LogBuffer) closeLocked() {
	if b.file != nil {
		b.file.Close()
	}
	b.file = nil
	b.fileDate = ""
	b.fileSize = 0
	b.capped = false
}

// openForDateLocked 确保当前文件对应 now 所在的日期，
// 返回是否发生了跨天轮转（调用方需持有锁）。
func (b *LogBuffer) openForDateLocked(now time.Time) (bool, error) {
	if b.dir == "" {
		return false, nil
	}
	date := now.Format(logDateLayout)
	if b.file != nil && b.fileDate == date {
		return false, nil
	}
	rotated := b.file != nil
	b.closeLocked()

	path := filepath.Join(b.dir, logFilePrefix+date+logFileSuffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return rotated, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return rotated, err
	}
	b.file = f
	b.fileDate = date
	b.fileSize = info.Size()
	b.capped = b.maxFileBytes > 0 && b.fileSize >= b.maxFileBytes
	return rotated, nil
}

// cleanupLocked 删除超过保留期的日志文件，返回删除数量与保留天数。
//
// 只处理文件名符合「logFilePrefix + YYYY-MM-DD + .log」的文件
// （见 logFileDate），目录里其他东西一律不碰。
func (b *LogBuffer) cleanupLocked(now time.Time) (int, int) {
	if b.dir == "" || b.retainDays <= 0 {
		return 0, b.retainDays
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return 0, b.retainDays
	}

	// 保留「今天 + 之前 retainDays-1 天」
	cut := now.AddDate(0, 0, -(b.retainDays - 1))
	cutoff := time.Date(cut.Year(), cut.Month(), cut.Day(), 0, 0, 0, 0, now.Location())

	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		dateStr, ok := logFileDate(name)
		if !ok {
			continue
		}
		d, err := time.ParseInLocation(logDateLayout, dateStr, now.Location())
		if err != nil {
			continue // 名字对不上日期格式，不是本程序生成的，不碰
		}
		if d.Before(cutoff) {
			if err := os.Remove(filepath.Join(b.dir, name)); err == nil {
				removed++
			}
		}
	}
	return removed, b.retainDays
}

// logf 追加一条日志：写入内存环形缓冲、推送订阅者、按需落盘。
func (b *LogBuffer) logf(level, format string, args ...any) {
	now := time.Now()
	line := LogLine{
		Time:  now.Format("15:04:05"),
		Level: level,
		Text:  fmt.Sprintf(format, args...),
	}

	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	b.writeFileLocked(now, line)
	subs := make([]chan LogLine, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- line:
		default: // 订阅者消费不过来就丢弃，避免阻塞同步循环
		}
	}
}

// writeFileLocked 把日志写入当天文件（调用方需持有锁）。
//
// 任何写失败都静默放弃：日志只是辅助，绝不能因为它把同步循环搞挂。
func (b *LogBuffer) writeFileLocked(now time.Time, line LogLine) {
	if b.dir == "" {
		return
	}
	rotated, err := b.openForDateLocked(now)
	if err != nil {
		return
	}
	if rotated {
		// 刚跨天，顺手清一次过期日志
		b.cleanupLocked(now)
	}
	if b.capped {
		return
	}

	entry := fmt.Sprintf("%s %-5s %s\n", now.Format("2006-01-02 15:04:05"), line.Level, line.Text)
	n, err := b.file.WriteString(entry)
	if err != nil {
		return
	}
	b.fileSize += int64(n)
	if b.maxFileBytes > 0 && b.fileSize >= b.maxFileBytes {
		b.capped = true
		fmt.Fprintf(b.file, "%s %-5s 本日日志已达 %s 上限，后续日志只保留在内存中\n",
			now.Format("2006-01-02 15:04:05"), "warn", humanBytes(b.maxFileBytes))
	}
}

// humanBytes 把字节数格式化成便于阅读的写法（只用于日志文本）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}

func (b *LogBuffer) Infof(format string, args ...any)  { b.logf("info", format, args...) }
func (b *LogBuffer) Warnf(format string, args ...any)  { b.logf("warn", format, args...) }
func (b *LogBuffer) Errorf(format string, args ...any) { b.logf("error", format, args...) }

// Lines 返回最近 n 条日志（n<=0 表示全部）。
func (b *LogBuffer) Lines(n int) []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]LogLine, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// Subscribe 订阅日志流，返回通道与取消函数。
func (b *LogBuffer) Subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}
}

// preview 生成用于界面展示的文本摘要（截断 + 转义换行）。
func preview(s string, limit int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return s
}
