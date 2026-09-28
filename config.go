package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是持久化到 config.json 的用户配置。
// 配置文件固定放在可执行文件同目录，便于「绿色便携」使用。
type Config struct {
	// WebDAV 服务器根地址，如 https://dav.jianguoyun.com/dav/
	DavURL string `json:"davUrl"`
	// RemotePath 是远端剪贴板文件的路径（界面上的「远程路径」），
	// **相对服务器根、具体到文件**，如 rime/clip/1.json。
	//
	// **留空 = 用 DefaultRemotePath**（xime/clipboard/current.json）。
	// 实际拼接就是 {davUrl}/{RemotePathOrDefault()}，不会再自动补任何文件名。
	RemotePath string `json:"remotePath"`
	Username   string `json:"username"`

	// Password 是**明文**密码，只存在于内存中（Basic Auth 需要它）。
	// 落盘时会被加密进 PasswordEnc，JSON 里绝不会写出这个字段。
	//
	// 还保留这个 json tag 是为了兼容旧配置：老版本把明文写在这里，
	// loadConfig 读到后会迁移成 PasswordEnc 并立即重写文件。
	Password string `json:"password,omitempty"`

	// PasswordEnc 是经 Windows DPAPI 加密后的密码（base64）。
	// 密文绑定当前 Windows 用户，换用户/换机器后需要重新填写密码。
	PasswordEnc string `json:"passwordEnc,omitempty"`

	// 本机设备名，写入 Profile 的 source 字段，便于对端区分来源
	DeviceName string `json:"deviceName"`
	// 是否启用同步
	Enabled bool `json:"enabled"`
	// 开机自动启动
	Autostart bool `json:"autostart"`
	// 远端轮询间隔（秒）
	PollSeconds int `json:"pollSeconds"`
	// 本地剪贴板检查间隔（秒）
	LocalPollSeconds int `json:"localPollSeconds"`
	// 局域网自签证书时放宽 TLS 校验
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
	// 自动推送文本长度上限（字符）。本地复制的内容超过这个长度就**自动同步**不推送到远端；
	// 0 表示不限制。用字符而不是字节，是因为用户看到的是「多少字」。
	MaxTextChars int `json:"maxTextChars"`
	// 手动推送的长度上限（字符）。托盘和界面上的「推送」按钮用它，而不是 MaxTextChars——
	// 手动触发是用户的明确意图，允许突破自动同步的上限（偶尔确实要把一段长文本强推过去）；
	// 0 表示不限制。
	ManualMaxTextChars int `json:"manualMaxTextChars"`
	// 是否把日志写入 logs/ 目录（无控制台构建下排查问题的唯一途径）
	LogToFile bool `json:"logToFile"`
	// 日志保留天数（含今天）；0 表示永久保留、不自动清理
	LogRetainDays int `json:"logRetainDays"`
	// 单个日志文件的大小上限（MB）；0 表示不限制。
	//
	// 达到上限后当天不再写文件（内存日志不受影响），避免某天异常刷屏把磁盘写满。
	// 与「日志保留天数」一起决定 logs/ 的占用上限（大致 = 上限 × 保留天数）。
	LogMaxFileMB int `json:"logMaxFileMB"`
	// 配置界面监听端口，默认 defaultUIPort。
	//
	// 这里存的**永远是一个具体端口号**：没有「留空 = 随机挑一个」这回事。
	// 地址是用户要存成书签的东西，所以宁可固定也不随机。
	// 端口被别的程序占着时**不**顺延：界面直接起不来，日志和托盘气泡会说明改哪里。
	UIPort int `json:"uiPort"`
}

// HasPassword 表示当前是否已配置密码（界面据此提示「留空则不修改」）。
func (c Config) HasPassword() bool {
	return c.Password != "" || c.PasswordEnc != ""
}

// 单个日志文件大小上限（MB）的取值范围。
// 默认值直接由 logbuf.go 里的字节常量换算，避免同一个数字写两遍。
const (
	defaultLogFileMB  = defaultMaxLogFileBytes >> 20
	maxLogFileMBLimit = 1024 // 1 GB，再大就不是「限制」了
)

// 配置界面默认监听端口。
//
// 选 31213 是因为它落在 Windows 动态端口段（49152–65535）**以下**，
// 系统不会把它临时分配出去，固定用它很稳。端口写死成一个具体值，
// 用户才能把界面地址存成书签——早期版本是「随机挑一个空闲端口」，
// 地址每次都可能变。
const defaultUIPort = 31213

// DefaultRemotePath 是「远程路径」留空时使用的远端文件路径（相对服务器根）。
//
// 注意这是**完整文件路径**，不是目录：界面上的「远程路径」要求具体到文件
// （如 rime/clip/1.json），留空就走协议约定的默认布局
// {davUrl}/xime/clipboard/current.json。
//
// 默认值**只在这一层**处理，`buildFileURL` 保持纯字符串拼接，不掺平台默认值。
const DefaultRemotePath = "xime/" + ClipboardFile

// RemotePathOrDefault 返回实际生效的远端文件路径（留空 → DefaultRemotePath）。
//
// 故意不在 normalize() 里把空值改写成默认值：那样用户留空、一保存，
// 输入框里就会被填上默认路径，之后分不清哪些是自己填的、哪些是程序塞的。
// 配置里保持用户原样，只在用的时候解析。
//
// 这里自己 TrimSpace 一遍，而不是指望调用方先 normalize：
// 「   」和「  /  」也该算「没配」。方法本身健壮，调用方就不用记这个前提。
func (c Config) RemotePathOrDefault() string {
	if p := strings.TrimSpace(c.RemotePath); stripSlashes(p) != "" {
		return p
	}
	return DefaultRemotePath
}

// FileURL 返回远端剪贴板文件 URL；davUrl 未配置时 ok=false。
//
// 所有需要远端文件地址的地方都应该走这里，别直接调 buildFileURL——
// 免得漏掉「远程路径留空」的默认值处理。
func (c Config) FileURL() (string, bool) {
	return buildFileURL(c.DavURL, c.RemotePathOrDefault())
}

// defaultConfig 返回带合理默认值的配置。
func defaultConfig() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "desktop"
	}
	return Config{
		RemotePath:       DefaultRemotePath,
		DeviceName:       host,
		Enabled:          true,
		PollSeconds:      30,
		LocalPollSeconds: 5,
		MaxTextChars:     300, // 出厂默认；0 = 不限制，用户可在「同步行为」里改
		// 手动推送（托盘/界面的「推送」按钮）默认放宽到 10000 字符：
		// 比自动同步的 300 宽得多，能应付「偶尔要把长文本强推过去」，
		// 又不至于一次把几 MB 的内容塞进远端文件。
		ManualMaxTextChars: 10000,
		LogToFile:          true,
		LogRetainDays:      7,
		LogMaxFileMB:       defaultLogFileMB,
		UIPort:             defaultUIPort,
	}
}

// logDirPath 返回日志目录：与配置文件同目录的 logs/ 子目录。
func logDirPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "logs")
}

// hadPriorRuns 判断「这个目录里以前跑过程序」。
//
// 依据是日志目录里已经有日志文件：日志默认保留 7 天，比配置文件更容易留下来。
// 用它来区分「真的首次运行」和「配置文件被删/被移走了」——两种情况都会让
// loadConfig 走「生成默认配置」这条路，但后者必须让用户知道，
// 否则用户会以为自己的设置「自己丢了」。
func hadPriorRuns(cfgPath string) bool {
	entries, err := os.ReadDir(logDirPath(cfgPath))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), logFileSuffix) {
			return true
		}
	}
	return false
}

// logMaxFileBytes 把「MB」配置换算成字节；0（不限制）返回 0。
func (c Config) logMaxFileBytes() int64 {
	if c.LogMaxFileMB <= 0 {
		return 0
	}
	return int64(c.LogMaxFileMB) << 20
}

// logFileOptions 由配置推导出日志落盘策略（启动时与保存配置时共用）。
func logFileOptions(cfgPath string, cfg Config) LogFileOptions {
	return LogFileOptions{
		Dir:          logDirPath(cfgPath),
		Enabled:      cfg.LogToFile,
		RetainDays:   cfg.LogRetainDays,
		MaxFileBytes: cfg.logMaxFileBytes(),
	}
}

// logPolicyText 用一句话描述日志落盘策略，便于在启动日志里一眼看清当前设置。
func logPolicyText(cfg Config) string {
	retain := fmt.Sprintf("保留 %d 天", cfg.LogRetainDays)
	if cfg.LogRetainDays <= 0 {
		retain = "永久保留"
	}
	limit := humanBytes(cfg.logMaxFileBytes())
	if cfg.LogMaxFileMB <= 0 {
		limit = "单文件不限大小"
	} else {
		limit = "单文件上限 " + limit
	}
	return retain + "，" + limit
}

// normalize 修正非法或缺失的字段，保证引擎拿到可用的值。
func (c *Config) normalize() {
	d := defaultConfig()
	c.DavURL = strings.TrimSpace(c.DavURL)
	c.RemotePath = strings.TrimSpace(c.RemotePath)
	c.Username = strings.TrimSpace(c.Username)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	if c.PollSeconds < 3 {
		c.PollSeconds = d.PollSeconds
	}
	if c.PollSeconds > 3600 {
		c.PollSeconds = 3600
	}
	if c.LocalPollSeconds < 1 {
		c.LocalPollSeconds = d.LocalPollSeconds
	}
	if c.LocalPollSeconds > 60 {
		c.LocalPollSeconds = 60
	}
	// 端口必须是一个能绑的合法端口号。非法值（手改配置、越界、早期版本留下的 0）
	// 一律退回默认端口——0 不再有「随机挑一个」的含义，留着它只会让启动失败。
	if c.UIPort <= 0 || c.UIPort > 65535 {
		c.UIPort = d.UIPort
	}
	if c.MaxTextChars < 0 {
		c.MaxTextChars = 0
	}
	// 负数视为非法（手改配置）；0 是合法值，含义是「不限制」
	if c.ManualMaxTextChars < 0 {
		c.ManualMaxTextChars = 0
	}
	if c.LogRetainDays < 0 {
		c.LogRetainDays = 0
	}
	if c.LogRetainDays > 365 {
		c.LogRetainDays = 365
	}
	// 0 = 不限制单文件大小；负数视为非法（旧配置/手改），退回默认
	if c.LogMaxFileMB < 0 {
		c.LogMaxFileMB = d.LogMaxFileMB
	}
	if c.LogMaxFileMB > maxLogFileMBLimit {
		c.LogMaxFileMB = maxLogFileMBLimit
	}
	if c.DeviceName == "" {
		c.DeviceName = d.DeviceName
	}
}

// ---------------------------------------------------------------------------
// 密码
// ---------------------------------------------------------------------------

// encryptPassword 把内存里的明文密码加密成落盘字段。
// 调用后 Password 被清空，因此序列化结果里不会出现明文。
func (c *Config) encryptPassword() error {
	if c.Password == "" {
		return nil
	}
	enc, err := protectSecret(c.Password)
	if err != nil {
		return err
	}
	c.PasswordEnc = enc
	c.Password = ""
	return nil
}

// resolvePassword 把密码在「明文（内存）」与「密文（磁盘）」之间对齐。
//
//   - 老配置把明文写在 password 里 → 加密进 PasswordEnc，返回 needsRewrite=true
//     （Password 保留在内存里，Basic Auth 还要用）
//   - 否则把 PasswordEnc 解到 Password
//
// warn 非空表示「能继续跑，但用户需要知道」。
func (c *Config) resolvePassword() (needsRewrite bool, warn string) {
	if c.Password != "" {
		enc, err := protectSecret(c.Password)
		if err != nil {
			return false, "配置里的明文密码加密失败，未改写文件：" + err.Error()
		}
		c.PasswordEnc = enc
		return true, "" // Password 留在内存里
	}
	if c.PasswordEnc == "" {
		return false, ""
	}
	plain, err := unprotectSecret(c.PasswordEnc)
	if err != nil {
		return false, "已保存的密码无法解密：" + err.Error() +
			"；请在配置界面重新填写密码后保存"
	}
	c.Password = plain
	return false, ""
}

// ---------------------------------------------------------------------------
// 读写
// ---------------------------------------------------------------------------

// configPath 决定配置文件位置：**固定为可执行文件同目录**。
//
// 这里刻意不做「目录不可写就退到 %AppData%」的回退：那会让配置文件悄悄
// 跑到用户找不到的地方，破坏「绿色便携」的预期。目录不可写时宁可明确报错
// （见 dirWritable 与启动时的告警），也不静默换位置。
func configPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

// dirWritable 探测目录是否可写。
// 程序目录要能放 config.json 与 logs/，不可写时应在启动时就告诉用户。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// loadConfig 读取配置并补齐缺失项。
//
// 除了读文件 + 反序列化，还会顺手把配置补齐并写回磁盘：
//   - 老配置里的明文 password 会被加密成 passwordEnc，让明文从磁盘上消失
//   - passwordEnc 会被解密到内存的 Password
//   - 文件里没写端口（或写的是 0/越界值）时补一个默认端口
//   - 首次运行时配置文件还不存在，写默认端口这一步顺带把配置生成出来
//     ——所以「配置文件存在」等价于「跑过一次」
//
// 返回的 warnings 是「不影响启动、但用户应该知道」的问题。
func loadConfig(path string) (Config, []string, error) {
	var warnings []string
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	missing := os.IsNotExist(err)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, &cfg); uerr != nil {
			return cfg, nil, fmt.Errorf("配置文件解析失败：%w", uerr)
		}
	case !missing:
		return cfg, nil, err
	}
	cfg.normalize()

	// 配置不见了、但日志还在 → 多半是被删除或移走了，不是真的首次运行。
	// 默默生成一份默认配置会让用户以为设置「自己丢了」，所以明确说出来。
	if missing && hadPriorRuns(path) {
		warnings = append(warnings, "配置文件不存在，已按默认值重新生成一份；"+
			"如果这不是首次运行，说明原来的 config.json 被删除或移走了，请检查（密码需要重新填写）")
	}

	// changes 收集「已经改写了配置」这件事本身需要告知用户的说明。
	// 只有真正写盘成功后才并入 warnings——否则用户会以为已经存下了。
	var changes []string

	if err == nil {
		rewritePwd, warn := cfg.resolvePassword()
		if warn != "" {
			warnings = append(warnings, warn)
		}
		if rewritePwd {
			changes = append(changes, "检测到配置里的明文密码，已改为加密保存")
		}
	}

	// 文件里没写端口（或写的是 0/越界值）时要补一个默认端口进文件。
	//
	// Config 是按默认值反序列化的，缺字段时从 cfg 上看不出差别，所以单独用指针
	// 探一次原文。端口是用户要在浏览器里敲的地址，只留在内存里不落盘的话，
	// 用户翻 config.json 会以为「这里没配」。
	//
	// 顺带这一条也兜住了「首次运行」：文件不存在 → 探不到 → 写出默认配置，
	// 所以「配置文件存在」仍然等价于「跑过一次」。
	var probe struct {
		UIPort *int `json:"uiPort"`
	}
	if err == nil {
		_ = json.Unmarshal(data, &probe)
	}
	portNeedsFix := probe.UIPort == nil || *probe.UIPort != cfg.UIPort

	if portNeedsFix || len(changes) > 0 {
		if err := saveConfig(path, cfg); err != nil {
			warnings = append(warnings, "配置写回失败（本次改动只对当前这次运行生效）："+err.Error())
		} else {
			warnings = append(warnings, changes...)
		}
	}
	return cfg, warnings, nil
}

// saveConfig 原子写入配置文件（先写临时文件再重命名）。
// 写盘前会把明文密码加密，所以磁盘上永远不会有明文。
func saveConfig(path string, cfg Config) error {
	cfg.normalize()
	if err := cfg.encryptPassword(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
