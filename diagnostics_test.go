package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 对端 hash 与本地算法不一致时应给出提示，但绝不能影响内容同步。
func TestHashMismatchDetection(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	// 对端写了一个非 SHA-256 的 hash
	fake.setFile(remoteFilePath,
		`{"type":"text","hash":"deadbeefdeadbeef","text":"来自手机的内容","size":21,"source":"phone"}`)
	engine.syncCycle(true)

	st := engine.Status()
	if !st.HashMismatch {
		t.Errorf("应检测到 hash 不一致：remoteHash=%q localHash=%q", st.RemoteHash, st.LocalHash)
	}
	if clip.get() != "来自手机的内容" {
		t.Errorf("hash 不一致不应影响同步，实际剪贴板：%q", clip.get())
	}

	// 换成正确的 SHA-256 后提示应消失
	const text2 = "第二条手机内容"
	fake.setFile(remoteFilePath,
		`{"type":"text","hash":"`+hashText(text2)+`","text":"`+text2+`","size":18,"source":"phone"}`)
	engine.syncCycle(true)

	if engine.Status().HashMismatch {
		t.Error("hash 一致时不应再报告不一致")
	}
	if clip.get() != text2 {
		t.Errorf("内容未同步：%q", clip.get())
	}
}

// 对端 hash 为空（交给宿主计算）时不应报告不一致。
func TestHashEmptyNoMismatch(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	fake.setFile(remoteFilePath, `{"type":"text","hash":"","text":"abc","size":3}`)
	engine.syncCycle(true)

	if engine.Status().HashMismatch {
		t.Error("hash 留空时不应报告不一致")
	}
	if clip.get() != "abc" {
		t.Errorf("内容未同步：%q", clip.get())
	}
}

// lastBytes 返回字符串末尾最多 n 个字节，仅用于让测试失败信息别刷屏。
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// testLogOptions 是测试里最常用的落盘策略：写入 dir、保留 7 天、单文件用默认上限。
func testLogOptions(dir string) LogFileOptions {
	return LogFileOptions{
		Dir:          dir,
		Enabled:      true,
		RetainDays:   7,
		MaxFileBytes: defaultMaxLogFileBytes,
	}
}

// 日志落盘：按天分文件、写入内容正确、关闭后不再写。
func TestLogFile(t *testing.T) {
	dir := t.TempDir()
	log := NewLogBuffer(10)
	if err := log.SetLogFile(testLogOptions(dir)); err != nil {
		t.Fatalf("打开日志目录失败：%v", err)
	}
	log.Infof("hello %s", "world")
	log.Errorf("出错了：%d", 42)

	path := log.CurrentFile()
	if path == "" {
		t.Fatal("落盘时 CurrentFile 不应为空")
	}
	wantName := logFilePrefix + time.Now().Format(logDateLayout) + logFileSuffix
	if got := filepath.Base(path); got != wantName {
		t.Errorf("日志文件名 = %q，期望 %q", got, wantName)
	}
	if got := filepath.Dir(path); got != dir {
		t.Errorf("日志目录 = %q，期望 %q", got, dir)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件失败：%v", err)
	}
	content := string(data)
	for _, want := range []string{"hello world", "出错了：42", "error"} {
		if !strings.Contains(content, want) {
			t.Errorf("日志文件缺少 %q，实际内容：\n%s", want, content)
		}
	}

	// 关闭落盘后不应再写文件，CurrentFile 也应为空
	if err := log.SetLogFile(LogFileOptions{RetainDays: 7}); err != nil {
		t.Fatalf("关闭日志落盘失败：%v", err)
	}
	if p := log.CurrentFile(); p != "" {
		t.Errorf("关闭落盘后 CurrentFile = %q，期望空串", p)
	}
	log.Infof("关闭后不应出现")
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "关闭后不应出现") {
		t.Error("关闭落盘之后仍在写文件")
	}
	// 内存日志不受落盘开关影响
	if len(log.Lines(0)) == 0 {
		t.Error("关闭落盘后内存日志不应受影响")
	}
}

// 单个日志文件达到上限后应停止写入，而不是无限增长。
// 上限来自配置，这里用一个远小于默认值的数字，确保真的按配置生效。
func TestLogFileSizeCap(t *testing.T) {
	dir := t.TempDir()
	log := NewLogBuffer(10)
	const cap64k = 64 << 10
	opt := testLogOptions(dir)
	opt.MaxFileBytes = cap64k
	if err := log.SetLogFile(opt); err != nil {
		t.Fatal(err)
	}
	// 测试结束前必须关掉文件句柄，否则 Windows 下 TempDir 清理会失败
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	big := strings.Repeat("x", 1024)
	for i := 0; i < 500; i++ { // 约 500 KiB，足以越过 64 KiB 上限
		log.Infof("%s", big)
	}

	info, err := os.Stat(log.CurrentFile())
	if err != nil {
		t.Fatal(err)
	}
	// 允许超出一行的余量，但绝不该接近「写满 500 KiB」
	if info.Size() > cap64k+4096 {
		t.Errorf("日志未按配置的上限封顶：体积 %d 字节（上限 %d）", info.Size(), cap64k)
	}
	data, _ := os.ReadFile(log.CurrentFile())
	if !strings.Contains(string(data), "64 KB 上限") {
		t.Errorf("封顶后应留下说明上限的提示，实际内容结尾：\n%s", lastBytes(string(data), 200))
	}
	if len(log.Lines(0)) == 0 {
		t.Error("封顶后内存日志不应受影响")
	}
}

// 单文件上限设为 0 表示不限制：内容再大也不封顶。
func TestLogFileSizeUnlimited(t *testing.T) {
	dir := t.TempDir()
	log := NewLogBuffer(10)
	opt := testLogOptions(dir)
	opt.MaxFileBytes = 0
	if err := log.SetLogFile(opt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	big := strings.Repeat("x", 1024)
	for i := 0; i < 300; i++ { // 约 300 KiB，远超默认的 2 MiB 之外也要能写
		log.Infof("%s", big)
	}

	info, err := os.Stat(log.CurrentFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 300<<10 {
		t.Errorf("不限制时不应封顶，体积只有 %d 字节", info.Size())
	}
	data, _ := os.ReadFile(log.CurrentFile())
	if strings.Contains(string(data), "上限") {
		t.Error("不限制时不该出现封顶提示")
	}
}

// 配置里的 MB 上限要正确换算成字节，非法值要收敛。
func TestLogMaxFileMBBounds(t *testing.T) {
	cfg := defaultConfig()
	if cfg.LogMaxFileMB != 2 || cfg.logMaxFileBytes() != 2<<20 {
		t.Errorf("默认单文件上限应为 2 MB / %d 字节，实际 %d MB / %d 字节",
			2<<20, cfg.LogMaxFileMB, cfg.logMaxFileBytes())
	}

	// 0 是合法值，表示不限制，不能被 normalize 改回默认
	cfg = defaultConfig()
	cfg.LogMaxFileMB = 0
	cfg.normalize()
	if cfg.LogMaxFileMB != 0 || cfg.logMaxFileBytes() != 0 {
		t.Errorf("0 应表示不限制，实际 %d MB / %d 字节", cfg.LogMaxFileMB, cfg.logMaxFileBytes())
	}

	// 负数（手改配置文件）退回默认
	cfg = defaultConfig()
	cfg.LogMaxFileMB = -5
	cfg.normalize()
	if cfg.LogMaxFileMB != defaultLogFileMB {
		t.Errorf("负数应退回默认 %d，实际 %d", defaultLogFileMB, cfg.LogMaxFileMB)
	}

	// 过大收敛到上限
	cfg = defaultConfig()
	cfg.LogMaxFileMB = 99999
	cfg.normalize()
	if cfg.LogMaxFileMB != maxLogFileMBLimit {
		t.Errorf("过大应收敛到 %d，实际 %d", maxLogFileMBLimit, cfg.LogMaxFileMB)
	}

	// 换算：16 MB → 16<<20 字节
	cfg = defaultConfig()
	cfg.LogMaxFileMB = 16
	if got := cfg.logMaxFileBytes(); got != 16<<20 {
		t.Errorf("16 MB 应换算为 %d 字节，实际 %d", 16<<20, got)
	}
}

// 旧配置里没有 logMaxFileMB 时，必须落到默认值而不是 0（0 会被理解成「不限制」）。
func TestConfigUpgradeKeepsLogFileCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"davUrl":"https://host/dav/","remotePath":"xime","username":"u",
	         "enabled":true,"logToFile":true,"logRetainDays":7}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读取旧配置失败：%v", err)
	}
	if cfg.LogMaxFileMB != defaultLogFileMB {
		t.Errorf("旧配置升级后单文件上限应为 %d，实际 %d", defaultLogFileMB, cfg.LogMaxFileMB)
	}
}

// 保留期清理：只删过期的日志文件，且不碰名字不匹配的文件。
func TestLogRetention(t *testing.T) {
	dir := t.TempDir()
	log := NewLogBuffer(10)
	if err := log.SetLogFile(LogFileOptions{Dir: dir, Enabled: true, RetainDays: 3, MaxFileBytes: defaultMaxLogFileBytes}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	now := time.Now()
	mk := func(daysAgo int) string {
		name := logFilePrefix + now.AddDate(0, 0, -daysAgo).Format(logDateLayout) + logFileSuffix
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 保留 3 天 = 今天 + 前 2 天
	keep := []string{mk(0), mk(1), mk(2)}
	drop := []string{mk(3), mk(10), mk(200)}

	// 名字不符合规则的文件不应被误删
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("别删我"), 0o644); err != nil {
		t.Fatal(err)
	}

	log.Cleanup()

	for _, p := range keep {
		if !fileExists(p) {
			t.Errorf("应保留 %s", filepath.Base(p))
		}
	}
	for _, p := range drop {
		if fileExists(p) {
			t.Errorf("应删除 %s", filepath.Base(p))
		}
	}
	if !fileExists(other) {
		t.Error("不该删除名字不符合日志规则的文件")
	}

	// 保留天数为 0 表示永久保留
	log2 := NewLogBuffer(10)
	if err := log2.SetLogFile(LogFileOptions{Dir: dir, Enabled: true, MaxFileBytes: defaultMaxLogFileBytes}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log2.SetLogFile(LogFileOptions{}) })
	ancient := mk(999)
	log2.Cleanup()
	if !fileExists(ancient) {
		t.Error("保留天数为 0 时不应删除任何日志")
	}
}

// logFileDate 只负责「剥掉已知前缀、取出日期段」，
// 日期本身合不合法交给调用方的 ParseInLocation 判断。
func TestLogFileDate(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"xime-clip-sync-2026-09-27.log", "2026-09-27", true},
		{"xime-clip-sync-notes.log", "notes", true}, // 前缀对但日期不合法 → 由调用方挡掉
		{"xime-clip-sync-.log", "", true},
		{"notes.txt", "", false},
		{"xime-clip-sync-2026-09-27.txt", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := logFileDate(c.name)
		if ok != c.ok || got != c.want {
			t.Errorf("logFileDate(%q) = (%q, %v)，期望 (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// 保留期清理只认「前缀 + 日期」的日志文件，别的文件一个都不许碰。
//
// logs/ 就在 exe 旁边，用户完全可能把自己的东西丢进去。前缀像日志、但日期段不是日期的
// 文件（如 `xime-clip-sync-notes.log`）必须留着——删用户自己的文件是不可逆的。
func TestLogRetentionSkipsFilesWithoutDate(t *testing.T) {
	dir := t.TempDir()
	log := NewLogBuffer(10)
	if err := log.SetLogFile(LogFileOptions{Dir: dir, Enabled: true, RetainDays: 3, MaxFileBytes: defaultMaxLogFileBytes}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	now := time.Now()
	mk := func(name string, daysAgo int) string {
		p := filepath.Join(dir, name)
		if daysAgo >= 0 {
			p = filepath.Join(dir, logFilePrefix+now.AddDate(0, 0, -daysAgo).Format(logDateLayout)+logFileSuffix)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	fresh := mk("", 1) // 保留期内 → 留
	old := mk("", 30)  // 已过期 → 清
	odd := mk(logFilePrefix+"notes"+logFileSuffix, -1)
	// 日期段合法但不是本程序写的前缀 → 也不许碰
	foreign := mk("ximeclip-2020-01-01"+logFileSuffix, -1)

	log.Cleanup()

	if !fileExists(fresh) {
		t.Error("保留期内的日志不该被删")
	}
	if fileExists(old) {
		t.Error("过期的日志应被清理")
	}
	if !fileExists(odd) {
		t.Error("前缀像日志但日期不合法，不该被删")
	}
	if !fileExists(foreign) {
		t.Error("不是本程序写的前缀，不该被删")
	}
}

// 从 v1.2 升级：旧配置只有 maxTextBytes，升级后应拿到新的默认值而不是 0。
// 这条很关键——如果升级后 MaxTextChars 变成 0，长度限制就静默失效了。
func TestConfigUpgradeFromBytesLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"davUrl":"https://host/dav/","remotePath":"xime","username":"u",
	         "enabled":true,"maxTextBytes":1048576,"logToFile":true,"pollSeconds":15}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读取旧配置失败：%v", err)
	}
	if cfg.MaxTextChars != 300 {
		t.Errorf("升级后文本长度上限应为默认值 300，实际 %d", cfg.MaxTextChars)
	}
	if cfg.LogRetainDays != 7 {
		t.Errorf("升级后日志保留天数应为 7，实际 %d", cfg.LogRetainDays)
	}
	// 旧配置里已有的值必须原样保留
	if cfg.DavURL != "https://host/dav/" || cfg.Username != "u" || !cfg.Enabled {
		t.Errorf("已有配置项被破坏：%+v", cfg)
	}
}
