package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 密码相关的测试：加密落盘、旧配置迁移、界面不回显。
//
// 这些都属于「写错了不会有编译错误，但会静默泄露密码或静默失效」的类别，
// 所以每一条都直接断言磁盘上的字节。

func TestSecretRoundTrip(t *testing.T) {
	for _, plain := range []string{
		"aynwtwbpzhtjaedu",
		"含中文的密码·带符号!@#$%^&*()",
		"a",
		strings.Repeat("x", 512),
	} {
		enc, err := protectSecret(plain)
		if err != nil {
			t.Fatalf("加密 %q 失败：%v", plain, err)
		}
		if enc == "" {
			t.Fatalf("加密 %q 得到空串", plain)
		}
		if enc == plain {
			t.Errorf("密文与明文相同：%q", enc)
		}
		// 短明文的字符会偶然出现在 base64 里（比如 "a"），所以只在
		// 明文够长时才做「不含明文」的判断；真正的落盘检查在
		// TestSaveConfigEncryptsPassword 里对文件字节做。
		if len(plain) >= 8 && strings.Contains(enc, plain) {
			t.Errorf("密文里不该出现明文：%q", enc)
		}
		got, err := unprotectSecret(enc)
		if err != nil {
			t.Fatalf("解密失败：%v", err)
		}
		if got != plain {
			t.Errorf("往返不一致：%q → %q", plain, got)
		}
	}

	// 空串是合法输入，应原样返回空串（表示「没有密码」）
	if enc, err := protectSecret(""); err != nil || enc != "" {
		t.Errorf("空串应原样返回：enc=%q err=%v", enc, err)
	}
	if got, err := unprotectSecret(""); err != nil || got != "" {
		t.Errorf("空密文应返回空串：got=%q err=%v", got, err)
	}
	// 不是 base64 的内容应报错而不是静默返回空密码
	if _, err := unprotectSecret("这不是-base64!!"); err == nil {
		t.Error("非法密文应当报错")
	}
}

// 保存配置后，磁盘上不能出现明文密码。
func TestSaveConfigEncryptsPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	const secret = "SuperSecret-App-Password-123"
	cfg := defaultConfig()
	cfg.DavURL = "https://dav.example.com/dav/"
	cfg.Username = "user@example.com"
	cfg.Password = secret

	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("配置文件里出现了明文密码：\n%s", raw)
	}
	if strings.Contains(string(raw), `"password"`) {
		t.Errorf("配置里不该再有 password 字段：\n%s", raw)
	}

	var onDisk Config
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("落盘配置无法解析：%v", err)
	}
	if onDisk.PasswordEnc == "" {
		t.Fatal("缺少 passwordEnc 字段")
	}
	if onDisk.Password != "" {
		t.Errorf("内存字段 password 不该被写盘：%q", onDisk.Password)
	}

	// 读回来应能还原出明文
	back, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("正常情况下不该有告警：%v", warnings)
	}
	if back.Password != secret {
		t.Errorf("密码未能还原：期望 %q 实际 %q", secret, back.Password)
	}
	if back.Username != cfg.Username || back.DavURL != cfg.DavURL {
		t.Errorf("其余字段被破坏：%+v", back)
	}
}

// 旧配置里的明文密码应被自动加密，并立即从磁盘上抹掉。
func TestLoadConfigMigratesPlaintextPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	const secret = "legacy-plaintext-pw"
	old := `{"davUrl":"https://host/dav/","remotePath":"xime","username":"u",
	         "password":"` + secret + `","enabled":true,"pollSeconds":15}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatalf("读取旧配置失败：%v", err)
	}
	// 内存里要有明文（Basic Auth 需要）
	if cfg.Password != secret {
		t.Errorf("内存里的密码 = %q，期望 %q", cfg.Password, secret)
	}
	if cfg.PasswordEnc == "" {
		t.Error("应生成加密字段")
	}
	if len(warnings) == 0 {
		t.Error("迁移明文密码应给出告警，让用户知道文件被改写了")
	}

	// 关键：磁盘上的明文必须已经消失
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("迁移后磁盘上仍有明文密码：\n%s", raw)
	}
	if !strings.Contains(string(raw), "passwordEnc") {
		t.Errorf("迁移后应写入 passwordEnc：\n%s", raw)
	}

	// 再读一次不应再触发迁移
	_, warnings2, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings2) != 0 {
		t.Errorf("第二次读取不该再有告警：%v", warnings2)
	}
}

// 解不开的密文（换机器/换用户）应给出告警并把密码留空，而不是让程序起不来。
func TestLoadConfigUndecryptablePassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 一段合法 base64 但不是 DPAPI 密文的内容
	broken := `{"davUrl":"https://host/dav/","username":"u","passwordEnc":"YWJjZGVmZw=="}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, warnings, err := loadConfig(path)
	if err != nil {
		t.Fatalf("解不开密码不该导致读取失败：%v", err)
	}
	if cfg.Password != "" {
		t.Errorf("解不开时密码应为空，实际 %q", cfg.Password)
	}
	if len(warnings) == 0 {
		t.Error("解不开密码应给出告警，提示用户重新填写")
	}
}

// 配置文件必须固定在程序（可执行文件）同目录，不能悄悄跑到 %AppData%。
func TestConfigPathIsExeDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取不到可执行文件路径：%v", err)
	}
	got := configPath("")
	want := filepath.Join(filepath.Dir(exe), "config.json")
	if got != want {
		t.Errorf("configPath(\"\") = %q，期望 %q", got, want)
	}
	if filepath.Base(got) != "config.json" {
		t.Errorf("文件名 = %q，期望 config.json", filepath.Base(got))
	}
	// 显式指定时应当原样使用
	if got := configPath("D:/x/y.json"); got != "D:/x/y.json" {
		t.Errorf("显式路径未被采用：%q", got)
	}

	// 日志目录必须是同目录下的 logs/
	if d := logDirPath(want); d != filepath.Join(filepath.Dir(exe), "logs") {
		t.Errorf("日志目录 = %q，期望同目录的 logs/", d)
	}
}

// ---------------------------------------------------------------------------
// 界面：绝不能把密码回显给浏览器
// ---------------------------------------------------------------------------

func newPasswordTestUI(t *testing.T) (*httptest.Server, *SyncEngine, string) {
	t.Helper()
	remote := httptest.NewServer(newFakeWebDAV())
	t.Cleanup(remote.Close)

	clip := &fakeClipboard{}
	engine := newTestEngine(t, remote, clip)

	cfg := testConfig(remote.URL)
	cfg.Password = "stored-secret-pw"
	engine.UpdateConfig(cfg)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	log := NewLogBuffer(10)
	// 保存配置会把日志落到 <配置目录>/logs 下。测试结束前必须释放文件句柄，
	// 否则 Windows 下 TempDir 清理会因「文件被另一进程占用」失败。
	t.Cleanup(func() { log.SetLogFile(LogFileOptions{}) })

	ui := httptest.NewServer(NewServer(engine, log, cfgPath).Handler())
	t.Cleanup(ui.Close)
	return ui, engine, cfgPath
}

func TestAPIConfigNeverReturnsPassword(t *testing.T) {
	ui, _, _ := newPasswordTestUI(t)

	resp, err := http.Get(ui.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(rawBody)

	if strings.Contains(body, "stored-secret-pw") {
		t.Fatalf("接口回显了明文密码：%s", body)
	}
	if strings.Contains(body, "passwordEnc") {
		t.Errorf("接口不该返回密文：%s", body)
	}
	if !strings.Contains(body, `"passwordSaved":true`) {
		t.Errorf("应告知界面「密码已设置」：%s", body)
	}
}

func TestAPISaveWithBlankPasswordKeepsStored(t *testing.T) {
	ui, engine, cfgPath := newPasswordTestUI(t)

	// 界面不会回显明文，所以提交上来的 password 是空的
	body := `{"davUrl":"` + engine.Config().DavURL + `","remotePath":"xime",
	          "username":"u","password":"","deviceName":"pc","enabled":true,
	          "pollSeconds":30,"localPollSeconds":5,
	          "maxTextChars":1000,"logToFile":true,"logRetainDays":7,"uiPort":9099}`
	resp, err := http.Post(ui.URL+"/api/config/save", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("保存失败：HTTP %d", resp.StatusCode)
	}

	if got := engine.Config().Password; got != "stored-secret-pw" {
		t.Errorf("留空密码应保留原值，实际 %q", got)
	}
	// 落盘也不能有明文
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stored-secret-pw") {
		t.Fatalf("落盘出现明文密码：\n%s", raw)
	}

	// 提交新密码则应替换
	body2 := strings.Replace(body, `"password":""`, `"password":"new-pw-42"`, 1)
	resp2, err := http.Post(ui.URL+"/api/config/save", "application/json", strings.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := engine.Config().Password; got != "new-pw-42" {
		t.Errorf("新密码未生效，实际 %q", got)
	}
	raw2, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw2), "new-pw-42") {
		t.Fatalf("落盘出现明文新密码：\n%s", raw2)
	}
}
