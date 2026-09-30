package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// 代理模式。存进 config.json 的 proxyMode 就是这四个字符串之一。
const (
	// ProxyModeSystem 跟随 Windows 系统代理（「Internet 选项 → 连接 → 局域网设置」）。
	ProxyModeSystem = "system"
	// ProxyModeHTTP 手动指定 HTTP 代理（HTTPS 请求走 CONNECT）。
	ProxyModeHTTP = "http"
	// ProxyModeSOCKS5 手动指定 SOCKS5 代理。
	ProxyModeSOCKS5 = "socks5"
	// ProxyModeNone 直连，不走任何代理。
	ProxyModeNone = "none"
)

// validProxyMode 报告 s 是否是已知的代理模式。
func validProxyMode(s string) bool {
	switch s {
	case ProxyModeSystem, ProxyModeHTTP, ProxyModeSOCKS5, ProxyModeNone:
		return true
	}
	return false
}

// proxyModeLabel 是代理模式在界面与错误信息里的中文名，与下拉框选项一致。
func proxyModeLabel(mode string) string {
	switch mode {
	case ProxyModeSystem:
		return "系统代理"
	case ProxyModeHTTP:
		return "HTTP 代理"
	case ProxyModeSOCKS5:
		return "SOCKS5 代理"
	case ProxyModeNone:
		return "不代理"
	}
	return mode
}

// proxyFuncForConfig 返回 http.Transport 要的 Proxy 回调；返回 nil 表示直连。
//
// 「系统代理」那条路读注册表而不是环境变量，理由见 proxy_windows.go。
func proxyFuncForConfig(cfg Config) func(*http.Request) (*url.URL, error) {
	switch cfg.ProxyMode {
	case ProxyModeSystem:
		return systemProxyFunc()
	case ProxyModeHTTP, ProxyModeSOCKS5:
		u, err := proxyURLFromConfig(cfg)
		if err != nil {
			// 选了代理却没填（或填错）地址：**让请求明确失败**，不静默退回直连。
			// 用户选代理往往就是为了让流量走代理，悄悄直连比同步报错更糟。
			return func(*http.Request) (*url.URL, error) { return nil, err }
		}
		return http.ProxyURL(u)
	default:
		return nil
	}
}

// proxyURLFromConfig 把「代理模式 + 代理地址」拼成 *url.URL。
//
// 代理地址只填 host:port，scheme 由模式决定（http / socks5）；
// 用户多写了 http:// 或 socks5:// 前缀也认，以模式为准。
func proxyURLFromConfig(cfg Config) (*url.URL, error) {
	addr := strings.TrimSpace(cfg.ProxyURL)
	if addr == "" {
		return nil, fmt.Errorf("代理模式为「%s」，但没填代理地址", proxyModeLabel(cfg.ProxyMode))
	}
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	u, err := url.Parse(cfg.ProxyMode + "://" + addr)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("代理地址 %q 无法解析，应形如 127.0.0.1:7890", cfg.ProxyURL)
	}
	return u, nil
}
