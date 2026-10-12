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
)

// 剪贴板内容的两种形态。current/remote 的「已达成一致」状态要带上类型，
// 否则「文本为空」和「是图片」会混在一起，导致远端图片被反复当成新内容重下。
const (
	kindText  = "text"
	kindImage = "image"
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

	// 图片相关。Kind 是 "" / text / image，界面上据此把「内容」一栏渲染成文本或图片；
	// *ImageBytes 是「已达成一致」的那张图的字节数（Kind 不是 image 时为 0）。
	//
	// *ImageHash 是界面取原图用的**内容地址**：/api/image 靠它做缓存键，
	// 同一张图的 URL 不变、图换了 URL 就换，界面不会画出一张已经过期的图。
	// 这里给的是本地自己算的 SHA-256，不是远端 JSON 里的 hash 字段——
	// 对端可能用别的算法（见 noteHash），拿它当缓存键会错判。
	//
	// *TextChars 是**未截断**的字符数。上面 CurrentText / RemoteText 是 150 字符的摘要，
	// 界面要显示的「多长」必须是完整长度，否则 1000 字的文本会显示成 150。
	CurrentKind        string `json:"currentKind"`
	RemoteKind         string `json:"remoteKind"`
	CurrentImageBytes  int    `json:"currentImageBytes"`
	RemoteImageBytes   int    `json:"remoteImageBytes"`
	CurrentImageHash   string `json:"currentImageHash"`
	RemoteImageHash    string `json:"remoteImageHash"`
	CurrentTextChars   int    `json:"currentTextChars"`
	RemoteTextChars    int    `json:"remoteTextChars"`
	ImageOversizeCount int    `json:"imageOversizeCount"`
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
	remoteHash   string
	localHash    string
	hashMismatch bool
	hashWarned   bool

	// 图片的「已达成一致」状态，与上面的 current 并列。
	// currentKind 指明当前一致的是哪一种内容：文本用 current 比，图片用 currentImageHash 比。
	// 图片不能只靠「文本相等」判重——图片记录的 text 恒为空串，
	// 拿它比的话任何一次本地图片变更都会被误判成「和远端一致」而漏推。
	currentKind      string
	currentImageHash string
	currentImageSize int
	// 界面要把图片直接画出来，所以额外留一份 PNG 字节。
	// PNG 是压过的（4K 截图约几十 KB），两份的内存代价可以忽略。
	// 只在「引擎手里确实有这张图的字节」时非 nil：推送过、拉取过、或启动时读过剪贴板。
	// 只有元数据没下过原图时（图片同步关着而远端是图）保持 nil，
	// 界面上就只显示体积、不显示图——不为了画一张图去偷偷下附件或现读剪贴板，
	// 那是同步该干的事，不该由一个界面请求触发。
	currentImagePNG []byte

	// 远端那边是什么内容（用于界面展示，以及判断要不要下载附件）
	remoteKind      string
	remoteImageSize int
	// 远端图片的字节与它**本地算的** SHA-256（理由见 Status.ImageHash 的注释）。
	// 与 remoteImageSize 不同步：Size 来自远端 JSON 的元数据，
	// 而这两个字段只有在真的拿到字节之后才有值。
	remoteImagePNG  []byte
	remoteImageHash string

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

	// 图片因超过体积上限被跳过推送的次数，以及「同一张图只告警一次」的去重值
	imageOversize     int
	oversizeImageHash string

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
	e.log.SyncInfof("配置已更新：%s（远程路径 %q，轮询 %ds）", cfg.DavURL, cfg.RemotePathOrDefault(), cfg.PollSeconds)
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
		e.log.SyncErrorf("%s", msg)
	}
}

func (e *SyncEngine) clearError() {
	e.mu.Lock()
	e.lastError = ""
	e.lastErrLog = ""
	e.mu.Unlock()
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
		e.log.SyncWarnf("对端 hash 与本地 SHA-256 不一致（远端 %s / 本地 %s），不影响同步",
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
	e.log.SyncWarnf("%s 遇到 503 限流，退避至 %s（坚果云免费版限 600 次/30 分钟）",
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

		CurrentKind:        e.currentKind,
		RemoteKind:         e.remoteKind,
		CurrentImageBytes:  e.currentImageSize,
		RemoteImageBytes:   e.remoteImageSize,
		CurrentImageHash:   e.currentImageHash,
		RemoteImageHash:    e.remoteImageHash,
		CurrentTextChars:   runeCount(e.current),
		RemoteTextChars:    runeCount(e.remoteText),
		ImageOversizeCount: e.imageOversize,
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
			e.log.SyncInfof("同步已停止")
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
	text, err := e.clip.GetText()
	switch {
	case err != nil:
		e.log.SyncWarnf("读取剪贴板失败：%v", err)
	case text != "":
		// 超长的剪贴板内容同样不当作基线（理由见 checkLocal）：
		// 它不会被推送，就不该出现在界面的「本地内容」里。
		if n := runeCount(text); cfg.MaxTextChars > 0 && n > cfg.MaxTextChars {
			e.mu.Lock()
			e.oversizeText = text
			e.oversize++
			e.mu.Unlock()
			e.log.SyncWarnf("本地 %d 字符超自动同步上限 %d，未推送", n, cfg.MaxTextChars)
		} else {
			e.mu.Lock()
			e.current = text
			e.currentKind = kindText
			e.mu.Unlock()
			e.log.SyncInfof("启动基线：本地 %d 字符", runeCount(text))
		}
	default:
		// 没有文本：剪贴板里可能是图片。同样立成基线，
		// 免得刚启动就把一张本来就在本机的图当成「新复制的内容」推上去。
		e.primeImageBaseline(cfg)
	}

	e.markLocalSeq()
	e.log.SyncInfof("开始同步：%s", e.describeRemote())
	e.syncCycle(true)
}

// primeImageBaseline 在启动时把剪贴板里的图片记为基线（没有文本时才会走到这里）。
func (e *SyncEngine) primeImageBaseline(cfg Config) {
	if !cfg.SyncImages {
		return
	}
	ic, ok := e.clip.(imageClipboard)
	if !ok || !ic.HasImage() {
		return
	}
	pngData, err := ic.GetImage()
	if err != nil || pngData == nil {
		return
	}
	e.mu.Lock()
	e.current = ""
	e.currentKind = kindImage
	e.currentImageHash = hashBytes(pngData)
	e.currentImageSize = len(pngData)
	e.currentImagePNG = pngData
	e.mu.Unlock()
	e.log.SyncInfof("启动基线：本地图片 %s", humanBytes(int64(len(pngData))))
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
//
// 内容优先级是**文本优先**：剪贴板同时有文本和位图时（Excel/Word 复制单元格、
// 网页复制富文本都会这样）按文本同步。用户复制的「是一段内容」，
// 把它当图片推过去只会得到一张看不清的截图。只有完全没有文本时才当图片处理。
func (e *SyncEngine) checkLocal(cfg Config) {
	if !e.localChanged() {
		return
	}
	text, err := e.clip.GetText()
	if err != nil {
		e.setError(err)
		return
	}
	if text != "" {
		e.handleLocalText(cfg, text)
		return
	}
	e.handleLocalImage(cfg)
}

// handleLocalText 处理「剪贴板里是文本」的情况（长度上限逻辑与图片无关）。
func (e *SyncEngine) handleLocalText(cfg Config, text string) {
	if text == e.getCurrent() {
		return
	}
	if n := runeCount(text); cfg.MaxTextChars > 0 && n > cfg.MaxTextChars {
		// 超长内容**不写进 current**。current 的语义是「本地与远端一致的那份内容」，
		// 界面上「本地内容」直接显示它，所以把它换成一份从未推送成功的内容会同时造成：
		//   ① 界面上显示出一段其实没同步过去的内容；
		//   ② 下一轮远端轮询发现 remote != current，会把远端内容写回剪贴板，
		//      把用户刚复制的这段长文本冲掉。
		// 「同一条只告警一次」由下面的 oversizeText 兜底
		// （正常情况下剪贴板序号没变就不会重复读到）。
		e.mu.Lock()
		first := e.oversizeText != text
		e.oversizeText = text
		e.oversize++ // 让界面能告诉用户「有多少次因为太长被跳过了」
		e.mu.Unlock()
		if first {
			e.log.SyncWarnf("本地 %d 字符超自动同步上限 %d，未推送", n, cfg.MaxTextChars)
		}
		return
	}
	e.pushRemote(cfg, text)
}

// handleLocalImage 处理「剪贴板里没有文本、但可能有图片」的情况。
//
// 没有文本也没有图片时直接返回：不推送空文本。
// 否则「复制一张图片」会把远端的文本记录覆盖成一条空文本
// （旧实现就是这么做的，图片同步关掉时尤其明显）。
func (e *SyncEngine) handleLocalImage(cfg Config) {
	ic, ok := e.clip.(imageClipboard)
	if !ok || !ic.HasImage() {
		return
	}
	if !cfg.SyncImages {
		// 图片同步关着就静默跳过：这是用户自己在「同步设置」里的选择，
		// 每复制一张图就记一条「为什么没同步」只会把日志刷满，没有信息量。
		return
	}
	pngData, err := ic.GetImage()
	if err != nil {
		e.setError(err)
		return
	}
	if pngData == nil {
		return
	}
	hash := hashBytes(pngData)
	if e.getCurrentKind() == kindImage && e.getCurrentImageHash() == hash {
		return
	}
	if mb := cfg.MaxImageMB; mb > 0 && len(pngData) > mb<<20 {
		e.mu.Lock()
		first := e.oversizeImageHash != hash
		e.oversizeImageHash = hash
		e.imageOversize++ // 界面上的「图片超限跳过」
		e.mu.Unlock()
		if first {
			e.log.SyncWarnf("本地图片 %s 超自动同步上限 %d MB，未推送",
				humanBytes(int64(len(pngData))), mb)
		}
		return
	}
	e.pushImageRemote(cfg, pngData)
}

// getCurrent 返回「两端已达成一致的内容」（文本）。
func (e *SyncEngine) getCurrent() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// getCurrentKind 返回「已达成一致的内容」是哪一种（"" / text / image）。
func (e *SyncEngine) getCurrentKind() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentKind
}

// getCurrentImageHash 返回「已达成一致的图片」的内容哈希。
func (e *SyncEngine) getCurrentImageHash() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentImageHash
}

// ImagePNG 返回界面上要画的那张图的 PNG 字节，以及它的内容哈希（缓存键）。
//
// side 取 "local" / "remote"；只有引擎手里确实有字节时 ok 才为真——
// 推送过、拉取过、或启动时读过剪贴板。
//
// 刻意**不**为了画一张图去现读剪贴板、也不去现下附件：那是同步的职责，
// 不该由一个界面请求触发。现读剪贴板还会和截图工具抢剪贴板，
// 正是「截图进不了 Ditto」那个 bug 的来源，不能从这条路上再开一个口子。
//
// 返回的切片是引擎自己那份，调用方只能读、不能改。
func (e *SyncEngine) ImagePNG(side string) ([]byte, string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch side {
	case "local":
		if e.currentKind != kindImage || len(e.currentImagePNG) == 0 {
			return nil, "", false
		}
		return e.currentImagePNG, e.currentImageHash, true
	case "remote":
		if e.remoteKind != kindImage || len(e.remoteImagePNG) == 0 {
			return nil, "", false
		}
		return e.remoteImagePNG, e.remoteImageHash, true
	}
	return nil, "", false
}

// localChanged 报告剪贴板自上次读取以来是否变化。
// 有廉价序号（Windows）时先用它预判，序号没变就不去读剪贴板内容。
func (e *SyncEngine) localChanged() bool {
	if !e.hasSeq {
		return true
	}
	sc, ok := e.clip.(seqClipboard)
	if !ok {
		return true
	}
	seq, ok := sc.Seq()
	if !ok {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := !e.seqPrimed || seq != e.localSeq
	e.localSeq, e.seqPrimed = seq, true
	return changed
}

// markLocalSeq 把当前剪贴板序号记为「已读」。
//
// 自己往剪贴板写完内容后必须调一次：写入会推进序号，不同步的话
// 下一个本地检查周期会把这次写入当成「用户复制了新内容」而再推回远端（回声）。
func (e *SyncEngine) markLocalSeq() {
	sc, ok := e.clip.(seqClipboard)
	if !ok {
		return
	}
	seq, ok := sc.Seq()
	if !ok {
		return
	}
	e.mu.Lock()
	e.localSeq, e.seqPrimed = seq, true
	e.mu.Unlock()
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
	e.currentKind = kindText
	e.currentImageHash = ""
	e.currentImageSize = 0
	e.currentImagePNG = nil
	e.remoteExists = true
	e.remoteText = text
	e.remoteKind = kindText
	e.remoteImageSize = 0
	e.remoteImagePNG = nil
	e.remoteImageHash = ""
	e.pushCount++
	e.lastError, e.lastErrLog = "", ""
	e.mu.Unlock()

	e.log.SyncInfof("⬆ 推送 （%d 字符）", runeCount(text))
	return true
}

// pushImageRemote 把图片推送到远端：**先 PUT blob、再 PUT JSON**。
//
// 顺序不能反。JSON 是索引、blob 是内容：
//   - 先写 JSON 的话，对端可能在我们上传 blob 之前就拉到 JSON，得到「有元数据、没图」的空条目；
//   - 中途失败时，远端会留下一个指向不存在 blob 的悬空引用，而它看起来是「一张图」，
//     对端会反复下载失败。反过来（blob 多余）只是浪费一次存储，无害。
func (e *SyncEngine) pushImageRemote(cfg Config, pngData []byte) bool {
	prof := newImageProfile(pngData, cfg.DeviceName)
	if prof.DataName == nil || *prof.DataName == "" {
		e.setError(errors.New("内部错误：图片附件名为空"))
		return false
	}
	blobErr := e.client.putBlob(*prof.DataName, pngData)

	e.mu.Lock()
	e.lastPushAt = time.Now()
	e.mu.Unlock()

	if blobErr != nil {
		if errors.Is(blobErr, ErrRateLimited) {
			e.backoff(pushBackoff, "推送")
			return false
		}
		e.setError(fmt.Errorf("上传图片失败：%w", blobErr))
		return false
	}

	payload, err := prof.encode()
	if err != nil {
		e.setError(err)
		return false
	}
	hdr, err := e.client.put(payload)
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
	e.current = ""
	e.currentKind = kindImage
	e.currentImageHash = prof.Hash
	e.currentImageSize = len(pngData)
	e.currentImagePNG = pngData
	e.remoteExists = true
	e.remoteText = ""
	e.remoteKind = kindImage
	e.remoteImageSize = len(pngData)
	// 刚推上去的就是远端现在的内容，字节同一份，界面两边都能画出来。
	// prof.Hash 就是 hashBytes(pngData)，所以拿它当远端那一侧的缓存键也对。
	e.remoteImagePNG = pngData
	e.remoteImageHash = prof.Hash
	e.pushCount++
	e.lastError, e.lastErrLog = "", ""
	e.mu.Unlock()

	e.log.SyncInfof("⬆ 推送 （图片 %s）", humanBytes(int64(len(pngData))))
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
		kind := e.currentKind
		e.mu.Unlock()
		e.clearError()
		if !wasKnown {
			switch kind {
			case kindText:
				if current != "" {
					e.log.SyncInfof("远端无文件，用本地初始化")
					e.pushRemote(cfg, current)
				}
			case kindImage:
				e.seedRemoteWithLocalImage(cfg)
			}
		}
		return
	}

	if len(body) == 0 {
		e.clearError()
		return
	}

	prof, plain, ok := decodeProfile(body)
	if !ok {
		return
	}
	if plain {
		e.log.SyncWarnf("远端为纯文本（旧格式），已兼容")
	}

	// 先把 ETag 与「远端是什么」记下来，再决定怎么应用。
	// 顺序有讲究：这样附件下载失败时 ETag 也留得住（失败分支自己会清掉它来触发重试），
	// 而 304 命中之后就不会每个轮询周期都重下 JSON。
	e.mu.Lock()
	if et := cacheETag(hdr); et != "" {
		e.lastETag = et
	}
	e.remoteExists = true
	e.remoteText = prof.Text
	// 只有元数据，先把手里的字节丢掉：它可能是**上一张**图的，
	// 留着会让界面画出一张已经不是远端当前内容的图。
	// 等真的下载完（applyRemoteImage）再放回来。
	e.remoteImagePNG = nil
	e.remoteImageHash = ""
	if prof.HasData {
		e.remoteKind = kindImage
		e.remoteImageSize = prof.Size
	} else {
		e.remoteKind = kindText
		e.remoteImageSize = 0
	}
	current := e.current
	currentKind := e.currentKind
	currentImageHash := e.currentImageHash
	e.mu.Unlock()

	// 图片记录：拉附件、写剪贴板
	if prof.HasData {
		e.applyRemoteImage(cfg, prof, currentKind, currentImageHash)
		return
	}

	e.noteHash(prof)
	if prof.Text == current && currentKind == kindText {
		e.clearError()
		return
	}

	if err := e.clip.SetText(prof.Text); err != nil {
		e.setError(err)
		return
	}
	e.mu.Lock()
	e.current = prof.Text
	e.currentKind = kindText
	e.currentImageHash = ""
	e.currentImageSize = 0
	e.currentImagePNG = nil
	e.pullCount++
	e.lastError, e.lastErrLog = "", ""
	e.mu.Unlock()
	// 写入剪贴板会改变序号，同步一下避免下个周期把它误判为新变更
	e.markLocalSeq()

	src := derefString(prof.Source)
	if src != "" {
		e.log.SyncInfof("⬇ 拉取（%d 字符，来自 %s）", runeCount(prof.Text), src)
	} else {
		e.log.SyncInfof("⬇ 拉取（%d 字符）", runeCount(prof.Text))
	}
}

// applyRemoteImage 下载远端图片附件并写入本地剪贴板。
//
// currentKind / currentImageHash 是调用方在加锁状态下取的「已达成一致」快照，
// 用来做回声抑制：同一张图不重复写剪贴板。
func (e *SyncEngine) applyRemoteImage(cfg Config, prof Profile, currentKind, currentImageHash string) {
	if prof.DataName == nil || *prof.DataName == "" {
		e.setError(errors.New("远端图片缺少附件名（data_name）"))
		return
	}
	if !cfg.SyncImages {
		// 图片同步关着：远端是图片就跳过。ETag 已缓存，不会每个轮询周期都重下 JSON。
		// 同样静默——理由见 handleLocalImage。
		return
	}
	data, err := e.client.getBlob(*prof.DataName)
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			e.backoff(pullBackoff, "拉取")
			return
		}
		e.setError(fmt.Errorf("下载图片失败：%w", err))
		// 附件可能还没传上来（对端 push「先 blob 后 JSON」的中间态）：
		// 清掉 ETag，下一轮重新拉 JSON 再试；否则 304 会让这次失败变成永久跳过
		e.mu.Lock()
		e.lastETag = ""
		e.mu.Unlock()
		return
	}
	if _, err := decodePNG(data); err != nil {
		e.setError(fmt.Errorf("远端附件不是合法图片：%w", err))
		e.mu.Lock()
		e.lastETag = ""
		e.mu.Unlock()
		return
	}
	// 判重按**下载到的字节**算，不信任远端 hash 字段：对端可能用别的算法，
	// 直接比它会让同一张图被反复写进剪贴板。
	hash := hashBytes(data)
	if currentKind == kindImage && currentImageHash == hash {
		e.clearError()
		return
	}
	ic, ok := e.clip.(imageClipboard)
	if !ok {
		e.log.SyncWarnf("当前平台不支持写入剪贴板图片，已跳过远端图片")
		return
	}
	if err := ic.SetImage(data); err != nil {
		e.setError(err)
		return
	}
	e.mu.Lock()
	e.current = ""
	e.currentKind = kindImage
	e.currentImageHash = hash
	e.currentImageSize = len(data)
	// 刚下下来并写进剪贴板的就是这一份，两边字节相同。
	e.currentImagePNG = data
	e.remoteImagePNG = data
	e.remoteImageHash = hash
	e.pullCount++
	e.lastError, e.lastErrLog = "", ""
	e.mu.Unlock()
	e.markLocalSeq()

	if src := derefString(prof.Source); src != "" {
		e.log.SyncInfof("⬇ 拉取（图片 %s，来自 %s）", humanBytes(int64(len(data))), src)
	} else {
		e.log.SyncInfof("⬇ 拉取（图片 %s）", humanBytes(int64(len(data))))
	}
}

// seedRemoteWithLocalImage 远端还没有文件时，用本机剪贴板里的图片初始化远端。
func (e *SyncEngine) seedRemoteWithLocalImage(cfg Config) {
	ic, ok := e.clip.(imageClipboard)
	if !ok || !ic.HasImage() {
		return
	}
	pngData, err := ic.GetImage()
	if err != nil || pngData == nil {
		return
	}
	if mb := cfg.MaxImageMB; mb > 0 && len(pngData) > mb<<20 {
		return // 超自动上限的图片不参与初始化（与文本超长不推送一致）
	}
	e.log.SyncInfof("远端无文件，用本地图片初始化")
	e.pushImageRemote(cfg, pngData)
}

// ---------------------------------------------------------------- 手动操作

// PushNow 立即把当前剪贴板推送到远端。
//
// 这是**手动**推送（托盘菜单与界面右上角的「推送」按钮共用它），所以上限用
// ManualMaxTextChars / ManualMaxImageMB 而不是自动那两个——手动触发是用户的明确意图，
// 允许突破自动同步的上限。两组上限互不影响：自动同步仍然只看 MaxTextChars / MaxImageMB。
//
// 内容优先级与自动同步一致：有文本推文本，没有文本才推图片。
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
	if text != "" {
		if n := runeCount(text); cfg.ManualMaxTextChars > 0 && n > cfg.ManualMaxTextChars {
			return fmt.Errorf("本地 %d 字符超手动推送上限 %d，未推送", n, cfg.ManualMaxTextChars)
		}
		if !e.pushRemote(cfg, text) {
			return e.lastPushError()
		}
		return nil
	}
	return e.pushLocalImageNow(cfg)
}

// pushLocalImageNow 手动推送剪贴板里的图片（没有文本时才会走到）。
func (e *SyncEngine) pushLocalImageNow(cfg Config) error {
	ic, ok := e.clip.(imageClipboard)
	if !ok || !ic.HasImage() {
		return errors.New("本地剪贴板没有可同步的内容")
	}
	if !cfg.SyncImages {
		return errors.New("本地剪贴板是图片，但图片同步已关闭")
	}
	pngData, err := ic.GetImage()
	if err != nil {
		return err
	}
	if pngData == nil {
		return errors.New("本地剪贴板没有可同步的内容")
	}
	if mb := cfg.ManualMaxImageMB; mb > 0 && len(pngData) > mb<<20 {
		return fmt.Errorf("本地图片 %s 超手动推送上限 %d MB，未推送",
			humanBytes(int64(len(pngData))), mb)
	}
	if !e.pushImageRemote(cfg, pngData) {
		return e.lastPushError()
	}
	return nil
}

// lastPushError 取上一次失败的原因（手动推送用它拼返回的错误）。
func (e *SyncEngine) lastPushError() error {
	e.mu.Lock()
	msg := e.lastError
	e.mu.Unlock()
	if msg == "" {
		msg = "推送失败"
	}
	return errors.New(msg)
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

// TestConnection 使用给定配置测试 WebDAV 连通性，并**把远端目录建好**。
//
// 建目录放在这里、而不是每次同步时做，是有意的：
//   - 这是用户配置连接时唯一会主动触发的动作，做一次就够；
//   - 同步过程中就不必再「猜服务器用哪个状态码表示父目录不存在」——
//     403 那次故障正是栽在这个猜测上（有的服务器给 403 而不是标准的 409）。
//
// 幂等：目录已存在时 MKCOL 返回 405，被当作成功，所以反复点「测试连接」没有副作用。
func TestConnection(cfg Config, log *LogBuffer) error {
	client := newWebDAVClient(cfg, log)
	if err := client.testConnection(); err != nil {
		return err
	}
	if err := client.ensureRemoteLayout(); err != nil {
		return fmt.Errorf("已连上服务器，但准备远端目录失败：%w", err)
	}
	return nil
}
