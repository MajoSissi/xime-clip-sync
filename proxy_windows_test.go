//go:build windows

package main

import "testing"

// ProxyServer 在注册表里有两种写法，都要能取出当前协议该用的那个地址。
// 取错的表现是「所有请求都被塞进代理」或「明明配了代理却直连」，都很难自查。
func TestSystemProxyURL(t *testing.T) {
	cases := []struct {
		name   string
		server string
		scheme string
		want   string
	}{
		{"裸 host:port 所有协议共用", "127.0.0.1:7890", "https", "http://127.0.0.1:7890"},
		{"按协议分开时取 https 那一项", "http=127.0.0.1:7890;https=127.0.0.1:7891", "https", "http://127.0.0.1:7891"},
		{"按协议分开时取 http 那一项", "http=127.0.0.1:7890;https=127.0.0.1:7891", "http", "http://127.0.0.1:7890"},
		{"按协议分开但没列出当前协议 → 直连", "http=127.0.0.1:7890", "https", ""},
		{"键名大小写不敏感", "HTTPS=127.0.0.1:7891", "https", "http://127.0.0.1:7891"},
		{"多余的空白要容忍", " http=127.0.0.1:7890 ; https=127.0.0.1:7891 ", "https", "http://127.0.0.1:7891"},
		{"已经带 scheme 也认", "http://127.0.0.1:7890", "https", "http://127.0.0.1:7890"},
		{"空串 → 直连", "", "https", ""},
		{"解析不出主机名 → 直连", "http=://", "https", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := systemProxyURL(c.server, c.scheme)
			if c.want == "" {
				if u != nil {
					t.Fatalf("期望直连（nil），实际 %s", u)
				}
				return
			}
			if u == nil {
				t.Fatalf("期望 %q，实际 nil", c.want)
			}
			if got := u.String(); got != c.want {
				t.Errorf("systemProxyURL = %q，期望 %q", got, c.want)
			}
		})
	}
}

// 「跳过代理」列表。ProxyOverride 默认就带 <local>，忽略了它，
// 局域网自建的 WebDAV 会被硬塞进代理直接连不上。
func TestBypassProxy(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		override string
		want     bool
	}{
		{"空列表不跳过", "example.com", "", false},
		{"星号全部跳过", "example.com", "*", true},
		{"<local> 匹配不带点的主机名", "nas", "<local>", true},
		{"<local> 不匹配带点的域名", "example.com", "<local>", false},
		{"后缀匹配子域", "dav.example.com", "example.com", true},
		{"精确匹配自身", "example.com", "example.com", true},
		{"前导点写法", "dav.example.com", ".example.com", true},
		{"通配前缀写法", "dav.example.com", "*.example.com", true},
		{"分号分隔的多项", "nas", "example.com;<local>", true},
		{"大小写不敏感", "DAV.Example.COM", "example.com", true},
		{"后缀必须落在点上，不是子串", "notexample.com", "example.com", false},
		{"不相关域名不跳过", "other.com", "example.com", false},
		{"主机名为空时不跳过", "", "*", false},
		// 通配符出现在中间——这才是系统里真正会出现的写法
		{"通配符在中间：127.*", "127.0.0.1", "127.*", true},
		{"通配符在中间：192.168.*", "192.168.1.5", "192.168.*", true},
		{"通配符在中间不误伤别的网段", "192.169.1.5", "192.168.*", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bypassProxy(c.host, c.override); got != c.want {
				t.Errorf("bypassProxy(%q, %q) = %v，期望 %v",
					c.host, c.override, got, c.want)
			}
		})
	}
}

// 系统默认的 ProxyOverride 就是这一串（本机实测），用的是「通配符在中间」的写法。
//
// 这条测试是冲着「局域网自建 WebDAV」来的：不认 192.168.* 的话，
// 家里/公司的 WebDAV 会被硬塞进代理，直接连不上，而用户完全看不出为什么。
const realWorldProxyOverride = "localhost;127.*;192.168.*;10.*;172.16.*;172.17.*;172.18.*;" +
	"172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;" +
	"172.28.*;172.29.*;172.30.*;172.31.*;<local>"

func TestBypassProxyRealWorldList(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"192.168.1.5", true},  // 局域网 WebDAV：必须直连
		{"10.0.0.7", true},
		{"172.20.3.9", true},
		{"nas", true},          // <local>：不带点的主机名
		{"172.32.0.1", false},  // 不在列表里
		{"higa.teracloud.jp", false}, // 公网：要走代理
		{"dav.jianguoyun.com", false},
	}
	for _, c := range cases {
		if got := bypassProxy(c.host, realWorldProxyOverride); got != c.want {
			t.Errorf("bypassProxy(%q, 系统默认列表) = %v，期望 %v", c.host, got, c.want)
		}
	}
}

// readSystemProxy 读的是真实注册表，只要求「不 panic、能返回」。
// 这里不断言具体值——那取决于开发机的设置。
func TestReadSystemProxyDoesNotPanic(t *testing.T) {
	enable, server, override := readSystemProxy()
	if enable && server == "" {
		t.Error("ProxyEnable=1 却没有 ProxyServer，说明读注册表的键名或类型写错了")
	}
	t.Logf("系统代理：enable=%v server=%q override=%q", enable, server, override)
}
