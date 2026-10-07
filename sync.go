package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// 503 限流后的退避时长。坚果云免费版每 30 分钟限 600 次请求，
	// 超限后继续请求会延长封禁窗口，所以必须停下来等。
	pullBackoff = 10 * time.Minute
	pushBackoff = 3 * time.Minute

	// 界面上「本地内容」和「远端内容」两块最多展示这么多字符，
	// 超出的截断并补省略号。150 是「够看清内容是什么、又不至于把卡片撑成一大块」的长度。
	previewLen = 150
	// 远端原始内容在界面上最多展示这么多字符（只在解析失败、退化成原文时才用到）
	rawPreviewLen = 400
)

// runeCount 返回字符数（不是字节数）。
// 用户看到的是「多少字」，所以长度上限也按字符算，而不是 UTF-8 字节。
func runeCount(s string) int { return len([]rune(s)) }

// Status 是暴露给配置界面的运行状态快照。
type Status struct {
	Configured   bool   `json:"configured"`
	Enabled      bool   `json:"enabled"`
	State        string `json:"state"` // stopped | ok | error | backoff | disabled
	Message      string `json:"message"`
	CurrentText  string `json:"currentText"`
	RemoteText   string `json:"remoteText"`
	RemoteRaw    string `json:"remoteRaw"`
	RemoteHash   string `json:"remoteHash"`
	LocalHash    string `json:"localHash"`
	HashMismatch bool   `json:"hashMismatch"`
	RemoteExists bool   `json:"remoteExists"`
	LastPushAt   string `json:"lastPushAt"`
	LastPullAt   string `json:"lastPullAt"`
	LastError    string `json:"lastError"`
	PushCount    int    `json:"pushCount"`
	PullCount    int    `json:"pullCount"`
	SkipCount    int    `json:"skipCount"`
	// OversizeCount 是因文本超过长度上限而被跳过推送的次数
	OversizeCount int    `json:"oversizeCount"`
	BackoffUntil  string `json:"backoffUntil"`
	DeviceName    string `json:"deviceName"`
	Autostart     bool   `json:"autostart"`
}

// SyncEngine 负责「本地剪贴板 ↔ 远端 current.json」的双向同步。
//
// 并发约定：
//   - syncMu 串行化整轮同步（含网络请求）
//   - mu 只保护状态字段，持有时不做网络 IO
type SyncEngine struct {
	syncMu sync.Mutex

	mu     sync.Mutex
	cfg    Config
	client *webdavClient
	log    *LogBuffer
	clip   Clipboard

	// 两端已达成一致的内容。这是回声抑制的关键：
	// 只要本地剪贴板等于 current，就不会再推送，从而避免
	// 「应用远端内容 → 触发本地变更 → 又推回远端」的死循环。
	current      string
	lastETag     string
	remoteExists bool
	remoteText   string
	remoteRaw    string
	remoteHash   string
	localHash    string
	hashMismatch bool
	hashWarned   bool

	localSeq  uint32
	seqPrimed bool
	hasSeq    bool

	backoffUntil time.Time
	lastPushAt   time.Time
	lastPullAt   time.Time
	lastError    string
	lastErrLog   string
	pushCount    int
	pullCount    int
	skipCount    int
	oversize     int
	// 上一次因超长被跳过的文本。只用于「同一条超长内容只告警一次」——
	// 超长内容**不写进 current**（理由见 checkLocal 里的说明）。
	oversizeText string

	wake chan struct{}
}

// NewSyncEngine 创建同步引擎。
func NewSyncEngine(cfg Config, clip Clipboard, log *LogBuffer) *SyncEngine {
	cfg.normalize()
	e := &SyncEngine{
		cfg:    cfg,
		clip:   clip,
		log:    log,
		client: newWebDAVClient(cfg, log),
		wake:   make(chan struct{}, 1),
	}
	if _, ok := clip.(seqClipboard); ok {
		e.hasSeq = true
	}
	return e
}

// ---------------------------------------------------------------- 配置

// UpdateConfig 应用新配置并重建客户端，随后立即触发一次同步。
func (e *SyncEngine) UpdateConfig(cfg Config) {
	cfg.normalize()
	e.mu.Lock()
	e.cfg = cfg
	e.client = newWebDAVClient(cfg, e.log)
	e.lastETag = ""              // 服务器/目录可能已变，ETag 缓存作废
	e.backoffUntil = time.Time{} // 配置变更往往就是在修正问题，解除退避
	e.mu.Unlock()
	// 记的是**实际生效**的远程路径：留空时会显示默认值，
	// 这样日志里的这一行能直接对上「远端文件」那一行。
	e.log.Infof("配置已更新：%s（远程路径 %q，轮询 %ds）", cfg.DavURL, cfg.RemotePathOrDefault(), cfg.PollSeconds)
	e.Wake()
}

// Config 返回当前配置副本。
func (e *SyncEngine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Wake 请求立即执行一次同步。
func (e *SyncEngine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------- 状态

func (e *SyncEngine) setError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	e.mu.Lock()
	e.lastError = msg
	shouldLog := msg != e.lastErrLog
	e.lastErrLog = msg
	e.mu.Unlock()
	if shouldLog {
		e.log.Errorf("%s", msg)
	}
}

func (e *SyncEngine) clearError() {
	e.mu.Lock()
	e.lastError = ""
	e.lastErrLog = ""
	e.mu.Unlock()
}

// getCurrent 返回「两端已达成一致的内容」。
func (e *SyncEngine) getCurrent() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// shortHash 截断哈希值，便于在界面上展示。
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// noteHash 对比远端 hash 与本地 SHA-256。
//
// 这只是兼容性诊断：同步判重用的是**文本内容**，不依赖 hash，
// 所以对端用自己的算法时同步照常工作，这里只记一条 warn。
// 本地写出去的 hash 固定是 SHA-256，没有开关可改。
func (e *SyncEngine) noteHash(prof Profile) {
	if prof.Text == "" {
		return
	}
	local := hashText(prof.Text)

	e.mu.Lock()
	e.localHash = local
	if prof.Hash == "" {
		e.remoteHash = ""
		e.hashMismatch = false
		e.hashWarned = false
		e.mu.Unlock()
		return
	}
	e.remoteHash = prof.Hash
	mismatch := !strings.EqualFold(prof.Hash, local)
	e.hashMismatch = mismatch
	first := mismatch && !e.hashWarned
	e.hashWarned = mismatch
	e.mu.Unlock()

	if first {
		e.log.Warnf("对端 hash 与本地 SHA-256 不一致（远端 %s / 本地 %s），不影响同步",
			shortHash(prof.Hash), shortHash(local))
	}
}

// backoff 进入限流退避。
func (e *SyncEngine) backoff(d time.Duration, action string) {
	until := time.Now().Add(d)
	e.mu.Lock()
	e.backoffUntil = until
	e.lastError = "服务器限流（HTTP 503），已暂停同步 " + d.String()
	e.mu.Unlock()
	e.log.Warnf("%s 遇到 503 限流，退避至 %s（坚果云免费版限 600 次/30 分钟）",
		action, until.Format("15:04:05"))
}

func (e *SyncEngine) inBackoff() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Before(e.backoffUntil)
}

// Status 返回状态快照。
func (e *SyncEngine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := Status{
		Configured:    e.cfg.DavURL != "",
		Enabled:       e.cfg.Enabled,
		CurrentText:   preview(e.current, previewLen),
		RemoteText:    preview(e.remoteText, previewLen),
		RemoteRaw:     e.remoteRaw,
		RemoteHash:    shortHash(e.remoteHash),
		LocalHash:     shortHash(e.localHash),
		HashMismatch:  e.hashMismatch,
		RemoteExists:  e.remoteExists,
		LastError:     e.lastError,
		PushCount:     e.pushCount,
		PullCount:     e.pullCount,
		SkipCount:     e.skipCount,
		OversizeCount: e.oversize,
		DeviceName:    e.cfg.DeviceName,
		Autostart:     autostartEnabled(),
	}
	if !e.lastPushAt.IsZero() {
		st.LastPushAt = e.lastPushAt.Format("15:04:05")
	}
	if !e.lastPullAt.IsZero() {
		st.LastPullAt = e.lastPullAt.Format("15:04:05")
	}

	switch {
	case !e.cfg.Enabled:
		st.State, st.Message = "disabled", "同步已关闭"
	case !st.Configured:
		st.State, st.Message = "stopped", "尚未配置 WebDAV 服务器"
	case time.Now().Before(e.backoffUntil):
		st.State, st.Message = "backoff", "服务器限流，退避至 "+e.backoffUntil.Format("15:04:05")
		st.BackoffUntil = e.backoffUntil.Format("15:04:05")
	case e.lastError != "":
		st.State, st.Message = "error", e.lastError
	default:
		st.State, st.Message = "ok", "同步中"
	}
	return st
}

// ---------------------------------------------------------------- 主循环

// Start 启动同步循环，阻塞直到 ctx 结束。
func (e *SyncEngine) Start(ctx context.Context) {
	e.prime()

	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	// prime() 已经完整跑过一轮（本地检查 + 远端拉取），
	// 所以下一次触发要排到各自间隔之后。若按 time.Now() 初始化，
	// 启动后 200ms 就会立刻再打一次远端请求，白白多一次往返。
	// 这两个变量是循环局部的，不需要 e.mu（interval 取值内部自己加锁）。
	nextLocal := time.Now().Add(e.localInterval())
	nextRemote := time.Now().Add(e.pollInterval())

	for {
		select {
		case <-ctx.Done():
			e.log.Infof("同步已停止")
			return
		case <-e.wake:
			e.syncCycle(true)
			now := time.Now()
			nextLocal = now.Add(e.localInterval())
			nextRemote = now.Add(e.pollInterval())
		case <-tick.C:
			now := time.Now()
			if now.After(nextLocal) {
				nextLocal = now.Add(e.localInterval())
				e.checkLocalOnly()
			}
			if now.After(nextRemote) {
				nextRemote = now.Add(e.pollInterval())
				e.syncCycle(false)
			}
		}
	}
}

func (e *SyncEngine) pollInterval() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Duration(e.cfg.PollSeconds) * time.Second
}

func (e *SyncEngine) localInterval() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Duration(e.cfg.LocalPollSeconds) * time.Second
}

// prime 建立启动基线：以本地剪贴板为 current，
// 再拉一次远端——远端有内容则远端优先，远端不存在则用本地播种。
func (e *SyncEngine) prime() {
	cfg := e.Config()
	if text, err := e.clip.GetText(); err == nil {
		// 超长的剪贴板内容同样不当作基线（理由见 checkLocal）：
		// 它不会被推送，就不该出现在界面的「本地内容」里。
		if n := runeCount(text); cfg.MaxTextChars > 0 && n > cfg.MaxTextChars {
			e.mu.Lock()
			e.oversizeText = text
			e.oversize++
			e.mu.Unlock()
			e.log.Warnf("本地 %d 字符超自动同步上限 %d，未推送", n, cfg.MaxTextChars)
		} else {
			e.mu.Lock()
			e.current = text
			e.mu.Unlock()
			if text != "" {
				e.log.Infof("启动基线：本地 %d 字符", runeCount(text))
			}
		}
	} else {
		e.log.Warnf("读取剪贴板失败：%v", err)
	}
	if sc, ok := e.clip.(seqClipboard); ok {
		if seq, ok2 := sc.Seq(); ok2 {
			e.mu.Lock()
			e.localSeq, e.seqPrimed = seq, true
			e.mu.Unlock()
		}
	}
	e.log.Infof("开始同步：%s", e.describeRemote())
	e.syncCycle(true)
}

func (e *SyncEngine) describeRemote() string {
	cfg := e.Config()
	u, ok := cfg.FileURL()
	if !ok {
		return "未配置 WebDAV 服务器"
	}
	return u
}

// syncCycle 执行一轮完整同步（本地 → 远端，再远端 → 本地）。
func (e *SyncEngine) syncCycle(force bool) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	cfg := e.Config()
	if !cfg.Enabled || cfg.DavURL == "" {
		return
	}
	if !force && e.inBackoff() {
		e.mu.Lock()
		e.skipCount++
		e.mu.Unlock()
		return
	}
	e.checkLocal(cfg)
	e.pullRemote(cfg, force)
}

// checkLocalOnly 只检查本地剪贴板变化（远端由独立节奏拉取）。
func (e *SyncEngine) checkLocalOnly() {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	cfg := e.Config()
	if !cfg.Enabled || cfg.DavURL == "" || e.inBackoff() {
		return
	}
	e.checkLocal(cfg)
}

// checkLocal 检测本地剪贴板变化并推送。
func (e *SyncEngine) checkLocal(cfg Config) {
	local, err := e.readLocalIfChanged()
	if err != nil {
		e.setError(err)
		return
	}
	if local == nil {
		return
	}
	if *local == e.getCurrent() {
		return
	}
	if n := runeCount(*local); cfg.MaxTextChars > 0 && n > cfg.MaxTextChars {
		// 超长内容**不写进 current**。current 的语义是「本地与远端一致的那份内容」，
		// 界面上「本地内容」直接显示它，所以把它换成一份从未推送成功的内容会同时造成：
		//   ① 界面上显示出一段其实没同步过去的内容；
		//   ② 下一轮远端轮询发现 remote != current，会把远端内容写回剪贴板，
		//      把用户刚复制的这段长文本冲掉。
		// 「同一条只告警一次」由下面的 oversizeText 兜底
		// （正常情况下剪贴板序号没变就不会重复读到）。
		e.mu.Lock()
		first := e.oversizeText != *local
		e.oversizeText = *local
		e.oversize++ // 让界面能告诉用户「有多少次因为太长被跳过了」
		e.mu.Unlock()
		if first {
			e.log.Warnf("本地 %d 字符超自动同步上限 %d，未推送", n, cfg.MaxTextChars)
		}
		return
	}
	e.pushRemote(cfg, *local)
}

// readLocalIfChanged 读取本地剪贴板文本。
// 返回 nil 表示「自上次读取以来未变化」（Windows 下借助剪贴板序号快速跳过）。
func (e *SyncEngine) readLocalIfChanged() (*string, error) {
	if e.hasSeq {
		if sc, ok := e.clip.(seqClipboard); ok {
			if seq, ok2 := sc.Seq(); ok2 {
				e.mu.Lock()
				unchanged := e.seqPrimed && seq == e.localSeq
				e.localSeq, e.seqPrimed = seq, true
				e.mu.Unlock()
				if unchanged {
					return nil, nil
				}
			}
		}
	}
	text, err := e.clip.GetText()
	if err != nil {
		return nil, err
	}
	return &text, nil
}

// pushRemote 把文本推送到远端。
func (e *SyncEngine) pushRemote(cfg Config, text string) bool {
	payload, err := newTextProfile(text, cfg.DeviceName).encode()
	if err != nil {
		e.setError(err)
		return false
	}
	hdr, err := e.client.put(payload)

	e.mu.Lock()
	e.lastPushAt = time.Now()
	e.mu.Unlock()

	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			e.backoff(pushBackoff, "推送")
			return false
		}
		e.setError(err)
		return false
	}

	e.mu.Lock()
	if et := cacheETag(hdr); et != "" {
		e.lastETag = et
	}
	e.current = text
	e.remoteExists = true
	e.remoteText = text
	e.pushCount++
	e.lastError, e.lastErrLog = "", ""
	e.mu.Unlock()

	e.log.Infof("⬆ 推送 （%d 字符）", runeCount(text))
	return true
}

// pullRemote 拉取远端内容，必要时写入本地剪贴板。
func (e *SyncEngine) pullRemote(cfg Config, force bool) {
	e.mu.Lock()
	etag := ""
	if !force {
		etag = e.lastETag
	}
	e.mu.Unlock()

	body, hdr, status, err := e.client.get(etag)

	e.mu.Lock()
	e.lastPullAt = time.Now()
	e.mu.Unlock()

	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			e.backoff(pullBackoff, "拉取")
			return
		}
		e.setError(err)
		return
	}

	switch status {
	case 304:
		e.clearError()
		return
	case 404:
		// 远端尚无文件：首次使用时用本地内容播种
		e.mu.Lock()
		wasKnown := e.remoteExists
		e.remoteExists = false
		current := e.current
		e.mu.Unlock()
		e.clearError()
		if !wasKnown && current != "" {
			e.log.Infof("远端无文件，用本地初始化")
			e.pushRemote(cfg, current)
		}
		return
	}

	if len(body) == 0 {
		e.clearError()
		return
	}

	// 保留远端原始内容，便于排查与手机端的协议兼容问题。
	// 渲染成固定字段顺序的清单（见 remoteRawText），而不是直接把 JSON 原文丢上去——
	// 原文里 text 可能很长，而且字段顺序随对端实现而变，看不出哪个字段是什么。
	prof, plain, ok := decodeProfile(body)
	e.mu.Lock()
	if ok {
		e.remoteRaw = remoteRawText(prof)
	} else {
		// 解析不出来时退回原文，至少让用户看到服务器到底返回了什么
		e.remoteRaw = preview(string(body), rawPreviewLen)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	if plain {
		e.log.Warnf("远端为纯文本（旧格式），已兼容")
	}
	e.noteHash(prof)

	e.mu.Lock()
	if et := cacheETag(hdr); et != "" {
		e.lastETag = et
	}
	e.remoteExists = true
	e.remoteText = prof.Text
	current := e.current
	e.mu.Unlock()

	if prof.Text == current {
		e.clearError()
		return
	}

	if err := e.clip.SetText(prof.Text); err != nil {
		e.setError(err)
		return
	}
	e.mu.Lock()
	e.current = prof.Text
	e.pullCount++
	e.lastError, e.lastErrLog = "", ""
	// 写入剪贴板会改变序号，同步一下避免下个周期把它误判为新变更
	if sc, ok := e.clip.(seqClipboard); ok {
		if seq, ok2 := sc.Seq(); ok2 {
			e.localSeq, e.seqPrimed = seq, true
		}
	}
	e.mu.Unlock()

	src := derefString(prof.Source)
	if src != "" {
		e.log.Infof("⬇ 拉取（%d 字符，来自 %s）", runeCount(prof.Text), src)
	} else {
		e.log.Infof("⬇ 拉取（%d 字符）", runeCount(prof.Text))
	}
}

// ---------------------------------------------------------------- 手动操作

// PushNow 立即把当前剪贴板推送到远端。
//
// 这是**手动**推送（托盘菜单与界面右上角的「推送」按钮共用它），所以长度上限用
// ManualMaxTextChars 而不是 MaxTextChars——手动触发是用户的明确意图，允许突破
// 自动同步的上限。两个上限互不影响：自动同步仍然只看 MaxTextChars。
func (e *SyncEngine) PushNow() error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	cfg := e.Config()
	if cfg.DavURL == "" {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	text, err := e.clip.GetText()
	if err != nil {
		return err
	}
	if text == "" {
		return errors.New("本地剪贴板没有文本内容")
	}
	if n := runeCount(text); cfg.ManualMaxTextChars > 0 && n > cfg.ManualMaxTextChars {
		return fmt.Errorf("本地 %d 字符超手动推送上限 %d，未推送", n, cfg.ManualMaxTextChars)
	}
	if !e.pushRemote(cfg, text) {
		e.mu.Lock()
		msg := e.lastError
		e.mu.Unlock()
		if msg == "" {
			msg = "推送失败"
		}
		return errors.New(msg)
	}
	return nil
}

// PullNow 忽略 ETag 缓存，强制从远端拉取一次。
func (e *SyncEngine) PullNow() error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	cfg := e.Config()
	if cfg.DavURL == "" {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	e.pullRemote(cfg, true)
	e.mu.Lock()
	msg := e.lastError
	e.mu.Unlock()
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

// TestConnection 使用给定配置测试 WebDAV 连通性。
func TestConnection(cfg Config, log *LogBuffer) error {
	return newWebDAVClient(cfg, log).testConnection()
}
