package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 假 WebDAV 服务

// fakeWebDAV 是一个最小可用的 WebDAV 服务，用于端到端验证同步逻辑。
type fakeWebDAV struct {
	mu       sync.Mutex
	dirs     map[string]bool
	files    map[string][]byte
	etags    map[string]string
	etagSeq  int
	putCount int
	getCount int
	mkcol    int
	// 期望的 Basic 凭据（user:pass），为空表示不校验
	wantAuth string
	// 返回 503 限流
	rateLimit bool
}

func newFakeWebDAV() *fakeWebDAV {
	return &fakeWebDAV{
		dirs:  map[string]bool{"/dav": true},
		files: map[string][]byte{},
		etags: map[string]string{},
	}
}

func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func (f *fakeWebDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.wantAuth != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user+":"+pass != f.wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	path := r.URL.Path
	switch r.Method {
	case "MKCOL":
		f.mkcol++
		if f.dirs[path] {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !f.dirs[parentDir(path)] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.dirs[path] = true
		w.WriteHeader(http.StatusCreated)

	case http.MethodPut:
		if f.rateLimit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !f.dirs[parentDir(path)] {
			// 父目录不存在：部分服务器返回 409，部分返回 404
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.etagSeq++
		et := fmt.Sprintf("%q", fmt.Sprintf("v%d", f.etagSeq))
		f.files[path] = body
		f.etags[path] = et
		f.putCount++
		w.Header().Set("ETag", et)
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet:
		if f.rateLimit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, ok := f.files[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.getCount++
		et := f.etags[path]
		if r.Header.Get("If-None-Match") == et {
			w.Header().Set("ETag", et)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", et)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)

	case "PROPFIND":
		if f.rateLimit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(207)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeWebDAV) file(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	return string(b), ok
}

func (f *fakeWebDAV) setFile(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etagSeq++
	f.files[path] = []byte(content)
	f.etags[path] = fmt.Sprintf("%q", fmt.Sprintf("v%d", f.etagSeq))
}

func (f *fakeWebDAV) counts() (put, get int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCount, f.getCount
}

// ---------------------------------------------------------------- 假剪贴板

type fakeClipboard struct {
	mu       sync.Mutex
	text     string
	seq      uint32
	setCount int
}

func (c *fakeClipboard) GetText() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text, nil
}

func (c *fakeClipboard) SetText(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = text
	c.seq++
	c.setCount++
	return nil
}

func (c *fakeClipboard) Seq() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq, true
}

// sets 返回写入剪贴板的次数。
func (c *fakeClipboard) sets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setCount
}

// set 模拟用户复制：内容变化并推进序号。
func (c *fakeClipboard) set(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = text
	c.seq++
}

func (c *fakeClipboard) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// ---------------------------------------------------------------- 测试

const remoteFilePath = "/dav/xime/clipboard/current.json"

func testConfig(serverURL string) Config {
	cfg := defaultConfig()
	cfg.DavURL = serverURL + "/dav/"
	// 「远程路径」现在具体到文件；这里显式写出，和上面 remoteFilePath 对齐。
	cfg.RemotePath = "xime/clipboard/current.json"
	cfg.DeviceName = "test-pc"
	cfg.Enabled = true
	return cfg
}

func newTestEngine(t *testing.T, srv *httptest.Server, clip *fakeClipboard) *SyncEngine {
	t.Helper()
	return NewSyncEngine(testConfig(srv.URL), clip, NewLogBuffer(100))
}

// newTestEngineWithLog 同 newTestEngine，但把日志缓冲一并返回——
// 要断言「日志里到底写了什么」的测试需要它。
func newTestEngineWithLog(t *testing.T, srv *httptest.Server, clip *fakeClipboard) (*SyncEngine, *LogBuffer) {
	t.Helper()
	lb := NewLogBuffer(100)
	return NewSyncEngine(testConfig(srv.URL), clip, lb), lb
}

// buildFileURL 是纯字符串拼接：remotePath 原样接到 davUrl 后面，不再自动补文件名。
//
// 第十一轮改的语义：「远程路径」现在**具体到文件**（如 rime/clip/1.json），
// 所以这里不能再追加 clipboard/current.json —— 否则用户填 xime 会被当成
// 「xime 目录」而不是「xime 文件」。留空 = 服务器根，默认文件由配置层给。
func TestBuildFileURL(t *testing.T) {
	cases := []struct {
		dav, path, want string
		ok              bool
	}{
		{"https://dav.jianguoyun.com/dav/", "xime", "https://dav.jianguoyun.com/dav/xime", true},
		{"https://dav.jianguoyun.com/dav", "xime", "https://dav.jianguoyun.com/dav/xime", true},
		{"https://host:8080/dav/", "rime/clip/1.json", "https://host:8080/dav/rime/clip/1.json", true},
		{"https://host:8080/dav/", "", "https://host:8080/dav", true},
		{"https://host:8080/dav/", "/xime/", "https://host:8080/dav/xime", true},
		{"https://host:8080/dav///", "a/b", "https://host:8080/dav/a/b", true},
		{"", "xime", "", false},
	}
	for _, c := range cases {
		got, ok := buildFileURL(c.dav, c.path)
		if ok != c.ok || got != c.want {
			t.Errorf("buildFileURL(%q, %q) = (%q, %v)，期望 (%q, %v)", c.dav, c.path, got, ok, c.want, c.ok)
		}
	}
}

// 界面上的「远程路径」留空时，落到默认文件 xime/clipboard/current.json，
// 而**不是**服务器根目录。
//
// 这条策略只在配置层（RemotePathOrDefault / FileURL）。
// buildFileURL 本身仍保持「留空 = 根目录」的纯语义 —— 见上面 TestBuildFileURL。
func TestRemotePathDefaultsToXimeWhenBlank(t *testing.T) {
	const dav = "https://dav.jianguoyun.com/dav/"
	const want = "https://dav.jianguoyun.com/dav/xime/clipboard/current.json"

	// 空、纯空白、只有斜杠 —— 都算「没配」
	for _, blank := range []string{"", "   ", "/", "//", "  /  "} {
		cfg := defaultConfig()
		cfg.DavURL = dav
		cfg.RemotePath = blank

		if got := cfg.RemotePathOrDefault(); got != DefaultRemotePath {
			t.Errorf("RemotePath=%q 时 RemotePathOrDefault = %q，期望 %q", blank, got, DefaultRemotePath)
		}
		got, ok := cfg.FileURL()
		if !ok || got != want {
			t.Errorf("RemotePath=%q 时 FileURL = (%q, %v)，期望 (%q, true)", blank, got, ok, want)
		}
	}
}

// 填了具体路径就按填的来（只做首尾斜杠归一化），**不再自动追加文件名**。
//
// 这是第十一轮的关键行为：用户填什么就是什么，包括看起来像目录的值
// （如 xime / Android/xime）—— 它会被当成「叫这个名字的文件」。
func TestRemotePathUserValueIsKept(t *testing.T) {
	cfg := defaultConfig()
	cfg.DavURL = "https://host/dav/"
	cases := []struct{ in, want string }{
		{"rime/clip/1.json", "https://host/dav/rime/clip/1.json"},
		{"/rime/clip/1.json/", "https://host/dav/rime/clip/1.json"},
		{"xime", "https://host/dav/xime"},
		{"/xime/", "https://host/dav/xime"},
		{"Android/xime", "https://host/dav/Android/xime"},
	}
	for _, c := range cases {
		cfg.RemotePath = c.in
		got, ok := cfg.FileURL()
		if !ok || got != c.want {
			t.Errorf("RemotePath=%q 时 FileURL = (%q, %v)，期望 (%q, true)", c.in, got, ok, c.want)
		}
	}
}

// 「留空」不能被 normalize 写回成默认路径：配置里保持用户原样，只在用的时候解析。
//
// 否则用户留空、一保存，输入框里就被填上默认路径，之后分不清哪些是自己填的、哪些是程序塞的。
func TestRemotePathBlankIsNotWrittenBack(t *testing.T) {
	cfg := defaultConfig()
	cfg.DavURL = "https://host/dav/"
	cfg.RemotePath = ""
	cfg.normalize()

	if cfg.RemotePath != "" {
		t.Errorf("normalize 把留空的 RemotePath 改成了 %q，配置里应保持留空", cfg.RemotePath)
	}
	if got := cfg.RemotePathOrDefault(); got != DefaultRemotePath {
		t.Errorf("RemotePathOrDefault = %q，期望 %q", got, DefaultRemotePath)
	}
}

// 真正发请求的那个客户端也必须走默认值 —— 防止哪天有人绕开 RemotePathOrDefault
// 直接拿 cfg.RemotePath 去拼 URL（那样留空就会悄悄同步到服务器根目录去）。
func TestWebDAVClientUsesDefaultRemotePath(t *testing.T) {
	cfg := defaultConfig()
	cfg.DavURL = "https://host/dav/"
	cfg.RemotePath = ""

	got, ok := newWebDAVClient(cfg).fileURL()
	if !ok || got != "https://host/dav/xime/clipboard/current.json" {
		t.Errorf("webdavClient.fileURL() = (%q, %v)，留空时应落到默认文件", got, ok)
	}
}

func TestBuildDirURL(t *testing.T) {
	got := buildDirURL("https://host/dav/xime/clipboard/current.json")
	if got != "https://host/dav/xime/clipboard" {
		t.Errorf("buildDirURL = %q", got)
	}
}

func TestRelativeDirParts(t *testing.T) {
	got := relativeDirParts(
		"https://host/dav/",
		"https://host/dav/xime/clipboard/current.json",
	)
	want := []string{"xime", "clipboard"}
	if len(got) != len(want) {
		t.Fatalf("relativeDirParts = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("relativeDirParts = %v，期望 %v", got, want)
		}
	}
	// 根目录：不产生任何层级
	if p := relativeDirParts("https://host/dav/", "https://host/dav/clipboard/current.json"); len(p) != 1 || p[0] != "clipboard" {
		t.Errorf("根目录场景 relativeDirParts = %v", p)
	}
	// 用户自定义的嵌套文件路径：目录层级完全由他填的内容决定，要逐级建出来
	if p := relativeDirParts("https://host/dav/", "https://host/dav/rime/clip/1.json"); len(p) != 2 ||
		p[0] != "rime" || p[1] != "clip" {
		t.Errorf("自定义嵌套路径 relativeDirParts = %v，期望 [rime clip]", p)
	}
	// 直接放在服务器根下的文件：没有任何目录要建
	if p := relativeDirParts("https://host/dav/", "https://host/dav/1.json"); len(p) != 0 {
		t.Errorf("无目录场景 relativeDirParts = %v，期望空", p)
	}
}

// wire 格式必须是 snake_case，且与插件 JSON.stringify 的字段一致
func TestProfileWireFormat(t *testing.T) {
	p := newTextProfile("你好 hello", "my-pc")
	data, err := p.encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "hash", "text", "has_data", "data_name", "size", "source"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少字段 %q，实际 JSON：%s", k, data)
		}
	}
	if m["size"].(float64) != float64(len("你好 hello")) {
		t.Errorf("size 应为 UTF-8 字节长度 %d，实际 %v", len("你好 hello"), m["size"])
	}
	if m["type"] != "text" || m["has_data"] != false {
		t.Errorf("type/has_data 不符：%s", data)
	}
	if m["hash"] != hashText("你好 hello") {
		t.Errorf("hash 不符：%v", m["hash"])
	}
}

// 远端为旧版纯文本时应能兼容解析
func TestDecodeProfile(t *testing.T) {
	prof, plain, ok := decodeProfile([]byte(`{"type":"text","hash":"h","text":"hello","has_data":false,"data_name":null,"size":5,"source":"phone"}`))
	if !ok || plain {
		t.Fatalf("JSON Profile 解析失败：ok=%v plain=%v", ok, plain)
	}
	if prof.Text != "hello" || prof.Size != 5 || prof.Source == nil || *prof.Source != "phone" {
		t.Errorf("解析结果不符：%+v", prof)
	}

	prof, plain, ok = decodeProfile([]byte("just plain text"))
	if !ok || !plain {
		t.Fatalf("纯文本兼容失败：ok=%v plain=%v", ok, plain)
	}
	if prof.Text != "just plain text" || prof.Size != len("just plain text") {
		t.Errorf("纯文本解析结果不符：%+v", prof)
	}

	if _, _, ok := decodeProfile([]byte("   ")); ok {
		t.Errorf("空白内容应视为不可用")
	}
}

// 「远端原始内容」要按固定字段顺序输出，text 只留首尾各 5 个字符。
func TestRemoteRawText(t *testing.T) {
	src, name := "phone", "a.png"
	got := remoteRawText(Profile{
		Type:     "text",
		Hash:     "abc123",
		Text:     "0123456789ABCDEFGHIJ", // 20 字符 → 首 5 + 尾 5
		HasData:  true,
		DataName: &name,
		Size:     20,
		Source:   &src,
	})
	want := "type: text\n" +
		"size: 20\n" +
		"hash: abc123\n" +
		"has_data: true\n" +
		"data_name: a.png\n" +
		"source: phone\n" +
		"text: 01234…FGHIJ"
	if got != want {
		t.Errorf("remoteRawText 输出不符\n实际：\n%s\n期望：\n%s", got, want)
	}
}

// data_name / source 为空时输出空值，不能冒出 "null" / "<nil>" 之类的东西。
func TestRemoteRawTextNilFields(t *testing.T) {
	got := remoteRawText(Profile{Type: "text", Text: "短"})
	want := "type: text\nsize: 0\nhash: \nhas_data: false\ndata_name: \nsource: \ntext: 短"
	if got != want {
		t.Errorf("remoteRawText 输出不符\n实际：%q\n期望：%q", got, want)
	}
}

// 首尾截断按**字符**算：中文和 emoji 都不能被劈成半个。
func TestHeadTail(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"短", 5, "短"},
		{"0123456789", 5, "0123456789"},   // 正好 2n，不截
		{"01234567890", 5, "01234…67890"}, // 多一个字符就截
		{"你好世界一二三四五六七", 5, "你好世界一…三四五六七"},
		{"😀😀😀😀😀😀😀😀😀😀😀", 5, "😀😀😀😀😀…😀😀😀😀😀"},
	}
	for _, c := range cases {
		if got := headTail(c.in, c.n); got != c.want {
			t.Errorf("headTail(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
		}
	}
}

// 界面上「本地内容」「远端内容」超过 150 字符就截断并补省略号。
func TestStatusPreviewTruncatesAt150(t *testing.T) {
	if previewLen != 150 {
		t.Fatalf("previewLen = %d，期望 150", previewLen)
	}
	got := preview(strings.Repeat("字", 400), previewLen)
	if n := runeCount(got); n != previewLen+1 {
		t.Errorf("截断后长度 = %d 字符，期望 %d（150 字符 + 省略号）", n, previewLen+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应以省略号结尾：%q", got)
	}
	// 没超长的一字不动
	if short := preview("短内容", previewLen); short != "短内容" {
		t.Errorf("短内容不该被改动：%q", short)
	}
}

// PUT 遇到父目录缺失时，应自动 MKCOL 后重试成功
func TestPutCreatesDirectories(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	if err := engine.PushNow(); err == nil {
		t.Fatal("空剪贴板推送应返回错误")
	}

	clip.set("hello webdav")
	if err := engine.PushNow(); err != nil {
		t.Fatalf("推送失败：%v", err)
	}

	fake := srv.Config.Handler.(*fakeWebDAV)
	content, ok := fake.file(remoteFilePath)
	if !ok {
		t.Fatalf("远端文件未创建，已有：%v", fake.files)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("远端内容不是合法 JSON：%s", content)
	}
	if m["text"] != "hello webdav" {
		t.Errorf("远端 text 不符：%v", m["text"])
	}
	if m["source"] != "test-pc" {
		t.Errorf("远端 source 不符：%v", m["source"])
	}
}

// Basic Auth 必须按插件语义发送
func TestBasicAuth(t *testing.T) {
	fake := newFakeWebDAV()
	fake.wantAuth = "user:pass"
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Username = "user"
	cfg.Password = "pass"
	client := newWebDAVClient(cfg)

	if err := client.testConnection(); err != nil {
		t.Fatalf("带认证的连接测试失败：%v", err)
	}

	// 错误密码应报认证失败
	bad := cfg
	bad.Password = "wrong"
	if err := newWebDAVClient(bad).testConnection(); err == nil ||
		!strings.Contains(err.Error(), "认证失败") {
		t.Errorf("错误密码应返回认证失败，实际：%v", err)
	}
}

// 本地剪贴板变化 → 推送；远端变化 → 应用；且不产生回声循环
func TestSyncLocalToRemoteAndBack(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	// 建立基线（远端为空）
	engine.syncCycle(true)
	if put, _ := fake.counts(); put != 0 {
		t.Fatalf("空剪贴板不应推送，实际推送 %d 次", put)
	}

	// 1) 本地复制 → 推送到远端
	clip.set("来自电脑的内容")
	engine.checkLocalOnly()
	content, ok := fake.file(remoteFilePath)
	if !ok {
		t.Fatal("本地内容未推送到远端")
	}
	if !strings.Contains(content, "来自电脑的内容") {
		t.Errorf("远端内容不符：%s", content)
	}

	// 2) 连续多轮同步不应重复推送（回声抑制）
	putBefore, _ := fake.counts()
	for i := 0; i < 5; i++ {
		engine.checkLocalOnly()
	}
	if putAfter, _ := fake.counts(); putAfter != putBefore {
		t.Errorf("内容未变化却重复推送：%d → %d", putBefore, putAfter)
	}

	// 3) 远端被其他设备改写 → 应用到本地
	fake.setFile(remoteFilePath, `{"type":"text","hash":"x","text":"来自手机的内容","has_data":false,"data_name":null,"size":21,"source":"phone"}`)
	engine.syncCycle(true)
	if got := clip.get(); got != "来自手机的内容" {
		t.Errorf("远端内容未应用到本地，实际：%q", got)
	}

	// 4) 应用远端后不得再推回去（否则两端会死循环）
	putBefore, _ = fake.counts()
	for i := 0; i < 5; i++ {
		engine.checkLocalOnly()
	}
	if putAfter, _ := fake.counts(); putAfter != putBefore {
		t.Errorf("应用远端后出现回声推送：%d → %d", putBefore, putAfter)
	}
}

// 「推送 / 拉取」那两行日志必须**短**，用箭头 + 动词开头。
//
// 这是全程序最高频的一条日志（每次内容变化都写一行），它直接决定日志文件的增长速度，
// 所以文案要压到最短。同时钉住「别再退回旧的长句子」——
// 旧文案是「已推送本地剪贴板到远端（N 字符）」，长度翻倍。
func TestSyncLogLinesAreShort(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine, lb := newTestEngineWithLog(t, srv, clip)

	// 建立基线（远端为空，不推送）
	engine.syncCycle(true)

	// 推一次
	clip.set("来自电脑的内容")
	engine.checkLocalOnly()

	// 拉一次（带上来源设备）
	fake.setFile(remoteFilePath, `{"type":"text","hash":"x","text":"来自手机的内容","has_data":false,"data_name":null,"size":21,"source":"phone"}`)
	engine.syncCycle(true)

	all := lb.Lines(0)
	var pushLine, pullLine string
	for _, l := range all {
		if strings.HasPrefix(l.Text, "⬆") {
			pushLine = l.Text
		}
		if strings.HasPrefix(l.Text, "⬇") {
			pullLine = l.Text
		}
	}
	if pushLine == "" {
		t.Fatalf("没找到推送日志，实际日志：%+v", all)
	}
	if pullLine == "" {
		t.Fatalf("没找到拉取日志，实际日志：%+v", all)
	}

	// 信息不能丢：动词 + 字符数；拉取还要带上来源设备（多设备时靠它区分是谁推的）
	for name, line := range map[string]string{"推送": pushLine, "拉取": pullLine} {
		if !strings.Contains(line, name) || !strings.Contains(line, "字符") {
			t.Errorf("%s日志缺少动词或字符数：%q", name, line)
		}
	}
	if !strings.Contains(pullLine, "phone") {
		t.Errorf("拉取日志应带上来源设备：%q", pullLine)
	}

	// 反向哨兵：不许退回旧的长文案
	for _, old := range []string{"已推送本地剪贴板到远端", "已拉取远端剪贴板到本地"} {
		for _, l := range all {
			if strings.Contains(l.Text, old) {
				t.Errorf("日志里又出现了旧的长文案 %q：%q", old, l.Text)
			}
		}
	}

	// 高频行必须短（箭头 + 动词 + 字符数 + 可选的来源设备）
	for _, line := range []string{pushLine, pullLine} {
		if n := len([]rune(line)); n > 24 {
			t.Errorf("这条日志每次同步都写，太长了（%d 字）：%q", n, line)
		}
	}
}

// ETag 命中 304 时不应重复应用内容
func TestETagNotModified(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	fake.setFile(remoteFilePath, `{"type":"text","text":"remote-1","size":8}`)
	engine.syncCycle(true)
	if clip.get() != "remote-1" {
		t.Fatalf("首次拉取失败：%q", clip.get())
	}
	first := clip.sets()
	if first == 0 {
		t.Fatal("首次拉取应写入剪贴板")
	}

	// 远端未变化：应命中 304，不再写入剪贴板
	engine.pullRemote(engine.Config(), false)
	if clip.sets() != first {
		t.Errorf("ETag 未生效，304 后又写入了剪贴板（%d → %d）", first, clip.sets())
	}

	// 远端变化后应重新应用
	fake.setFile(remoteFilePath, `{"type":"text","text":"remote-2","size":8}`)
	engine.pullRemote(engine.Config(), false)
	if clip.get() != "remote-2" {
		t.Errorf("远端变更后未应用：%q", clip.get())
	}
}

// 503 限流应进入退避并跳过后续请求
func TestRateLimitBackoff(t *testing.T) {
	fake := newFakeWebDAV()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	clip := &fakeClipboard{}
	clip.set("hello")
	engine := newTestEngine(t, srv, clip)

	fake.mu.Lock()
	fake.rateLimit = true
	fake.mu.Unlock()

	engine.syncCycle(true)
	if !engine.inBackoff() {
		t.Fatal("遇到 503 应进入退避")
	}

	// 退避期间不再发起请求
	putBefore, getBefore := fake.counts()
	for i := 0; i < 5; i++ {
		engine.syncCycle(false)
	}
	putAfter, getAfter := fake.counts()
	if putAfter != putBefore || getAfter != getBefore {
		t.Errorf("退避期间仍在请求：put %d→%d, get %d→%d", putBefore, putAfter, getBefore, getAfter)
	}
	if st := engine.Status(); st.State != "backoff" {
		t.Errorf("状态应为 backoff，实际 %s", st.State)
	}
}

// 配置界面路由可用
func TestUIServer(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	ui := httptest.NewServer(NewServer(engine, NewLogBuffer(10), "config.json").Handler())
	defer ui.Close()

	// 首页
	resp, err := http.Get(ui.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Xime Clip Sync") {
		t.Errorf("首页异常：%d", resp.StatusCode)
	}

	// 状态接口
	resp, err = http.Get(ui.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if !st.Configured || st.DeviceName != "test-pc" {
		t.Errorf("状态接口异常：%+v", st)
	}
}

// 大文本与多字节内容应完整往返
func TestUnicodeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	engine.syncCycle(true)

	text := "中文🙂emoji\tTab\n换行\r\n结束"
	clip.set(text)
	engine.checkLocalOnly()

	content, _ := fake.file(remoteFilePath)
	var m map[string]any
	json.Unmarshal([]byte(content), &m)
	if m["text"] != text {
		t.Errorf("多字节内容往返不一致：%q", m["text"])
	}
	if int(m["size"].(float64)) != len(text) {
		t.Errorf("size 应为 %d，实际 %v", len(text), m["size"])
	}
}

// 配置默认值与归一化
func TestConfigNormalize(t *testing.T) {
	d := defaultConfig()
	// 默认值本身就是需求的一部分，单独钉住
	if d.MaxTextChars != 300 {
		t.Errorf("自动推送文本长度上限默认值应为 300，实际 %d", d.MaxTextChars)
	}
	if d.LogRetainDays != 7 {
		t.Errorf("日志保留天数默认值应为 7，实际 %d", d.LogRetainDays)
	}
	// 本地检查间隔的单位是**秒**（2026-09-28 从毫秒改成秒，默认 5）
	if d.LocalPollSeconds != 5 {
		t.Errorf("本地检查间隔默认值应为 5 秒，实际 %d", d.LocalPollSeconds)
	}
	// 手动推送（托盘/界面「推送」按钮）另有更宽的上限，默认 10000
	if d.ManualMaxTextChars != 10000 {
		t.Errorf("手动推送上限默认值应为 10000，实际 %d", d.ManualMaxTextChars)
	}
	if d.ManualMaxTextChars <= d.MaxTextChars {
		t.Errorf("手动推送上限（%d）应比自动同步的自动推送文本长度上限（%d）宽，否则这个配置项没意义",
			d.ManualMaxTextChars, d.MaxTextChars)
	}
	if !d.Enabled || !d.LogToFile {
		t.Errorf("同步与日志落盘默认应为开启：%+v", d)
	}

	c := Config{}
	c.normalize()
	if c.PollSeconds != d.PollSeconds || c.LocalPollSeconds != d.LocalPollSeconds {
		t.Errorf("默认值异常：%+v", c)
	}
	c.PollSeconds = 1
	c.normalize()
	if c.PollSeconds < 3 {
		t.Errorf("归一化失败：%+v", c)
	}

	// 本地检查间隔：低于 1 秒退回默认（0 会让轮询变成忙等），高于 60 秒封顶
	c.LocalPollSeconds = 0
	c.normalize()
	if c.LocalPollSeconds != d.LocalPollSeconds {
		t.Errorf("本地检查间隔 0 应退回默认 %d，实际 %d", d.LocalPollSeconds, c.LocalPollSeconds)
	}
	c.LocalPollSeconds = -7
	c.normalize()
	if c.LocalPollSeconds != d.LocalPollSeconds {
		t.Errorf("本地检查间隔负数应退回默认 %d，实际 %d", d.LocalPollSeconds, c.LocalPollSeconds)
	}
	c.LocalPollSeconds = 999
	c.normalize()
	if c.LocalPollSeconds != 60 {
		t.Errorf("本地检查间隔上限应为 60 秒，实际 %d", c.LocalPollSeconds)
	}

	// 负数应归零（0 表示不限制 / 永久保留），保留天数还要封顶
	c.MaxTextChars = -5
	c.ManualMaxTextChars = -5
	c.LogRetainDays = -3
	c.normalize()
	if c.MaxTextChars != 0 || c.ManualMaxTextChars != 0 || c.LogRetainDays != 0 {
		t.Errorf("负数应归零：%+v", c)
	}
	c.LogRetainDays = 9999
	c.normalize()
	if c.LogRetainDays != 365 {
		t.Errorf("日志保留天数上限应为 365，实际 %d", c.LogRetainDays)
	}

	// 首尾空白应被清理，避免把 "   " 误当成合法服务器地址
	c.DavURL = "  https://host/dav/  "
	c.RemotePath = " xime "
	c.normalize()
	if c.DavURL != "https://host/dav/" || c.RemotePath != "xime" {
		t.Errorf("空白清理失败：%+v", c)
	}
}

// 「本地检查间隔」的单位是**秒**（2026-09-28 从毫秒改成秒，默认 5）。
//
// 这条守住的是「只改了界面文案和字段名、忘了改换算倍数」——那种情况下配置里存的是 5，
// 实际却按 5 **毫秒**轮询：剪贴板被高频读取，CPU 白转，而界面上一点异常都看不出来。
func TestLocalPollIntervalIsSeconds(t *testing.T) {
	cfg := testConfig("")
	cfg.LocalPollSeconds = 5
	e := NewSyncEngine(cfg, &fakeClipboard{}, NewLogBuffer(10))
	if got := e.localInterval(); got != 5*time.Second {
		t.Errorf("配置 5 秒时实际间隔应为 5s，得到 %v（换算倍数写错了？）", got)
	}

	cfg.LocalPollSeconds = 60
	e.UpdateConfig(cfg)
	if got := e.localInterval(); got != time.Minute {
		t.Errorf("配置 60 秒时实际间隔应为 1m，得到 %v", got)
	}
}

// 超过上限的内容既不该推送到远端，也**不该出现在界面的「本地内容」里**。
//
// 「本地内容」那一栏的语义是「本地与远端一致的那份内容」，不是「剪贴板里现在有什么」。
// 曾经的做法是把超长内容也写进 current（当时是为了让告警只报一次），
// 结果界面上显示出一段根本没同步过去的内容——用户看到的和实际发生的不一致。
//
// 顺带钉住第二个后果：超长内容**不能被随后的远端轮询冲掉**。
// 若把超长内容写进 current，远端轮询会发现 remote != current，
// 于是把远端那份旧内容写回剪贴板，用户刚复制的那段长文本就没了。
func TestOversizeNotShownAsCurrentText(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	cfg := engine.Config()
	cfg.MaxTextChars = 10
	engine.UpdateConfig(cfg)

	engine.prime()

	// 先同步一条正常内容，作为「本地内容」的基线
	short := "短内容"
	clip.set(short)
	engine.checkLocalOnly()
	if st := engine.Status(); st.CurrentText != preview(short, previewLen) {
		t.Fatalf("基线没建立：「本地内容」= %q", st.CurrentText)
	}
	remoteBefore, ok := fake.file(remoteFilePath)
	if !ok {
		t.Fatal("基线内容没推到远端")
	}
	writesBefore := clip.sets()

	// 复制一段超长内容。**先只跑本地检查**：这一步才是「界面上会不会显示出来」的关键。
	// （不能直接跑整轮——拉取阶段可能会把 current 又改回去，
	//   那样中间那一瞬间的错误显示就被盖掉了，断言反而漏掉。）
	long := strings.Repeat("长", 11)
	clip.set(long)
	engine.checkLocalOnly()

	st := engine.Status()
	if strings.Contains(st.CurrentText, "长") {
		t.Errorf("超长内容不该出现在「本地内容」里，实际 %q", st.CurrentText)
	}
	if st.CurrentText != preview(short, previewLen) {
		t.Errorf("超长时「本地内容」应保持上一次同步成功的内容 %q，实际 %q",
			preview(short, previewLen), st.CurrentText)
	}
	if got, _ := fake.file(remoteFilePath); got != remoteBefore {
		t.Error("超长内容不该推送到远端")
	}
	if st.OversizeCount != 1 {
		t.Errorf("超长跳过次数应为 1，实际 %d", st.OversizeCount)
	}

	// 再跑一整轮（含远端拉取）：用户刚复制的长文本还在剪贴板上，不能被冲掉。
	// 若把超长内容写进 current，远端轮询会发现 remote != current，
	// 于是把远端那份旧内容写回剪贴板。
	engine.syncCycle(true)
	if got, _ := clip.GetText(); got != long {
		t.Errorf("剪贴板里的长文本被冲掉了：%q", got)
	}
	if n := clip.sets(); n != writesBefore {
		t.Errorf("超长内容不该导致任何剪贴板写入，实际多写了 %d 次", n-writesBefore)
	}
}

// 手动推送（托盘菜单与界面右上角的「推送」按钮共用 PushNow）用「手动推送上限」，
// 可以突破自动同步的「文本长度上限」——偶尔确实需要把一段长文本强推过去。
// 两个上限互不影响：自动同步仍然只看 MaxTextChars。
func TestManualPushBypassesAutoLimit(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	cfg := engine.Config()
	cfg.MaxTextChars = 10         // 自动同步：10 字符以内
	cfg.ManualMaxTextChars = 1000 // 手动推送：放宽到 1000
	engine.UpdateConfig(cfg)

	long := strings.Repeat("长", 11)

	// 自动同步会跳过它（超自动上限）
	clip.set(long)
	engine.checkLocalOnly()
	if _, ok := fake.file(remoteFilePath); ok {
		t.Error("超自动上限的内容不该被自动推送")
	}
	if st := engine.Status(); st.OversizeCount != 1 {
		t.Errorf("自动跳过计数应为 1，实际 %d", st.OversizeCount)
	}

	// 同一份内容，手动推送应成功
	if err := engine.PushNow(); err != nil {
		t.Fatalf("手动推送应能突破自动上限，却失败：%v", err)
	}
	if got, ok := fake.file(remoteFilePath); !ok || !strings.Contains(got, long) {
		t.Error("手动推送的内容没到远端")
	}
	// 推成功了就是「已达成一致的那份」，界面「本地内容」要跟着更新
	if st := engine.Status(); !strings.Contains(st.CurrentText, "长") {
		t.Errorf("手动推送成功后「本地内容」应更新，实际 %q", st.CurrentText)
	}

	// 超过「手动推送上限」时，手动推送也要拦住，且报错要说清是哪个上限
	cfg.ManualMaxTextChars = 10
	engine.UpdateConfig(cfg)
	err := engine.PushNow()
	if err == nil {
		t.Fatal("超过手动推送上限时，手动推送应被拦住")
	}
	if !strings.Contains(err.Error(), "手动推送上限") {
		t.Errorf("报错要说明是哪个上限被突破失败，实际 %q", err.Error())
	}

	// 0 = 不限制
	cfg.ManualMaxTextChars = 0
	engine.UpdateConfig(cfg)
	if err := engine.PushNow(); err != nil {
		t.Errorf("手动推送上限为 0（不限制）时不该拦住：%v", err)
	}
}

// 超过文本长度上限的本地内容不应推送到远端，且上限按「字符」而不是「字节」计。
func TestOversizeNotPushed(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	cfg := engine.Config()
	cfg.MaxTextChars = 10
	engine.UpdateConfig(cfg)

	// 刚好等于上限：应推送
	atLimit := strings.Repeat("中", 10)
	clip.set(atLimit)
	engine.checkLocalOnly()
	content, ok := fake.file(remoteFilePath)
	if !ok || !strings.Contains(content, atLimit) {
		t.Fatalf("刚好等于上限的内容应被推送（远端 ok=%v）", ok)
	}

	// 超过上限：不推送，但要计数供界面展示
	clip.set(strings.Repeat("中", 11))
	engine.checkLocalOnly()
	if got, _ := fake.file(remoteFilePath); got != content {
		t.Error("超过上限的内容不应推送到远端")
	}
	if st := engine.Status(); st.OversizeCount != 1 {
		t.Errorf("超长跳过次数应为 1，实际 %d", st.OversizeCount)
	}

	// 关键：按字符而非字节。60 个中文 = 180 字节，但在 100 字符上限内，应推送。
	cfg.MaxTextChars = 100
	engine.UpdateConfig(cfg)
	text := strings.Repeat("中", 60)
	clip.set(text)
	engine.checkLocalOnly()
	if got, _ := fake.file(remoteFilePath); !strings.Contains(got, text) {
		t.Error("60 个中文字符（180 字节）在 100 字符上限内，应被推送")
	}

	// 0 表示不限制
	cfg.MaxTextChars = 0
	engine.UpdateConfig(cfg)
	long := strings.Repeat("字", 2000)
	clip.set(long)
	engine.checkLocalOnly()
	if got, _ := fake.file(remoteFilePath); !strings.Contains(got, long) {
		t.Error("上限为 0 时不应限制长度")
	}
}

// 启动后不应紧接着再打一次远端请求。
//
// prime() 本身已经完整跑过一轮（本地检查 + 远端拉取）。如果循环把
// nextRemote 初始化成 time.Now()，那么 200ms 后的第一次 tick 会立刻
// 判定「到点了」，于是刚启动就多一次毫无意义的往返。
// 实测每次启动多约 330 字节，虽然不多，但这是纯粹的浪费。
func TestNoRedundantStartupFetch(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	fake.setFile(remoteFilePath, `{"type":"text","text":"remote","size":6}`)

	clip := &fakeClipboard{}
	clip.set("remote")

	cfg := testConfig(srv.URL)
	cfg.PollSeconds = 60 // 远大于下面的观察窗口
	cfg.LocalPollSeconds = 60
	engine := NewSyncEngine(cfg, clip, NewLogBuffer(100))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.Start(ctx)

	// 等启动那一轮跑完
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, g := fake.counts(); g > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, afterPrime := fake.counts()
	if afterPrime == 0 {
		t.Fatal("启动时应至少拉取一次远端")
	}

	// 轮询间隔 60s，这 1.5 秒内不该有任何新的远端请求
	time.Sleep(1500 * time.Millisecond)
	if _, later := fake.counts(); later != afterPrime {
		t.Errorf("启动后出现多余的远端请求：get %d → %d（nextRemote 初始化成了 time.Now()？）",
			afterPrime, later)
	}
}
