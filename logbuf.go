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

// LogKind 是日志通道。
//
// 分成两条通道，是因为两边的读者和排查时机完全不同：
//   - 同步日志回答「我复制的东西为什么没过去」——这是本程序的主业；
//   - 程序日志回答「程序自己怎么了」——配置、界面、托盘、开机自启。
//
// 混在一个列表里，程序日志会把同步日志冲淡，而用户打开「运行状态」页
// 十有八九是在看同步出了什么问题。
type LogKind string

const (
	// LogSync 同步日志：剪贴板内容的推送与拉取。
	LogSync LogKind = "sync"
	// LogApp 程序日志：配置、界面、托盘、开机自启等。
	LogApp LogKind = "app"
)

// logKinds 是全部通道。顺序即界面上两块面板的顺序：同步在前。
var logKinds = []LogKind{LogSync, LogApp}

// logFileName 拼出某条通道某一天的日志文件名。
//
// 形如 xime-clip-sync-2026-10-12.sync.log：
//   - 前缀 xime-clip-sync- 是「这是本程序的文件」的标记，清理时只认它；
//   - 中间是日期，按天轮转；
//   - 通道段放在日期之后，这样同一天的两份日志在文件管理器里挨着排。
func logFileName(kind LogKind, date string) string {
	return logFilePrefix + date + "." + string(kind) + logFileSuffix
}

// logFileInfo 是从日志文件名解析出来的信息。
type logFileInfo struct {
	Kind LogKind
	Date string
}

// parseLogFileName 解析本程序生成的日志文件名。
//
// 现行命名：xime-clip-sync-YYYY-MM-DD.<sync|app>.log
// 旧命名（v1.5 之前）：xime-clip-sync-YYYY-MM-DD.log——那时只有一条流，
// 一律按程序日志对待，这样升级后遗留的过期文件仍然会被清理掉。
//
// 名字不符合规则时返回 false——这类文件一律不碰，
// 因为 logs/ 就在 exe 旁边，用户完全可能把自己的东西丢进去。
func parseLogFileName(name string) (logFileInfo, bool) {
	if !strings.HasSuffix(name, logFileSuffix) {
		return logFileInfo{}, false
	}
	body := name[:len(name)-len(logFileSuffix)]
	if !strings.HasPrefix(body, logFilePrefix) {
		return logFileInfo{}, false
	}
	body = body[len(logFilePrefix):]

	if i := strings.LastIndex(body, "."); i >= 0 {
		kind := LogKind(body[i+1:])
		if kind != LogSync && kind != LogApp {
			return logFileInfo{}, false
		}
		return logFileInfo{Kind: kind, Date: body[:i]}, true
	}
	return logFileInfo{Kind: LogApp, Date: body}, true
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
	// 注意这是单文件上限：两条通道各有各的文件，目录总占用大致是
	// MaxFileBytes × RetainDays × 2。
	MaxFileBytes int64
}

// logSink 是一条通道的落盘状态：各自一个文件句柄、各自按天轮转、各自封顶。
type logSink struct {
	file   *os.File
	date   string // 当前文件对应的日期
	size   int64
	capped bool // 当天文件已达上限
}

// LogBuffer 是定长的内存日志环形缓冲，供配置界面实时查看运行情况，
// 可选地同时落盘。
//
// 两条通道在内存与磁盘上都是分开的：
//   - 内存：各自一个定长环，互不挤占——程序日志再多也顶不掉同步日志；
//   - 磁盘：各自一份文件句柄，按天轮转、按上限封顶，互不影响。
//
// 落盘策略：logs/xime-clip-sync-YYYY-MM-DD.<kind>.log，
// 跨天自动切换，并按保留天数定期清理旧文件。
// 这样做的原因：无控制台的桌面版出问题时日志是唯一线索，
// 但一个不断增长的单文件既不好查也不好清。
type LogBuffer struct {
	mu    sync.Mutex
	lines map[LogKind][]LogLine
	max   int
	subs  map[chan LogLine]struct{}

	// 落盘状态。两条通道共用目录、保留期与单文件上限，各自持有文件句柄。
	dir          string // 空表示不落盘
	retainDays   int
	maxFileBytes int64 // 单个文件上限；<=0 表示不限制
	sinks        map[LogKind]*logSink
}

// LogLine 是一条日志。
type LogLine struct {
	Time  string  `json:"time"`
	Kind  LogKind `json:"kind"`  // sync | app
	Level string  `json:"level"` // info | warn | error
	Text  string  `json:"text"`
}

func NewLogBuffer(max int) *LogBuffer {
	return &LogBuffer{
		max:   max,
		lines: map[LogKind][]LogLine{},
		subs:  map[chan LogLine]struct{}{},
		sinks: map[LogKind]*logSink{},
	}
}

// SetLogFile 应用日志落盘策略：换目录、开关、保留天数与单文件上限都会立即生效。
// 设置后会立即打开两条通道当天的文件，并清理一次过期日志。
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
	for _, kind := range logKinds {
		if _, err := b.openForDateLocked(kind, now); err != nil {
			// 一条打不开就整体退回不落盘。只留一条能查的日志比两条都没有更糟：
			// 用户会以为「另一条通道什么也没发生」，而不是「它根本没写下来」。
			b.closeLocked()
			b.dir = ""
			return err
		}
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

// CurrentFile 返回某条通道当前正在写入的日志文件路径（未落盘时为空）。
func (b *LogBuffer) CurrentFile(kind LogKind) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sinks[kind]
	if b.dir == "" || s == nil || s.date == "" {
		return ""
	}
	return filepath.Join(b.dir, logFileName(kind, s.date))
}

// ---------------------------------------------------------------- 内部

// closeLocked 关闭全部通道的当前文件（调用方需持有锁）。
func (b *LogBuffer) closeLocked() {
	for _, s := range b.sinks {
		if s.file != nil {
			s.file.Close()
		}
	}
	b.sinks = map[LogKind]*logSink{}
}

// openForDateLocked 确保某条通道的当前文件对应 now 所在的日期，
// 返回是否发生了跨天轮转（调用方需持有锁）。
func (b *LogBuffer) openForDateLocked(kind LogKind, now time.Time) (bool, error) {
	if b.dir == "" {
		return false, nil
	}
	date := now.Format(logDateLayout)

	rotated := false
	if s := b.sinks[kind]; s != nil && s.file != nil {
		if s.date == date {
			return false, nil
		}
		s.file.Close()
		rotated = true
	}

	path := filepath.Join(b.dir, logFileName(kind, date))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		delete(b.sinks, kind)
		return rotated, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		delete(b.sinks, kind)
		return rotated, err
	}
	b.sinks[kind] = &logSink{
		file:   f,
		date:   date,
		size:   info.Size(),
		capped: b.maxFileBytes > 0 && info.Size() >= b.maxFileBytes,
	}
	return rotated, nil
}

// cleanupLocked 删除超过保留期的日志文件，返回删除数量与保留天数。
//
// 只处理文件名符合本程序命名规则的文件（见 parseLogFileName），
// 目录里其他东西一律不碰。两条通道的文件按同一个保留期一起清理。
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
		info, ok := parseLogFileName(name)
		if !ok {
			continue
		}
		d, err := time.ParseInLocation(logDateLayout, info.Date, now.Location())
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

// logf 追加一条日志：写入对应通道的内存环、推送订阅者、按需落盘。
func (b *LogBuffer) logf(kind LogKind, level, format string, args ...any) {
	now := time.Now()
	line := LogLine{
		Time:  now.Format("15:04:05"),
		Kind:  kind,
		Level: level,
		Text:  fmt.Sprintf(format, args...),
	}

	b.mu.Lock()
	ring := append(b.lines[kind], line)
	if len(ring) > b.max {
		ring = ring[len(ring)-b.max:]
	}
	b.lines[kind] = ring
	b.writeFileLocked(kind, now, line)
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

// writeFileLocked 把日志写入对应通道当天的文件（调用方需持有锁）。
//
// 任何写失败都静默放弃：日志只是辅助，绝不能因为它把同步循环搞挂。
func (b *LogBuffer) writeFileLocked(kind LogKind, now time.Time, line LogLine) {
	if b.dir == "" {
		return
	}
	rotated, err := b.openForDateLocked(kind, now)
	if err != nil {
		return
	}
	if rotated {
		// 刚跨天，顺手清一次过期日志
		b.cleanupLocked(now)
	}
	s := b.sinks[kind]
	if s == nil || s.file == nil || s.capped {
		return
	}

	entry := fmt.Sprintf("%s %-5s %s\n", now.Format("2006-01-02 15:04:05"), line.Level, line.Text)
	n, err := s.file.WriteString(entry)
	if err != nil {
		return
	}
	s.size += int64(n)
	if b.maxFileBytes > 0 && s.size >= b.maxFileBytes {
		s.capped = true
		fmt.Fprintf(s.file, "%s %-5s 本日日志已达 %s 上限，后续日志只保留在内存中\n",
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

// Infof / Warnf / Errorf 走**程序**通道。
//
// 默认给程序通道、同步那边显式写 Sync*，是因为「程序日志」是更中性的默认值：
// main / ui / uiserver / autostart 这些调用点不必改动，读代码时看到 SyncInfof
// 就知道这条一定与剪贴板同步有关。
func (b *LogBuffer) Infof(format string, args ...any)  { b.logf(LogApp, "info", format, args...) }
func (b *LogBuffer) Warnf(format string, args ...any)  { b.logf(LogApp, "warn", format, args...) }
func (b *LogBuffer) Errorf(format string, args ...any) { b.logf(LogApp, "error", format, args...) }

// SyncInfof / SyncWarnf / SyncErrorf 走**同步**通道（剪贴板的推与拉）。
func (b *LogBuffer) SyncInfof(format string, args ...any)  { b.logf(LogSync, "info", format, args...) }
func (b *LogBuffer) SyncWarnf(format string, args ...any)  { b.logf(LogSync, "warn", format, args...) }
func (b *LogBuffer) SyncErrorf(format string, args ...any) { b.logf(LogSync, "error", format, args...) }

// Lines 返回某条通道最近 n 条日志（n<=0 表示全部）。
func (b *LogBuffer) Lines(kind LogKind, n int) []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := b.lines[kind]
	if n <= 0 || n > len(ring) {
		n = len(ring)
	}
	out := make([]LogLine, n)
	copy(out, ring[len(ring)-n:])
	return out
}

// LinesByKind 返回每条通道各自的最近 n 条日志，供配置界面一次取全。
func (b *LogBuffer) LinesByKind(n int) map[LogKind][]LogLine {
	out := make(map[LogKind][]LogLine, len(logKinds))
	for _, k := range logKinds {
		out[k] = b.Lines(k, n)
	}
	return out
}

// Subscribe 订阅日志流，返回通道与取消函数。
//
// 订阅拿到的是**两条通道混合**的流：每条日志自己带 Kind，
// 由订阅方（配置界面）决定放进哪块面板。
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
