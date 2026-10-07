package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// 出厂默认必须是「系统代理」：Windows 桌面程序就是这么被期望的。
// 系统没配代理时它等价于直连，所以这个默认值不会凭空改变出网路径。
func TestProxyModeDefault(t *testing.T) {
	if got := defaultConfig().ProxyMode; got != ProxyModeSystem {
		t.Errorf("出厂默认代理模式应为 %q，实际 %q", ProxyModeSystem, got)
	}
}

func TestProxyModeNormalize(t *testing.T) {
	// 合法值原样保留（顺手验证会去掉首尾空格）
	for _, m := range []string{ProxyModeSystem, ProxyModeHTTP, ProxyModeSOCKS5, ProxyModeNone} {
		c := Config{ProxyMode: "  " + m + " "}
		c.normalize()
		if c.ProxyMode != m {
			t.Errorf("代理模式 %q 应保留，实际 %q", m, c.ProxyMode)
		}
	}

	// 空串（老配置里没有这个字段）与拼错的值一律退回默认，不能留在配置里
	for _, bad := range []string{"", "   ", "sock5", "SYSTEM", "direct", "proxy"} {
		c := Config{ProxyMode: bad}
		c.normalize()
		if c.ProxyMode != ProxyModeSystem {
			t.Errorf("非法代理模式 %q 应退回 %q，实际 %q", bad, ProxyModeSystem, c.ProxyMode)
		}
	}
}

func TestProxyURLFromConfig(t *testing.T) {
	cases := []struct {
		name string
		mode string
		addr string
		want string
	}{
		{"HTTP 代理补 http scheme", ProxyModeHTTP, "127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"SOCKS5 补 socks5 scheme", ProxyModeSOCKS5, "127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"用户多写了 http:// 也认", ProxyModeHTTP, "http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"用户多写了 socks5:// 也认", ProxyModeSOCKS5, "socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"地址两端空格会被去掉", ProxyModeSOCKS5, "  proxy.lan:1080  ", "socks5://proxy.lan:1080"},
		// scheme 以**模式**为准：模式选了 HTTP 代理，地址里写 socks5:// 也不改变协议，
		// 否则界面上选的和实际用的会不一致。
		{"scheme 以模式为准", ProxyModeHTTP, "socks5://127.0.0.1:7890", "http://127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.ProxyMode = c.mode
			cfg.ProxyURL = c.addr
			u, err := proxyURLFromConfig(cfg)
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got := u.String(); got != c.want {
				t.Errorf("proxyURLFromConfig = %q，期望 %q", got, c.want)
			}
		})
	}
}

// 选了代理却没填地址：**明确报错**，不静默退回直连。
//
// 悄悄直连比同步报错更糟——用户选代理往往就是为了让流量走代理，
// 结果流量照样裸奔，而他看到的界面一切正常。
func TestProxyWithoutAddressFailsLoudly(t *testing.T) {
	for _, mode := range []string{ProxyModeHTTP, ProxyModeSOCKS5} {
		cfg := defaultConfig()
		cfg.ProxyMode = mode
		cfg.ProxyURL = ""

		fn := proxyFuncForConfig(cfg)
		if fn == nil {
			t.Fatalf("模式 %q 选了代理却没填地址，不该静默直连（返回 nil）", mode)
		}
		req, err := http.NewRequest(http.MethodGet, "https://example.com/dav/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fn(req); err == nil {
			t.Errorf("模式 %q 没填代理地址时应报错", mode)
		}
	}
}

// 地址填了但解析不出主机名，同样要报错，而不是当成直连。
func TestProxyWithUnparsableAddressFails(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyMode = ProxyModeHTTP
	cfg.ProxyURL = "://"

	fn := proxyFuncForConfig(cfg)
	if fn == nil {
		t.Fatal("地址解析失败时不该静默直连（返回 nil）")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/dav/", nil)
	if _, err := fn(req); err == nil {
		t.Error("代理地址解析失败时应报错")
	}
}

// 「不代理」直接返回 nil（Transport 把 nil 当作直连），填了地址也不该生效。
func TestProxyModeNoneIsDirect(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyMode = ProxyModeNone
	cfg.ProxyURL = "127.0.0.1:7890"

	if fn := proxyFuncForConfig(cfg); fn != nil {
		t.Error("「不代理」应返回 nil（直连）")
	}
}

// 手动代理要真的挂到 Transport 上，不能只算了个 URL 就丢掉。
func TestManualProxyReachesTransport(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyMode = ProxyModeSOCKS5
	cfg.ProxyURL = "127.0.0.1:1080"

	client := newWebDAVClient(cfg, NewLogBuffer(10))
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型不对：%T", client.http.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("选了 SOCKS5 代理，Transport.Proxy 却是 nil")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/dav/", nil)
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got == nil || got.String() != "socks5://127.0.0.1:1080" {
		t.Errorf("代理地址 = %v，期望 socks5://127.0.0.1:1080", got)
	}
}

// 「系统代理」在非 Windows 上不存在；在 Windows 上必须给出一个可用的回调，// 而不是 nil（nil 就是直连，会让这个选项静默失效）。
func TestSystemProxyFuncIsWired(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProxyMode = ProxyModeSystem

	fn := proxyFuncForConfig(cfg)
	if fn == nil {
		t.Fatal("「系统代理」不该返回 nil —— 那等于直连，选项会静默失效")
	}
	// 真读一次注册表：不该 panic、不该报错（读不到就当作直连）。
	req, err := http.NewRequest(http.MethodGet, "https://example.com/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := fn(req)
	if err != nil {
		t.Errorf("读系统代理不该报错：%v", err)
	}
	if u != nil {
		// 本机确实配了系统代理时才会走到这里：地址必须是能用的 http(s) 代理。
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Errorf("系统代理 scheme 应为 http/https，实际 %q（%s）", u.Scheme, u)
		}
		if _, err := url.Parse(u.String()); err != nil {
			t.Errorf("系统代理地址无法解析：%v", err)
		}
		t.Logf("本机系统代理：%s", u)
	} else {
		t.Log("本机未配置系统代理（或已跳过代理）→ 直连")
	}
}

// 最硬的一条：请求**真的**经过了代理服务器。
//
// 目标地址故意指向 127.0.0.1:1（必然连不上），代理则是一个会直接应答的
// 正向代理。请求能成功，就只能是走了代理——光断言 Proxy 回调返回了什么，
// 证明不了它真的被用上。
func TestHTTPProxyIsActuallyUsed(t *testing.T) {
	var hits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 正向代理收到的是**绝对** URL；收到相对 URL 说明请求根本没走代理。
		if !r.URL.IsAbs() {
			t.Errorf("代理收到的是相对 URL %q，请求没走代理", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("from-proxy"))
	}))
	defer proxy.Close()

	cfg := testConfig("http://127.0.0.1:1") // 目标不可达
	cfg.ProxyMode = ProxyModeHTTP
	cfg.ProxyURL = strings.TrimPrefix(proxy.URL, "http://")

	client := newWebDAVClient(cfg, NewLogBuffer(10))
	body, _, status, err := client.get("")
	if err != nil {
		t.Fatalf("走代理时不该报错：%v", err)
	}
	if status != http.StatusOK || string(body) != "from-proxy" {
		t.Errorf("应拿到代理的应答，实际 status=%d body=%q", status, body)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Error("代理服务器一次都没被请求到")
	}
}

// 「不代理」时请求必须**不**经过代理：同一个正向代理，这里应该一个请求都收不到。
func TestProxyModeNoneBypassesProxy(t *testing.T) {
	var hits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	cfg := testConfig("http://127.0.0.1:1") // 目标不可达
	cfg.ProxyMode = ProxyModeNone
	cfg.ProxyURL = strings.TrimPrefix(proxy.URL, "http://")

	client := newWebDAVClient(cfg, NewLogBuffer(10))
	if _, _, _, err := client.get(""); err == nil {
		t.Error("「不代理」时目标不可达应当报错")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("「不代理」时不该经过代理，实际被请求 %d 次", n)
	}
}
