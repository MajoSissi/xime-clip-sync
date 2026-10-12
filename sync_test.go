package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	// putOrder 记录 PUT 的路径顺序（图片要求「先 blob 后 JSON」）
	putOrder []string
	// 期望的 Basic 凭据（user:pass），为空表示不校验
	wantAuth string
	// 返回 503 限流
	rateLimit bool
	// putMissingParentStatus / putMissingParentBody 覆盖「父目录不存在时 PUT 怎么回」。
	// 0 = 409（标准）；设成 403 可模拟坚果云 / 反向代理后的 WebDAV。
	putMissingParentStatus int
	putMissingParentBody   string
	// mkcolStatus 非 0 时，所有 MKCOL 一律返回该状态（用来模拟「目录也建不起来」）。
	mkcolStatus int
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
		if f.mkcolStatus != 0 {
			w.WriteHeader(f.mkcolStatus)
			return
		}
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
			// 父目录不存在：409 是标准，404 见 Alist/Nextcloud，
			// 403 见部分坚果云 / 反向代理后的 WebDAV（本 fake 可配置）
			status := f.putMissingParentStatus
			if status == 0 {
				status = http.StatusConflict
			}
			if f.putMissingParentBody != "" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			}
			w.WriteHeader(status)
			if f.putMissingParentBody != "" {
				io.WriteString(w, f.putMissingParentBody)
			}
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.etagSeq++
		et := fmt.Sprintf("%q", fmt.Sprintf("v%d", f.etagSeq))
		f.files[path] = body
		f.etags[path] = et
		f.putCount++
		f.putOrder = append(f.putOrder, path)
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

// hasDir 报告某个目录是否已在远端存在（「测试连接建目录」的用例要看它）。
func (f *fakeWebDAV) hasDir(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[path]
}

func (f *fakeWebDAV) setFile(path, content string) {
	f.setFileBytes(path, []byte(content))
}

// setFileBytes 直接放一段二进制内容（图片 blob 用）。
func (f *fakeWebDAV) setFileBytes(path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etagSeq++
	f.files[path] = content
	f.etags[path] = fmt.Sprintf("%q", fmt.Sprintf("v%d", f.etagSeq))
}

// blob 返回远端某个附件的内容。
func (f *fakeWebDAV) blob(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	return b, ok
}

// puts 返回 PUT 的路径顺序（图片要求「先 blob 后 JSON」）。
func (f *fakeWebDAV) puts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putOrder...)
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
	image    []byte // PNG 字节；非空表示剪贴板里是图片
	seq      uint32
	setCount int
}

func (c *fakeClipboard) GetText() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 真实 Windows 上「复制一张图片」的剪贴板里没有 CF_UNICODETEXT，读出来就是空串
	if c.image != nil {
		return "", nil
	}
	return c.text, nil
}

func (c *fakeClipboard) SetText(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = text
	c.image = nil
	c.seq++
	c.setCount++
	return nil
}

func (c *fakeClipboard) Seq() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq, true
}

// HasImage / GetImage / SetImage 让假剪贴板也满足 imageClipboard，
// 这样图片相关的同步链路能在不碰真实剪贴板的前提下端到端跑。
func (c *fakeClipboard) HasImage() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.image != nil
}

func (c *fakeClipboard) GetImage() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.image == nil {
		return nil, nil
	}
	out := make([]byte, len(c.image))
	copy(out, c.image)
	return out, nil
}

func (c *fakeClipboard) SetImage(png []byte) error {
	// 真实实现会先解码校验；这里也校验一下，免得测试把坏数据当成合法图片
	if _, err := decodePNG(png); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.image = append([]byte(nil), png...)
	c.text = ""
	c.seq++
	c.setCount++
	return nil
}

// sets 返回写入剪贴板的次数。
func (c *fakeClipboard) sets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setCount
}

// set 模拟用户复制文本：内容变化并推进序号。
func (c *fakeClipboard) set(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = text
	c.image = nil
	c.seq++
}

// setImage 模拟用户复制图片：剪贴板里只有位图、没有文本。
func (c *fakeClipboard) setImage(png []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.image = append([]byte(nil), png...)
	c.text = ""
	c.seq++
}

func (c *fakeClipboard) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// getImage 返回当前剪贴板里的图片字节（没有则 nil）。
func (c *fakeClipboard) getImage() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.image == nil {
		return nil
	}
	return append([]byte(nil), c.image...)
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
	// 测试一律直连。默认配置是「跟随系统代理」，照抄的话这套测试就会去读
	// **开发机自己的注册表**——本机配了系统代理时，指向 127.0.0.1 的 httptest
	// 请求会被塞进真代理里，测试随机器而红或绿。测试不该依赖开发机的网络设置。
	cfg.ProxyMode = ProxyModeNone
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

	got, ok := newWebDAVClient(cfg, NewLogBuffer(10)).fileURL()
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
	client := newWebDAVClient(cfg, NewLogBuffer(10))

	if err := client.testConnection(); err != nil {
		t.Fatalf("带认证的连接测试失败：%v", err)
	}

	// 错误密码应报认证失败
	bad := cfg
	bad.Password = "wrong"
	if err := newWebDAVClient(bad, NewLogBuffer(10)).testConnection(); err == nil ||
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

	all := lb.Lines(LogSync, 0)
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

// 同步引擎写出来的日志必须落在同步通道里，一条都不能漏到程序通道；
// /api/logs 也要把两条通道分开返回，且每行都带 kind——
// 界面就是靠它决定这一行进「同步日志」还是「程序日志」面板的。
func TestSyncLogsStayOnSyncChannel(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()

	clip := &fakeClipboard{}
	lb := NewLogBuffer(100)
	engine := NewSyncEngine(testConfig(srv.URL), clip, lb)

	engine.syncCycle(true)  // 建立基线
	clip.set("要推过去的内容")     // 用户复制了新内容
	engine.checkLocalOnly() // → ⬆ 推送
	lb.Infof("程序自己的事")      // 程序通道的一条

	syncLines := lb.Lines(LogSync, 0)
	if len(syncLines) == 0 {
		t.Fatal("同步通道里一条日志都没有")
	}
	pushed := false
	for _, l := range syncLines {
		if l.Kind != LogSync {
			t.Errorf("同步通道里混进了 kind=%q 的日志：%+v", l.Kind, l)
		}
		if strings.HasPrefix(l.Text, "⬆") {
			pushed = true
		}
	}
	if !pushed {
		t.Errorf("推送没写进同步通道：%+v", syncLines)
	}

	appLines := lb.Lines(LogApp, 0)
	if len(appLines) != 1 || appLines[0].Text != "程序自己的事" {
		t.Errorf("程序通道内容不符：%+v", appLines)
	}

	// HTTP 层：两条通道分开返回，行里带 kind
	ui := httptest.NewServer(NewServer(engine, lb, "config.json").Handler())
	defer ui.Close()
	resp, err := http.Get(ui.URL + "/api/logs")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string][]LogLine
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(got["sync"]) != len(syncLines) {
		t.Errorf("/api/logs 的 sync 应有 %d 行，实际 %d 行", len(syncLines), len(got["sync"]))
	}
	if len(got["app"]) != 1 || got["app"][0].Kind != LogApp {
		t.Errorf("/api/logs 的 app 通道不符：%+v", got["app"])
	}
	for _, l := range got["sync"] {
		if l.Kind != LogSync {
			t.Errorf("/api/logs 的 sync 里混进了 %+v", l)
		}
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

// ---------------------------------------------------------------- 传输层重试

// 只有「快速失败」的传输错误才值得立刻重试。
//
// 超时类错误必须排除：重试会再等一个完整的 Client.Timeout（30 秒），
// 而这段时间 syncMu 被占着，连本地剪贴板的推送都跟着停摆。
func TestTransportErrIsRetriable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"Client.Timeout 到点（超时类）→ 不重试",
			&url.Error{Op: "Get", URL: "https://x/dav/", Err: context.DeadlineExceeded},
			false,
		},
		{
			"连接被对端断开（EOF）→ 重试",
			&url.Error{Op: "Get", URL: "https://x/dav/", Err: io.EOF},
			true,
		},
		{
			"DNS 解析失败 → 重试",
			&url.Error{Op: "Get", URL: "https://x/dav/", Err: &net.DNSError{Err: "no such host", Name: "x"}},
			true,
		},
		{
			"连接被拒绝 → 重试",
			&url.Error{Op: "Get", URL: "https://x/dav/", Err: syscall.ECONNREFUSED},
			true,
		},
		{"认不出来的错误按可重试处理", errors.New("boom"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transportErrIsRetriable(c.err); got != c.want {
				t.Errorf("transportErrIsRetriable(%v) = %v，期望 %v", c.err, got, c.want)
			}
		})
	}
}

// 传输层断连要自动重试一次，且**只重试一次**。
//
// 服务端第一次请求直接掐掉连接（制造传输层 EOF，不是 HTTP 错误码），
// 第二次正常应答。客户端应当自己恢复，用户不该为此白等一个轮询周期。
func TestTransferErrorRetriesOnce(t *testing.T) {
	var mu sync.Mutex
	hits := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		first := hits == 1
		mu.Unlock()

		if first {
			// 掐断连接：客户端读到的是 EOF（传输层错误），而不是某个状态码
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	lb := NewLogBuffer(50)
	client := newWebDAVClient(cfg, lb)

	status, _, err := client.do(http.MethodGet, srv.URL+"/dav/x.json", nil, nil)
	if err != nil {
		t.Fatalf("第一次断连后应当自动重试并成功，实际报错：%v", err)
	}
	if status != http.StatusOK {
		t.Errorf("重试后应拿到 200，实际 %d", status)
	}

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Errorf("服务端应被请求 2 次（原请求 + 一次重试），实际 %d 次", got)
	}

	// 重试这件事必须留痕：否则日志里只有「最终成功了」，看不出中间抖动过。
	var sawRetry bool
	for _, l := range lb.Lines(LogSync, 0) {
		if strings.Contains(l.Text, "重试") {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Errorf("重试没有记日志，实际日志：%+v", lb.Lines(LogSync, 0))
	}
}

// 超时类错误不重试：服务端一直不响应时，请求次数必须停在 1。
//
// 这条是「重试不会把 30 秒阻塞翻倍」的客观证据。
func TestTimeoutIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		<-release // 挂住不响应，逼客户端超时
	}))
	defer srv.Close()
	defer close(release)

	cfg := testConfig(srv.URL)
	client := newWebDAVClient(cfg, NewLogBuffer(50))
	// 把超时压到很短，测试不用真等 30 秒
	client.http.Timeout = 150 * time.Millisecond

	start := time.Now()
	if _, _, err := client.do(http.MethodGet, srv.URL+"/dav/x.json", nil, nil); err == nil {
		t.Fatal("服务端不响应时应当超时报错")
	}
	elapsed := time.Since(start)

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Errorf("超时不该重试，服务端应只被请求 1 次，实际 %d 次（耗时 %v）", got, elapsed)
	}
	// 重试一次会多出 500ms 的等待；不重试的话耗时就在超时值附近
	if elapsed > time.Second {
		t.Errorf("超时路径耗时 %v，像是又重试了一次", elapsed)
	}
}

// ---------------------------------------------------------------- 图片同步

const blobDirPath = "/dav/xime/clipboard/blobs"

// makeNoisePNG 造一张 w×h 的 PNG，像素是伪随机噪声（不可压缩），
// 体积大致 = w*h*4 字节，方便构造「超过上限」的用例。
func makeNoisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	x := uint32(0x9E3779B9)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = uint8(x)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255 // 不透明，避免预乘带来的额外差异
	}
	data, err := encodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// makeBigPNG 造一张编码后**大于** minBytes 的噪声图。
//
// 图片上限的单位是 MB，所以「超限」用例需要一张实打实超过 1 MB 的图。
// 噪声不可压缩，PNG 体积基本随像素数线性涨，按几档边长试到够大为止。
func makeBigPNG(t *testing.T, minBytes int) []byte {
	t.Helper()
	for _, n := range []int{256, 512, 700, 1024, 1400} {
		if png := makeNoisePNG(t, n, n); len(png) > minBytes {
			return png
		}
	}
	t.Fatalf("无法生成大于 %d 字节的测试图", minBytes)
	return nil
}

// makeSolidPNG 造一张很小的纯色 PNG（体积几十字节）。
func makeSolidPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	data, err := encodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// blobPath 由附件名拼出远端路径。
func blobPath(dataName string) string { return blobDirPath + "/" + dataName }

// 图片记录的 wire 格式：has_data 为真、data_name 是内容寻址的 <sha256>.png。
func TestImageProfileWireFormat(t *testing.T) {
	png := makeSolidPNG(t, 2, 2, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	p := newImageProfile(png, "my-pc")

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
	if m["type"] != "image" || m["has_data"] != true {
		t.Errorf("type/has_data 不符：%s", data)
	}
	wantName := hashBytes(png) + ".png"
	if m["data_name"] != wantName {
		t.Errorf("data_name 应为内容寻址 %q，实际 %v", wantName, m["data_name"])
	}
	if m["hash"] != hashBytes(png) {
		t.Errorf("hash 应为图片字节的 SHA-256：%v", m["hash"])
	}
	if int(m["size"].(float64)) != len(png) {
		t.Errorf("size 应为 PNG 字节数 %d，实际 %v", len(png), m["size"])
	}
	if m["source"] != "my-pc" {
		t.Errorf("source 不符：%v", m["source"])
	}
}

// 图片记录**没有 text 值**（插件端直接省略这个键）。
//
// 这是必须单独钉住的一条：早先 decodeProfile 只认 text 键，
// 遇到图片 JSON 会掉进「旧版纯文本兼容」分支，把整段 JSON 当剪贴板文本写回本地。
func TestDecodeImageProfile(t *testing.T) {
	// 插件写法：完全没有 text 键
	prof, plain, ok := decodeProfile([]byte(`{"type":"image","hash":"h","has_data":true,"data_name":"abc.png","size":123,"source":"phone"}`))
	if !ok || plain {
		t.Fatalf("图片 JSON 应被认成 Profile：ok=%v plain=%v", ok, plain)
	}
	if !prof.HasData || prof.DataName == nil || *prof.DataName != "abc.png" || prof.Size != 123 {
		t.Errorf("图片记录解析结果不符：%+v", prof)
	}
	if prof.Text != "" {
		t.Errorf("图片记录的 text 应为空，实际 %q", prof.Text)
	}

	// 本项目写法：text 是空串
	prof, plain, ok = decodeProfile([]byte(`{"type":"image","hash":"h","text":"","has_data":true,"data_name":"abc.png","size":123}`))
	if !ok || plain || !prof.HasData {
		t.Fatalf("带空 text 的图片 JSON 也应被认成 Profile：ok=%v plain=%v %+v", ok, plain, prof)
	}

	// 反向哨兵：普通 JSON 对象（没有 Profile 特征字段）不能被当成 Profile
	if _, plain, ok := decodeProfile([]byte(`{"foo":1}`)); !ok || !plain {
		t.Errorf("非 Profile 的 JSON 应退回纯文本兼容：ok=%v plain=%v", ok, plain)
	}
}

// 附件 URL：{剪贴板文件所在目录}/blobs/{名字}，非法名字一律拒绝。
func TestBuildBlobURL(t *testing.T) {
	const file = "https://host/dav/xime/clipboard/current.json"
	got, ok := buildBlobURL(file, "abc123.png")
	if !ok || got != "https://host/dav/xime/clipboard/blobs/abc123.png" {
		t.Errorf("buildBlobURL = (%q, %v)", got, ok)
	}
	// 用户的「远程路径」自定义到别处时，blobs 跟着那个目录走
	if got, ok := buildBlobURL("https://host/dav/rime/clip/1.json", "a.png"); !ok ||
		got != "https://host/dav/rime/clip/blobs/a.png" {
		t.Errorf("自定义路径下 buildBlobURL = (%q, %v)", got, ok)
	}

	// 路径穿越 / 空名 / 路径分隔符必须被白名单挡住
	for _, bad := range []string{"", "..", "../evil", "a/b.png", "/abs.png", ".hidden", "a\\b.png", "a b.png"} {
		if got, ok := buildBlobURL(file, bad); ok {
			t.Errorf("非法附件名 %q 不该被接受，得到 %q", bad, got)
		}
	}
}

// blobsDirURL 只要目录本身，用于配置阶段「先把目录建好」。
func TestBlobsDirURL(t *testing.T) {
	got, ok := blobsDirURL("https://host/dav/xime/clipboard/current.json")
	if !ok || got != "https://host/dav/xime/clipboard/blobs" {
		t.Errorf("blobsDirURL = (%q, %v)", got, ok)
	}
	// 自定义嵌套路径时跟着走
	if got, ok := blobsDirURL("https://host/dav/rime/clip/1.json"); !ok ||
		got != "https://host/dav/rime/clip/blobs" {
		t.Errorf("自定义路径下 blobsDirURL = (%q, %v)", got, ok)
	}
	// 和 buildBlobURL 必须拼出同一个前缀，否则「建目录」和「写附件」会各说各话
	blob, _ := buildBlobURL("https://host/dav/xime/clipboard/current.json", "a.png")
	if dir, _ := blobsDirURL("https://host/dav/xime/clipboard/current.json"); blob != dir+"/a.png" {
		t.Errorf("blobsDirURL(%q) 与 buildBlobURL(%q) 前缀不一致", dir, blob)
	}
}

// relativePathParts 把 URL 当目录拆；relativeDirParts 先剥掉文件名再拆。
func TestRelativePathParts(t *testing.T) {
	const base = "https://host/dav/"
	if got := relativePathParts(base, "https://host/dav/xime/clipboard/blobs"); len(got) != 3 ||
		got[0] != "xime" || got[1] != "clipboard" || got[2] != "blobs" {
		t.Errorf("relativePathParts(目录) = %v，期望 [xime clipboard blobs]", got)
	}
	// 同一个 URL 交给 relativeDirParts，应少一层（剥掉最后一段）
	if got := relativeDirParts(base, "https://host/dav/xime/clipboard/blobs"); len(got) != 2 ||
		got[0] != "xime" || got[1] != "clipboard" {
		t.Errorf("relativeDirParts(目录) = %v，期望 [xime clipboard]", got)
	}
	// davURL 之后没有目录层级时返回空
	if got := relativePathParts(base, "https://host/dav"); len(got) != 0 {
		t.Errorf("根目录场景 relativePathParts = %v，期望空", got)
	}
}

func TestContentTypeOf(t *testing.T) {
	cases := map[string]string{
		"a.png":    "image/png",
		"a.PNG":    "image/png",
		"a.jpg":    "image/jpeg",
		"a.jpeg":   "image/jpeg",
		"a.webp":   "image/webp",
		"a.bmp":    "image/bmp",
		"a.bin":    "application/octet-stream",
		"noext":    "application/octet-stream",
		"a.tar.gz": "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentTypeOf(name); got != want {
			t.Errorf("contentTypeOf(%q) = %q，期望 %q", name, got, want)
		}
	}
}

// 本地复制一张图片 → 远端应同时出现 blob 与 JSON，且**顺序是先 blob 后 JSON**。
//
// 顺序反了会留下「JSON 指向不存在 blob」的悬空引用，对端会反复下载失败。
func TestPushImageToRemote(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	engine.syncCycle(true) // 建立基线（远端为空）

	png := makeNoisePNG(t, 16, 16)
	clip.setImage(png)
	engine.checkLocalOnly()

	// JSON 已写入，且是图片记录
	content, ok := fake.file(remoteFilePath)
	if !ok {
		t.Fatal("图片没有推送到远端")
	}
	prof, plain, ok := decodeProfile([]byte(content))
	if !ok || plain || !prof.HasData {
		t.Fatalf("远端不是图片记录：ok=%v plain=%v %s", ok, plain, content)
	}
	if prof.DataName == nil {
		t.Fatal("图片记录缺少 data_name")
	}

	// blob 内容与本地 PNG 逐字节一致
	stored, ok := fake.blob(blobPath(*prof.DataName))
	if !ok {
		t.Fatalf("远端附件不存在：%s", *prof.DataName)
	}
	if !bytes.Equal(stored, png) {
		t.Errorf("远端附件与本地图片不一致（%d vs %d 字节）", len(stored), len(png))
	}
	if prof.Size != len(png) {
		t.Errorf("记录里的 size 应为 %d，实际 %d", len(png), prof.Size)
	}

	// 顺序：blob 必须在 JSON 之前
	puts := fake.puts()
	if len(puts) < 2 {
		t.Fatalf("PUT 次数太少：%v", puts)
	}
	if puts[0] != blobPath(*prof.DataName) || puts[1] != remoteFilePath {
		t.Errorf("必须先 PUT blob 再 PUT JSON，实际顺序 %v", puts)
	}

	// 界面上「本地内容」应显示为图片，而不是空文本
	if st := engine.Status(); st.CurrentKind != kindImage || st.CurrentImageBytes != len(png) {
		t.Errorf("状态里的图片信息不符：kind=%q bytes=%d", st.CurrentKind, st.CurrentImageBytes)
	}
}

// 远端来了一张图 → 写进本地剪贴板；同一张图不重复写（回声抑制）。
func TestPullImageToClipboard(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	engine.syncCycle(true)

	png := makeNoisePNG(t, 16, 16)
	prof := newImageProfile(png, "phone")
	body, err := prof.encode()
	if err != nil {
		t.Fatal(err)
	}
	fake.setFile(remoteFilePath, string(body))
	fake.setFileBytes(blobPath(*prof.DataName), png)

	engine.syncCycle(true)
	if got := clip.getImage(); !bytes.Equal(got, png) {
		t.Fatalf("远端图片没有写进剪贴板（得到 %d 字节）", len(got))
	}
	if got := clip.get(); got != "" {
		t.Errorf("图片记录不该产生文本，实际 %q", got)
	}

	// 回声抑制：同一张图反复轮询不得重复写剪贴板
	writesBefore := clip.sets()
	for i := 0; i < 3; i++ {
		engine.syncCycle(true) // force：绕过 ETag，逼它真的重新比对
	}
	if after := clip.sets(); after != writesBefore {
		t.Errorf("同一张图被重复写入剪贴板：%d → %d", writesBefore, after)
	}

	// 远端换成另一张图 → 应重新应用
	other := makeNoisePNG(t, 12, 12)
	otherProf := newImageProfile(other, "phone")
	otherBody, _ := otherProf.encode()
	fake.setFile(remoteFilePath, string(otherBody))
	fake.setFileBytes(blobPath(*otherProf.DataName), other)
	engine.syncCycle(true)
	if got := clip.getImage(); !bytes.Equal(got, other) {
		t.Error("远端换成新图片后没有更新剪贴板")
	}
}

// 拉取到的附件不是合法图片时必须报错跳过，不能把坏字节塞进剪贴板。
func TestPullCorruptBlobIsRejected(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	engine.syncCycle(true)

	fake.setFile(remoteFilePath, `{"type":"image","hash":"h","has_data":true,"data_name":"bad.png","size":3}`)
	fake.setFileBytes(blobPath("bad.png"), []byte("nope"))
	engine.syncCycle(true)

	if clip.getImage() != nil {
		t.Error("坏附件不该被写进剪贴板")
	}
	if st := engine.Status(); st.LastError == "" {
		t.Error("坏附件应当留下错误信息")
	}
}

// 父目录不存在时服务器返回 403（而不是 409/404），也要能自动建目录并重试成功。
//
// 这正是「上传附件失败（HTTP 403）」最可能的成因：远端还没有 blobs 目录，
// 而部分服务器（坚果云 / 反向代理后的 WebDAV）用 403 而不是 409 表达
// 「父目录不存在」。插件 main.ts 只兜了 409/404，所以这条路径此前是断的。
func TestBlobUploadRecoversFrom403MissingDir(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)
	fake.putMissingParentStatus = http.StatusForbidden

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	engine.syncCycle(true)

	png := makeSolidPNG(t, 4, 4, color.NRGBA{G: 200, A: 255})
	clip.setImage(png)
	engine.checkLocalOnly()

	prof := newImageProfile(png, "")
	if _, ok := fake.file(blobPath(*prof.DataName)); !ok {
		t.Fatal("403 之后应自动建目录并重试成功，附件却没上去")
	}
	if _, ok := fake.file(remoteFilePath); !ok {
		t.Error("附件成功后应继续写 current.json")
	}
	if st := engine.Status(); st.LastError != "" {
		t.Errorf("不该留下错误：%s", st.LastError)
	}
}

// 403 且目录也建不起来时：错误信息要同时给出状态码含义和服务端原话，
// 用户才能判断是「程序没建目录」还是「服务器不让写」。
func TestUploadErrorCarriesServerMessage(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)
	fake.putMissingParentStatus = http.StatusForbidden
	fake.putMissingParentBody = "<D:error><D:quota-exceeded/></D:error>"
	fake.mkcolStatus = http.StatusForbidden

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	clip.setImage(makeSolidPNG(t, 4, 4, color.NRGBA{B: 200, A: 255}))
	err := engine.PushNow()
	if err == nil {
		t.Fatal("目录建不起来时推送应当失败")
	}
	msg := err.Error()
	// 状态码、服务端原话、以及「建目录也失败了」三者都要在，缺一个用户就得靠猜
	for _, want := range []string{"403", "quota-exceeded", "MKCOL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际：%s", want, msg)
		}
	}
	// XML 尖括号不能原样进界面（会破坏排版），必须已被压平
	if strings.Contains(msg, "<") || strings.Contains(msg, ">") {
		t.Errorf("响应体里的尖括号应被压平，实际：%s", msg)
	}
}

// 「测试连接」应顺手把远端目录建好（剪贴板目录 + blobs 附件目录），
// 这样后续同步就不必再依赖「父目录不存在会返回哪个状态码」。
func TestTestConnectionPreparesRemoteDirs(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	if err := TestConnection(testConfig(srv.URL), NewLogBuffer(10)); err != nil {
		t.Fatalf("测试连接应成功：%v", err)
	}
	for _, dir := range []string{"/dav/xime", "/dav/xime/clipboard", "/dav/xime/clipboard/blobs"} {
		if !fake.hasDir(dir) {
			t.Errorf("测试连接后目录 %s 应已创建", dir)
		}
	}
	// 只建目录，不写任何文件
	if put, _ := fake.counts(); put != 0 {
		t.Errorf("测试连接不该写文件，实际 PUT %d 次", put)
	}

	// 关键：目录就绪之后，即使服务器对缺失父目录回 403，推图也不该再失败。
	// 这正是用户那次「上传附件失败（HTTP 403）」的场景。
	fake.putMissingParentStatus = http.StatusForbidden
	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	clip.setImage(makeSolidPNG(t, 4, 4, color.NRGBA{R: 120, A: 255}))
	if err := engine.PushNow(); err != nil {
		t.Fatalf("目录已就绪后推图应成功，却失败：%v", err)
	}
}

// 目录建不起来时，测试连接要如实报错，并说清是「连上了、但准备目录失败」。
func TestTestConnectionReportsDirFailure(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)
	fake.mkcolStatus = http.StatusForbidden

	err := TestConnection(testConfig(srv.URL), NewLogBuffer(10))
	if err == nil {
		t.Fatal("MKCOL 被拒时测试连接应当失败")
	}
	if !strings.Contains(err.Error(), "准备远端目录") {
		t.Errorf("错误信息应说明是准备目录失败，实际：%v", err)
	}
}

// 反复点「测试连接」不应有副作用：目录已存在时 MKCOL 返回 405，被当作成功。
func TestTestConnectionIsIdempotent(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	for i := 0; i < 3; i++ {
		if err := TestConnection(testConfig(srv.URL), NewLogBuffer(10)); err != nil {
			t.Fatalf("第 %d 次测试连接应成功（目录已存在不算错）：%v", i+1, err)
		}
	}
	if put, _ := fake.counts(); put != 0 {
		t.Errorf("测试连接不该写文件，实际 PUT %d 次", put)
	}
}

// 超过自动上限的图片不推送，但要计数供界面展示。
func TestImageOversizeNotPushed(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	cfg := engine.Config()
	cfg.MaxImageMB = 1 // 1 MB
	engine.UpdateConfig(cfg)
	engine.syncCycle(true)

	// 小图：在 1 MB 内 → 应推送
	small := makeSolidPNG(t, 2, 2, color.NRGBA{R: 255, A: 255})
	if len(small) > 1<<20 {
		t.Fatalf("测试前提不成立：小图 %d 字节已超过 1 MB", len(small))
	}
	clip.setImage(small)
	engine.checkLocalOnly()
	if _, ok := fake.file(remoteFilePath); !ok {
		t.Fatal("上限内的图片应被推送")
	}
	if st := engine.Status(); st.ImageOversizeCount != 0 {
		t.Errorf("不该有超限计数，实际 %d", st.ImageOversizeCount)
	}

	// 大图：超过 1 MB → 不推送，且计数
	big := makeBigPNG(t, 1<<20)
	if len(big) <= 1<<20 {
		t.Fatalf("测试前提不成立：噪声图只有 %d 字节", len(big))
	}
	before, _ := fake.file(remoteFilePath)
	clip.setImage(big)
	engine.checkLocalOnly()
	if after, _ := fake.file(remoteFilePath); after != before {
		t.Error("超限图片不该推送到远端")
	}
	if st := engine.Status(); st.ImageOversizeCount != 1 {
		t.Errorf("图片超限计数应为 1，实际 %d", st.ImageOversizeCount)
	}

	// 0 = 不限制。重新复制一次同样的图：序号没变的话本地检查会直接跳过，
	// 测不到「上限为 0 会不会放行」。
	cfg.MaxImageMB = 0
	engine.UpdateConfig(cfg)
	clip.setImage(big)
	engine.checkLocalOnly()
	if got, _ := fake.file(remoteFilePath); got == before {
		t.Error("上限为 0 时不该限制图片体积")
	}
}

// 手动推送用「手动图片上限」，可以突破自动上限；两个上限互不影响。
func TestManualImagePushBypassesAutoLimit(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	cfg := engine.Config()
	cfg.MaxImageMB = 1
	cfg.ManualMaxImageMB = 4
	engine.UpdateConfig(cfg)

	big := makeBigPNG(t, 1<<20)
	clip.setImage(big)

	// 自动同步会跳过（超自动上限）
	engine.checkLocalOnly()
	if _, ok := fake.file(remoteFilePath); ok {
		t.Error("超自动上限的图片不该被自动推送")
	}

	// 手动推送应成功
	if err := engine.PushNow(); err != nil {
		t.Fatalf("手动推送应能突破自动上限，却失败：%v", err)
	}
	if _, ok := fake.file(remoteFilePath); !ok {
		t.Error("手动推送的图片没到远端")
	}

	// 超过「手动上限」时手动推送也要拦住，且说清是哪个上限
	cfg.ManualMaxImageMB = 1
	engine.UpdateConfig(cfg)
	err := engine.PushNow()
	if err == nil {
		t.Fatal("超过手动图片上限时应被拦住")
	}
	if !strings.Contains(err.Error(), "手动推送上限") {
		t.Errorf("报错要说明是哪个上限，实际 %q", err.Error())
	}
}

// 图片同步关闭时：既不推图片，也不能把远端记录冲成空文本。
func TestImageSyncDisabledLeavesRemoteAlone(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)

	// 先让远端有一条文本记录
	clip.set("原有文本")
	engine.checkLocalOnly()
	before, ok := fake.file(remoteFilePath)
	if !ok {
		t.Fatal("文本没推到远端")
	}

	cfg := engine.Config()
	cfg.SyncImages = false
	engine.UpdateConfig(cfg)

	// 用户复制一张图片：剪贴板里没有文本
	clip.setImage(makeNoisePNG(t, 8, 8))
	engine.checkLocalOnly()

	if after, _ := fake.file(remoteFilePath); after != before {
		t.Errorf("图片同步关闭时不该改动远端记录：\n前 %s\n后 %s", before, after)
	}
	if got := clip.get(); got != "" {
		t.Errorf("不该写入文本，实际 %q", got)
	}

	// 远端来一张图：关闭时也不该写进剪贴板。
	// 先把本机剪贴板清空，否则「剪贴板里有图」是用户自己复制的那张，测不出远端有没有被应用。
	if err := clip.SetText(""); err != nil {
		t.Fatal(err)
	}
	png := makeNoisePNG(t, 8, 8)
	prof := newImageProfile(png, "phone")
	body, _ := prof.encode()
	fake.setFile(remoteFilePath, string(body))
	fake.setFileBytes(blobPath(*prof.DataName), png)
	engine.syncCycle(true)
	if clip.getImage() != nil {
		t.Error("图片同步关闭时不该把远端图片写进剪贴板")
	}
}

// 图片同步关闭 + 剪贴板是图片时，PushNow 要给出明确原因（而不是「没有文本」）。
func TestManualPushImageWhenDisabled(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	cfg := engine.Config()
	cfg.SyncImages = false
	engine.UpdateConfig(cfg)

	clip.setImage(makeNoisePNG(t, 8, 8))
	err := engine.PushNow()
	if err == nil {
		t.Fatal("图片同步关闭时手动推送应报错")
	}
	if !strings.Contains(err.Error(), "图片同步已关闭") {
		t.Errorf("报错应说明图片同步已关闭，实际 %q", err.Error())
	}

	// 既没文本也没图片：给出「没有可同步的内容」
	empty := &fakeClipboard{}
	engine2 := newTestEngine(t, srv, empty)
	if err := engine2.PushNow(); err == nil {
		t.Error("空剪贴板手动推送应报错")
	}
}

// 图片相关的两条高频日志也要短（与文本的 ⬆/⬇ 同一标准）。
func TestImageLogLinesAreShort(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine, lb := newTestEngineWithLog(t, srv, clip)
	engine.syncCycle(true)

	// 推一张
	clip.setImage(makeNoisePNG(t, 16, 16))
	engine.checkLocalOnly()

	// 拉一张（带上来源设备）
	png := makeNoisePNG(t, 12, 12)
	prof := newImageProfile(png, "phone")
	body, _ := prof.encode()
	fake.setFile(remoteFilePath, string(body))
	fake.setFileBytes(blobPath(*prof.DataName), png)
	engine.syncCycle(true)

	var pushLine, pullLine string
	for _, l := range lb.Lines(LogSync, 0) {
		if strings.HasPrefix(l.Text, "⬆") && strings.Contains(l.Text, "图片") {
			pushLine = l.Text
		}
		if strings.HasPrefix(l.Text, "⬇") && strings.Contains(l.Text, "图片") {
			pullLine = l.Text
		}
	}
	if pushLine == "" || pullLine == "" {
		t.Fatalf("没找到图片推/拉日志：%+v", lb.Lines(LogSync, 0))
	}
	for _, line := range []string{pushLine, pullLine} {
		if n := len([]rune(line)); n > 24 {
			t.Errorf("图片日志太长了（%d 字）：%q", n, line)
		}
	}
}

// 图片同步关闭后：复制图片既不推也不拉，且**不写日志**——
// 这是用户自己在「同步设置」里的选择，反复解释「为什么没同步」只会把日志刷满。
func TestImageSyncOffIsSilent(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine, lb := newTestEngineWithLog(t, srv, clip)

	cfg := engine.Config()
	cfg.SyncImages = false
	engine.UpdateConfig(cfg)
	engine.syncCycle(true)

	// 本地复制一张图 → 不推、不记日志
	before := len(lb.Lines(LogSync, 0))
	clip.setImage(makeNoisePNG(t, 8, 8))
	engine.checkLocalOnly()
	if _, ok := fake.file(remoteFilePath); ok {
		t.Error("图片同步关闭时不该把本地图片推到远端")
	}
	if after := len(lb.Lines(LogSync, 0)); after != before {
		t.Errorf("图片同步关闭时不该写日志，却多了 %d 行：%+v",
			after-before, lb.Lines(LogSync, 0)[before:])
	}

	// 远端是图片 → 不拉、也不记日志
	// 先把剪贴板清空：上一步是测试自己往里放的图，不清掉的话「剪贴板里还有图」
	// 这件事本身就来自测试自身，断言不出「远端那张图有没有被写进来」。
	if err := clip.SetText(""); err != nil {
		t.Fatal(err)
	}
	png := makeNoisePNG(t, 6, 6)
	prof := newImageProfile(png, "phone")
	body, _ := prof.encode()
	fake.setFile(remoteFilePath, string(body))
	fake.setFileBytes(blobPath(*prof.DataName), png)

	before = len(lb.Lines(LogSync, 0))
	engine.syncCycle(true)
	if got := clip.getImage(); got != nil {
		t.Error("图片同步关闭时不该把远端图片写进剪贴板")
	}
	if after := len(lb.Lines(LogSync, 0)); after != before {
		t.Errorf("远端是图片时也不该写日志，却多了 %d 行：%+v",
			after-before, lb.Lines(LogSync, 0)[before:])
	}
}

// 图片配置项的默认值与钳制。
func TestImageConfigDefaultsAndBounds(t *testing.T) {
	d := defaultConfig()
	if !d.SyncImages {
		t.Error("图片同步默认应开启")
	}
	if d.MaxImageMB != defaultMaxImageMB {
		t.Errorf("自动图片上限默认值应为 %d MB，实际 %d", defaultMaxImageMB, d.MaxImageMB)
	}
	if d.ManualMaxImageMB != defaultManualMaxImageMB {
		t.Errorf("手动图片上限默认值应为 %d MB，实际 %d", defaultManualMaxImageMB, d.ManualMaxImageMB)
	}
	if d.ManualMaxImageMB <= d.MaxImageMB {
		t.Errorf("手动图片上限（%d）应比自动（%d）宽，否则这个配置项没意义",
			d.ManualMaxImageMB, d.MaxImageMB)
	}

	c := defaultConfig()
	c.MaxImageMB = -5
	c.ManualMaxImageMB = -5
	c.normalize()
	if c.MaxImageMB != 0 || c.ManualMaxImageMB != 0 {
		t.Errorf("负数应归零（0 = 不限制）：%+v", c)
	}

	c = defaultConfig()
	c.MaxImageMB = 999999
	c.ManualMaxImageMB = 999999
	c.normalize()
	if c.MaxImageMB != maxImageMBLimit || c.ManualMaxImageMB != maxImageMBLimit {
		t.Errorf("过大应收敛到 %d：%+v", maxImageMBLimit, c)
	}

	// 硬上限必须小于附件读取上限，否则用户把上限调满后图片推得上去、拉不回来
	if maxImageMBLimit<<20 >= maxBlobResponseBytes {
		t.Errorf("图片上限 %d MB 不应超过附件读取上限 %d 字节", maxImageMBLimit, maxBlobResponseBytes)
	}

	// 旧配置里没有这三个字段时，应拿到默认值（而不是 false / 0）
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"davUrl":"https://host/dav/","remotePath":"xime/clipboard/current.json","enabled":true}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读取旧配置失败：%v", err)
	}
	if !cfg.SyncImages || cfg.MaxImageMB != defaultMaxImageMB || cfg.ManualMaxImageMB != defaultManualMaxImageMB {
		t.Errorf("旧配置升级后应拿到图片默认值：%+v", cfg)
	}
}

// 旧配置（图片上限用 KB）应被自动换算成 MB，而不是悄悄回到默认值。
func TestLegacyImageKBConfigMigratesToMB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"davUrl":"https://host/dav/","syncImages":true,` +
		`"maxImageKB":2048,"manualMaxImageKB":10240}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warns, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读取旧配置失败：%v", err)
	}
	if cfg.MaxImageMB != 2 {
		t.Errorf("2048 KB 应换算成 2 MB，实际 %d", cfg.MaxImageMB)
	}
	if cfg.ManualMaxImageMB != 10 {
		t.Errorf("10240 KB 应换算成 10 MB，实际 %d", cfg.ManualMaxImageMB)
	}
	// 用户得知道文件被改过，否则会以为设置自己变了
	joined := strings.Join(warns, "；")
	if !strings.Contains(joined, "KB") || !strings.Contains(joined, "MB") {
		t.Errorf("应提示单位已换算，实际 warnings=%v", warns)
	}

	// 换算结果要落盘：再读一次应直接是 MB，且旧键不再出现
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "maxImageKB") {
		t.Errorf("旧键应已从文件里消失：%s", raw)
	}
	cfg2, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.MaxImageMB != 2 || cfg2.ManualMaxImageMB != 10 {
		t.Errorf("二次读取应保持换算后的值：%+v", cfg2)
	}

	// 新键已存在时以新键为准，不被旧键覆盖
	both := `{"davUrl":"https://host/dav/","maxImageMB":7,` +
		`"manualMaxImageMB":9,"maxImageKB":1024,"manualMaxImageKB":1024}`
	if err := os.WriteFile(path, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg3, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.MaxImageMB != 7 || cfg3.ManualMaxImageMB != 9 {
		t.Errorf("新旧键都在时应以新键为准：%+v", cfg3)
	}
}

// kbToMB 的边界：0 保持 0（= 不限制），正数向上取整到至少 1 MB。
func TestKBToMBBoundaries(t *testing.T) {
	for _, tc := range []struct{ kb, want int }{
		{0, 0}, {-1, 0}, {1, 1}, {1023, 1}, {1024, 1}, {1025, 2}, {10240, 10},
	} {
		if got := kbToMB(tc.kb); got != tc.want {
			t.Errorf("kbToMB(%d) = %d，期望 %d", tc.kb, got, tc.want)
		}
	}
}

// 图片上限的单位是 MB：1 MB 应换算成 1<<20 字节。
func TestImageLimitUnitIsMB(t *testing.T) {
	srv := httptest.NewServer(newFakeWebDAV())
	defer srv.Close()
	fake := srv.Config.Handler.(*fakeWebDAV)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, srv, clip)
	cfg := engine.Config()
	cfg.MaxImageMB = 1
	engine.UpdateConfig(cfg)
	engine.syncCycle(true)

	// 约 16 KB 的图，远小于 1 MB，必须推送
	small := makeNoisePNG(t, 64, 64)
	if len(small) >= 1<<20 {
		t.Fatalf("测试前提不成立：%d 字节", len(small))
	}
	clip.setImage(small)
	engine.checkLocalOnly()
	if _, ok := fake.file(remoteFilePath); !ok {
		t.Error("远小于上限的图片应被推送（单位换算写错了？）")
	}
}
