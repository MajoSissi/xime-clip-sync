package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 端口是一个普通配置项：默认 defaultUIPort，非法值一律退回默认。
//
// 早期版本把 0 当作「还没挑端口」的占位值，启动时再随机挑一个写进配置——
// 那样地址每次都可能变，用户存的书签对不上。现在配置里存的永远是具体端口。
func TestUIPortNormalize(t *testing.T) {
	if got := defaultConfig().UIPort; got != defaultUIPort {
		t.Errorf("默认端口应为 %d，实际 %d", defaultUIPort, got)
	}

	// 合法端口原样保留（用户填的不许动）
	for _, ok := range []int{31213, 8899, 1, 65535} {
		c := Config{UIPort: ok}
		c.normalize()
		if c.UIPort != ok {
			t.Errorf("合法端口 %d 应原样保留，实际 %d", ok, c.UIPort)
		}
	}

	// 非法值（0、负数、越界）一律退回默认端口
	for _, bad := range []int{0, -1, 65536, 70000} {
		c := Config{UIPort: bad}
		c.normalize()
		if c.UIPort != defaultUIPort {
			t.Errorf("非法端口 %d 应退回默认 %d，实际 %d", bad, defaultUIPort, c.UIPort)
		}
	}
}

// 用户自己填的端口不能被顺手改掉。
func TestUIPortUserChoiceIsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"davUrl":"https://host/dav/","uiPort":9001}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPort != 9001 {
		t.Errorf("用户指定的端口被改动了：%d", cfg.UIPort)
	}
	if len(warnings) != 0 {
		t.Errorf("用户指定的端口不该产生任何告警：%v", warnings)
	}
}

// 配置里没有 uiPort（老配置、手写的）时用默认端口，并写进配置文件。
func TestLoadConfigUsesDefaultUIPortWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"davUrl":"https://host/dav/"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPort != defaultUIPort {
		t.Fatalf("缺 uiPort 时应补成默认端口 %d，实际 %d", defaultUIPort, cfg.UIPort)
	}
	// 补默认端口是正常行为，不该占用「需要用户注意」的告警通道
	if len(warnings) != 0 {
		t.Errorf("补默认端口不该产生告警：%v", warnings)
	}

	// 关键：必须落盘。端口是用户要在浏览器里敲的地址，只留在内存里，
	// 用户翻 config.json 会以为「这里没配」。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.UIPort != defaultUIPort {
		t.Errorf("默认端口未写回配置文件：uiPort=%d，期望 %d", saved.UIPort, defaultUIPort)
	}

	// 再读一次：端口保持默认，且不再改写
	cfg2, warnings2, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.UIPort != defaultUIPort {
		t.Errorf("第二次读取端口变了：%d", cfg2.UIPort)
	}
	if len(warnings2) != 0 {
		t.Errorf("第二次读取不该有告警：%v", warnings2)
	}
}

// 首次运行：配置文件还不存在，loadConfig 应把它生成出来（顺带把端口存下来）。
func TestLoadConfigGeneratesFileOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("前提不成立：文件不该存在，err=%v", err)
	}

	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("首次运行不该失败：%v", err)
	}
	if cfg.UIPort <= 0 {
		t.Errorf("首次运行应挑到端口，实际 %d", cfg.UIPort)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("首次运行应生成配置文件：%v", err)
	}
	var saved Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("生成的配置无法解析：%v", err)
	}
	if saved.UIPort != cfg.UIPort {
		t.Errorf("生成的配置里端口 = %d，期望 %d", saved.UIPort, cfg.UIPort)
	}
	// 生成的配置必须是「可用的默认值」，不能是空壳
	if saved.PollSeconds != defaultConfig().PollSeconds || !saved.LogToFile {
		t.Errorf("生成的配置缺少默认值：%+v", saved)
	}
}

// 配置不见了、但日志还在 → 必须告警，不能让用户以为设置自己丢了。
func TestLoadConfigWarnsWhenConfigDisappeared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 造出「以前跑过」的痕迹：日志目录里已经有一个日志文件
	logDir := logDirPath(path)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "xime-clip-sync-2026-01-01.log"),
		[]byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstring(warnings, "配置文件不存在") {
		t.Errorf("配置消失时应给出告警，实际：%v", warnings)
	}
	// 告警归告警，配置还是要生成出来（否则下次启动又得重来一遍）
	if cfg.UIPort != defaultUIPort {
		t.Errorf("应照常补上默认端口 %d，实际 %d", defaultUIPort, cfg.UIPort)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("应生成配置文件：%v", err)
	}
}

// 真正的首次运行（目录里干干净净）不该报「配置消失」。
func TestLoadConfigNoWarnOnGenuineFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	_, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstring(warnings, "配置文件不存在") {
		t.Errorf("首次运行不该告警：%v", warnings)
	}
}

// 从配置文件读进来的「远程路径」原样保留，不做任何补全/改写。
//
// 这个字段的语义曾经从「目录」改成过「具体到文件」，当时的做法是识别出「像目录的值」
// 再自动补全成文件路径。现在不再兼容旧语义：用户填什么就是什么
// （`xime` 就是「一个叫 xime 的文件」），要不要带上文件名由用户在界面上自己决定。
func TestLoadConfigKeepsRemotePathAsIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"davUrl":"https://host/dav/","remotePath":"rime/clip/1.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemotePath != "rime/clip/1.json" {
		t.Errorf("远程路径不该被改动，实际 %q", cfg.RemotePath)
	}
	if len(warnings) != 0 {
		t.Errorf("读取配置不该产生告警：%v", warnings)
	}

	// 看起来像目录的值也照样原样保留——不再猜测用户想填哪个文件。
	if err := os.WriteFile(path, []byte(`{"davUrl":"https://host/dav/","remotePath":"Android/xime"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemotePath != "Android/xime" {
		t.Errorf("像目录的值也应原样保留，实际 %q", cfg.RemotePath)
	}
	if got, ok := cfg.FileURL(); !ok || got != "https://host/dav/Android/xime" {
		t.Errorf("FileURL = (%q, %v)，期望按用户填的值原样拼接", got, ok)
	}
	if len(warnings) != 0 {
		t.Errorf("不该为旧语义发任何告警：%v", warnings)
	}
}

// 端口为 defaultUIPort 之外的值时照常监听，并把地址写进 xime-clip-sync.ui。
func TestUIServerStartAndURLFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ui := newUIServer(ctx, okHandler(), NewLogBuffer(10), cfgPath)
	port := mustFreePort(t)
	url, err := ui.Start(port)
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer ui.Close()

	if ui.Port() != port {
		t.Fatalf("应监听 %d，实际 %d", port, ui.Port())
	}
	if url != ui.URL() || !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("地址异常：url=%q ui.URL()=%q", url, ui.URL())
	}
	if !httpReachable(url) {
		t.Errorf("配置界面不可访问：%s", url)
	}

	// 地址文件要写下来，供第二个实例查找
	b, err := os.ReadFile(uiURLFile(cfgPath))
	if err != nil {
		t.Fatalf("地址文件未写入：%v", err)
	}
	if strings.TrimSpace(string(b)) != url {
		t.Errorf("地址文件内容 = %q，期望 %q", strings.TrimSpace(string(b)), url)
	}

	// 关掉之后地址文件应被清理，免得留下指向死端口的记录
	ui.Close()
	if _, err := os.Stat(uiURLFile(cfgPath)); !os.IsNotExist(err) {
		t.Errorf("Close 后地址文件应被删除，err=%v", err)
	}
}

// 改端口要能立刻生效：新端口可用、旧端口释放、地址文件跟着更新。
func TestUIServerRebindSwitchesPort(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ui := newUIServer(ctx, okHandler(), NewLogBuffer(10), cfgPath)
	first, err := ui.Start(mustFreePort(t))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer ui.Close()
	firstPort := ui.Port()

	// 换一个明确指定的端口
	free, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ui.Rebind(free)
	if err != nil {
		t.Fatalf("切换端口失败：%v", err)
	}
	if ui.Port() != free {
		t.Errorf("切换后端口应为 %d，实际 %d", free, ui.Port())
	}
	if !httpReachable(second) {
		t.Errorf("新端口不可访问：%s", second)
	}

	// 旧监听器是延迟关闭的（得让当前的 HTTP 响应先送到浏览器），等它关掉
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && httpReachable(first) {
		time.Sleep(100 * time.Millisecond)
	}
	if httpReachable(first) {
		t.Errorf("旧端口 %d 应被释放，但仍可访问", firstPort)
	}

	// 地址文件要指向新端口
	b, err := os.ReadFile(uiURLFile(cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != second {
		t.Errorf("地址文件 = %q，期望 %q", strings.TrimSpace(string(b)), second)
	}
}

// 切换到一个已被占用的端口必须失败，而且**不能**把正在用的界面弄没。
func TestUIServerRebindKeepsOldPortOnFailure(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ui := newUIServer(ctx, okHandler(), NewLogBuffer(10), filepath.Join(dir, "config.json"))
	url, err := ui.Start(mustFreePort(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ui.Close()
	before := ui.Port()

	// 先占住一个端口，再要求切过去
	taken, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := listenOn(taken)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	// 端口被占着 → Rebind 必须失败，而且界面要留在原端口上
	// （旧监听器只在新监听器起来之后才关，所以失败不会把用户关在门外）。
	if _, err := ui.Rebind(taken); err == nil {
		t.Fatalf("切到被别人占用的端口 %d 不该成功", taken)
	}
	if ui.Port() != before {
		t.Errorf("切换失败后应留在原端口 %d，实际 %d", before, ui.Port())
	}
	if !httpReachable(url) {
		t.Errorf("切换失败后原界面应仍可访问：%s", url)
	}
}

// 第二个实例通过地址文件找到第一个实例的界面。
func TestRunningUIURL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// 没有地址文件、配置里也没有可用端口 → 无从得知，返回空串
	if got := runningUIURL(cfgPath, Config{UIPort: 0}); got != "" {
		t.Errorf("无地址文件且没有端口时应返回空串，实际 %q", got)
	}

	// 没有地址文件 → 退回配置里的端口
	if got := runningUIURL(cfgPath, Config{UIPort: 8899}); got != "http://127.0.0.1:8899/" {
		t.Errorf("应退回配置端口，实际 %q", got)
	}

	// 有地址文件时以它为准（配置里那个端口未必起得来，运行中的实例写下的才准）
	want := "http://127.0.0.1:54321/"
	if err := os.WriteFile(uiURLFile(cfgPath), []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runningUIURL(cfgPath, Config{UIPort: 8899}); got != want {
		t.Errorf("应优先采用地址文件，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------

// okHandler 是一个永远返回 200 的处理器，用于测端口而不牵扯业务逻辑。
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func httpReachable(url string) bool {
	client := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// freePort 借一个「当下没人用」的端口号，测试用。
//
// 生产代码里已经没有「挑端口」这回事了（端口是配置里的固定值），所以这里自己
// 绑 :0 再释放。释放到真正监听之间有个极短窗口，理论上可能被别人抢走，测试里够用。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// mustFreePort 是 freePort 在测试里的直白用法。
func mustFreePort(t *testing.T) int {
	t.Helper()
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// listenOn 占住一个端口（用于构造「端口被占用」的场景）。
func listenOn(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func containsSubstring(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 端口不可用
//
// 端口是配置里的固定值，绑不上就直接报错——不顺延、也不换随机端口。
// 地址是用户要存成书签的东西，程序悄悄换一个比明说「这个端口用不了」更让人困惑。
// 报错会一路传到托盘，告诉他去改 config.json 里的 uiPort 再重启。
// ---------------------------------------------------------------------------

// occupyPort 占住一个空闲端口，返回端口号（用于构造「这个端口被占了」的场景）。
func occupyPort(t *testing.T) int {
	t.Helper()
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listenOn(p)
	if err != nil {
		t.Skipf("端口 %d 占不住，跳过：%v", p, err)
	}
	t.Cleanup(func() { ln.Close() })
	return p
}

// 端口被别的程序占着时，listen 要明确报错。
func TestListenFailsWhenPortTaken(t *testing.T) {
	taken := occupyPort(t)

	u := newUIServer(context.Background(), http.NotFoundHandler(),
		NewLogBuffer(10), filepath.Join(t.TempDir(), "config.json"))

	if ln, err := u.listen(taken); err == nil {
		ln.Close()
		t.Fatalf("端口 %d 已被占用，listen 不该成功", taken)
	}
}

// 端口号本身不合法时同样直接报错。
// 配置里的端口由 normalize 兜住，这里防的是别的调用方直接传进来。
func TestListenRejectsIllegalPort(t *testing.T) {
	u := newUIServer(context.Background(), http.NotFoundHandler(),
		NewLogBuffer(10), filepath.Join(t.TempDir(), "config.json"))

	for _, bad := range []int{0, -1, 70000} {
		if ln, err := u.listen(bad); err == nil {
			ln.Close()
			t.Errorf("端口 %d 不合法，listen 不该成功", bad)
		}
	}
}

// 同一个场景走完整流程：Start 要失败，失败原因要能被识别成「端口冲突」
// （托盘靠这个词把错误翻译成人话），而且不能留下一个指向死端口的地址文件。
func TestStartFailsWhenPortTaken(t *testing.T) {
	taken := occupyPort(t)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	u := newUIServer(context.Background(), http.NotFoundHandler(), NewLogBuffer(10), cfgPath)

	_, err := u.Start(taken)
	if err == nil {
		u.Close()
		t.Fatalf("端口 %d 已被占用，Start 应失败", taken)
	}
	if got := uiFailReasonOf(err); got != "端口冲突" {
		t.Errorf("失败原因应识别成「端口冲突」，实际 %q", got)
	}
	if _, serr := os.Stat(uiURLFile(cfgPath)); !os.IsNotExist(serr) {
		t.Errorf("Start 失败时不该写地址文件，err=%v", serr)
	}
}

// 用户在界面上填了一个用不了的端口时：配置照旧保存（他填的就是他填的），
// 界面不能消失，而且要明确告诉他「端口用不了、界面还在旧地址、改配置文件再重启」。
func TestSaveWithUnavailablePortWarnsAndKeepsUI(t *testing.T) {
	taken := occupyPort(t)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	log := NewLogBuffer(10)
	// 保存配置会往 <配置目录>/logs 写日志，测试结束要释放句柄，
	// 否则 Windows 下 TempDir 清理会因文件被占用而失败。
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	engine := NewSyncEngine(defaultConfig(), &fakeClipboard{}, log)
	srv := NewServer(engine, log, cfgPath)
	ui := newUIServer(context.Background(), srv.Handler(), log, cfgPath)
	srv.ui = ui
	if _, err := ui.Start(mustFreePort(t)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ui.Close)
	before := ui.Port()

	// autostart 回填当前真实状态：否则保存流程会去改注册表，
	// 把用户机器上的自启设置弄成测试的副作用。
	body := fmt.Sprintf(`{"davUrl":"https://host/dav/","remotePath":"xime/clipboard/current.json",
	  "username":"u","password":"","deviceName":"pc","enabled":true,
	  "pollSeconds":30,"localPollSeconds":5,
	  "maxTextChars":1000,"logToFile":false,"logRetainDays":7,
	  "autostart":%v,"uiPort":%d}`, autostartEnabled(), taken)

	resp, err := http.Post(ui.URL()+"/api/config/save", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("换端口不该让保存失败：HTTP %d", resp.StatusCode)
	}

	var got struct {
		OK      bool   `json:"ok"`
		Warning string `json:"warning"`
		UIURL   string `json:"uiUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应无法解析：%v", err)
	}
	if !got.OK {
		t.Error("ok 应为 true")
	}
	if !strings.Contains(got.Warning, "用不了") {
		t.Errorf("应提示端口用不了，实际 warning=%q", got.Warning)
	}
	if !strings.Contains(got.Warning, "uiPort") {
		t.Errorf("提示里要写明改哪个配置项，实际 warning=%q", got.Warning)
	}
	// 切不过去就不该让前端「去新地址」
	if got.UIURL != "" {
		t.Errorf("切换失败时不该返回 uiUrl，实际 %q", got.UIURL)
	}
	if ui.Port() != before {
		t.Errorf("切换失败后界面应留在原端口 %d，实际 %d", before, ui.Port())
	}

	// 界面必须还活着——「改个端口把用户关在门外」是最不能接受的结果
	alive, err := http.Get(ui.URL())
	if err != nil {
		t.Fatalf("界面不该消失：%v", err)
	}
	alive.Body.Close()
	if alive.StatusCode != 200 {
		t.Errorf("界面不该消失：HTTP %d", alive.StatusCode)
	}
}

// warnings 要能装下多条、按发生顺序拼起来，空的时候是空串
// （调用方靠空串判断「要不要带 warning 字段」）。
func TestWarningsCollectAllMessages(t *testing.T) {
	var w warnings
	if got := w.String(); got != "" {
		t.Errorf("没加过任何提示时应为空串，实际 %q", got)
	}

	w.add("第一件：%v", "甲")
	w.add("第二件：%d", 2)
	w.add("第三件：%s", "丙")

	got := w.String()
	for _, want := range []string{"第一件：甲", "第二件：2", "第三件：丙"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示 %q 被吞了，实际 %q", want, got)
		}
	}
	if i, j := strings.Index(got, "第一件"), strings.Index(got, "第二件"); i > j {
		t.Errorf("提示应按发生顺序排列，实际 %q", got)
	}
}

// 端口切换后整页会被重绘成「去新地址」的引导页，warning 必须跟着一起显示。
//
// 换端口最常见的原因就是「填的端口用不了」，而这正是用户此刻最需要知道的一句话：
// 本页的端口马上就关了，他看不到任何别的反馈。早先这里直接 return，
// 提示被吞掉，用户只看到地址变了、不知道为什么变。
//
// 断言的是「页面里有 esc(r.warning) 这个拼接」而不是具体文案：
// 前端是内嵌的单文件，按语义留个哨兵比比对整段 HTML 稳。
func TestSwitchedPageKeepsWarning(t *testing.T) {
	html := string(indexHTML)
	if !strings.Contains(html, "esc(r.warning)") {
		t.Error("切端口后的引导页必须带上 warning——换端口的原因不能只写在日志里")
	}
	// 拼进 innerHTML 的字符串都要过 esc()，系统错误信息里可能带 < > &
	if !strings.Contains(html, "esc(r.uiUrl)") {
		t.Error("uiUrl 拼进 innerHTML 前应转义")
	}
}

// 这是「失败必须有回声」的直接推论：用户点一下保存，程序背地里做了三件事
// （写自启项、切日志文件、重绑界面端口），任何一件没成，用户都必须看得见。
// 早先的实现是 `if warning == "" { warning = ... }`，只留第一条——
// 第二条被静默丢掉，用户以为那件事也做成了。
//
// 两件事都能稳定触发：
//   - 日志：在配置目录里放一个叫 logs 的**普通文件**，日志目录就建不出来
//     （不依赖权限位，Windows 上照样稳定复现）
//   - 端口：占掉他要填的那个端口，Rebind 直接失败
func TestSaveReportsEveryWarningNotJustFirst(t *testing.T) {
	base := occupyPort(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(filepath.Join(dir, "logs"), []byte("占位，让 logs 目录建不出来"), 0o644); err != nil {
		t.Fatal(err)
	}

	log := NewLogBuffer(10)
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	engine := NewSyncEngine(defaultConfig(), &fakeClipboard{}, log)
	srv := NewServer(engine, log, cfgPath)
	ui := newUIServer(context.Background(), srv.Handler(), log, cfgPath)
	srv.ui = ui
	if _, err := ui.Start(mustFreePort(t)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ui.Close)

	// autostart 回填当前真实状态，避免测试去改用户机器上的注册表。
	body := fmt.Sprintf(`{"davUrl":"https://host/dav/","remotePath":"xime/clipboard/current.json",
	  "username":"u","password":"","deviceName":"pc","enabled":true,
	  "pollSeconds":30,"localPollSeconds":5,
	  "maxTextChars":1000,"logToFile":true,"logRetainDays":7,
	  "autostart":%v,"uiPort":%d}`, autostartEnabled(), base)

	resp, err := http.Post(ui.URL()+"/api/config/save", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("附带动作失败不该让保存整体失败：HTTP %d", resp.StatusCode)
	}

	var got struct {
		OK      bool   `json:"ok"`
		Warning string `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应无法解析：%v", err)
	}
	if !got.OK {
		t.Error("ok 应为 true")
	}
	if !strings.Contains(got.Warning, "日志") {
		t.Errorf("日志落盘那条提示丢了，实际 warning=%q", got.Warning)
	}
	if !strings.Contains(got.Warning, "用不了") {
		t.Errorf("端口那条提示被前一条挤掉了，实际 warning=%q", got.Warning)
	}
}

// Failed 只认「意外退出」，正常关闭不算。
//
// uiStatus 靠它区分「在跑」和「起过又没了」：监听器已经关了、地址还写着，
// 照报一个访问不通的地址比不报还误导——用户会以为是浏览器的问题。
func TestUIServerFailedOnlyOnUnexpectedExit(t *testing.T) {
	u := newUIServer(context.Background(), http.NotFoundHandler(),
		NewLogBuffer(10), filepath.Join(t.TempDir(), "config.json"))
	if _, err := u.Start(mustFreePort(t)); err != nil {
		t.Fatal(err)
	}
	if u.Failed() {
		t.Error("刚启动的服务不该被标记为失败")
	}

	// 换端口会关掉旧监听器（延迟 600ms 关，让当前响应先送到浏览器）。
	// 旧监听器收到的是「连接已关闭」，绝不能因此把**新**监听器标成失败——
	// 用户改一次端口、托盘菜单就永久显示「配置界面 - 已停止」是最糟的结果。
	if _, err := u.Rebind(mustFreePort(t)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond) // 等旧监听器真的被关掉
	if u.Failed() {
		t.Error("换端口（旧监听器被主动关掉）不该被标记为失败")
	}
	if u.URL() == "" {
		t.Error("换端口后应该还在跑")
	}

	u.Close()
	// Close 关监听器是同步的，但 Serve 的返回值要等 goroutine 跑完。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if u.Failed() {
			t.Fatal("正常关闭不该被标记为失败")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 每个配置字段都必须在 web/index.html 的 FIELDS 数组里。
//
// 漏一个的后果是**静默**的：readForm 按 FIELDS 取值，漏掉的字段不会提交，
// 服务端收到零值 → normalize 回落到默认 → 用户每保存一次就丢一次自己填的内容，
// 而且界面上看不出任何异常。
//
// 只管这一个方向。反过来（FIELDS 里有、页面上没有对应输入框）不会丢数据：
// fillForm/readForm 都是 `if (!el) continue`，只是那个字段改不了而已。
func TestEveryConfigFieldIsInTheForm(t *testing.T) {
	// 不参与界面表单的字段：passwordEnc 只由服务端从明文现算，
	// 界面既不给看也不给改（不给浏览器往配置里塞任意 blob 的机会）。
	skip := map[string]bool{"passwordEnc": true}

	html := string(indexHTML)
	// 只看 FIELDS 数组本身。字段名也会以 id="..." 的形式出现在 HTML 别处，
	// 搜整份文件的话这个测试永远通过（第一版就是这么写的，等于没测）。
	start := strings.Index(html, "const FIELDS = [")
	if start < 0 {
		t.Fatal("web/index.html 里找不到 FIELDS 数组")
	}
	end := strings.Index(html[start:], "];")
	if end < 0 {
		t.Fatal("web/index.html 里 FIELDS 数组没有正常结束")
	}
	fields := html[start : start+end]

	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" || skip[name] {
			continue
		}
		if !strings.Contains(fields, `"`+name+`"`) {
			t.Errorf("配置字段 %s（json:%q）不在 web/index.html 的 FIELDS 里，"+
				"界面保存时会把这一项清空", f.Name, name)
		}
	}
}

