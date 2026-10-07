//go:build windows

package main

import (
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"unsafe"
)

// Windows 的系统代理设置（「Internet 选项 → 连接 → 局域网设置」）存在注册表里。
//
// **为什么不用 http.ProxyFromEnvironment**：它只读 HTTP_PROXY / HTTPS_PROXY 环境变量。
// 从资源管理器双击启动的 GUI 进程拿不到 shell 里的那些变量，于是「系统代理」这个选项
// 会看起来有、实际不生效——那是最坏的一种失败。浏览器认的就是这里，这里才是
// 「系统代理」四个字的正确来源。
//
// 代价是这里**只认注册表**，不叠加环境变量：两套来源混着用，会让同一个设置
// 在「双击启动」和「从命令行启动」下表现不同，比不生效更难查。
const internetSettingsSubKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// regDWORD 是注册表里的 32 位整数类型（REG_SZ 已在 autostart_windows.go 里定义）。
const regDWORD = 4

// readSystemProxy 读出系统代理设置；enable=false 表示系统没开代理。
func readSystemProxy() (enable bool, server, override string) {
	subKey := utf16Ptr(internetSettingsSubKey)
	if subKey == nil {
		return false, "", ""
	}
	var hKey uintptr
	r, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyQueryValue,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r != 0 {
		return false, "", ""
	}
	defer procRegCloseKey.Call(hKey)

	n, _ := queryRegDWORD(hKey, "ProxyEnable")
	return n != 0, queryRegString(hKey, "ProxyServer"), queryRegString(hKey, "ProxyOverride")
}

// queryRegString 读一个 REG_SZ 值；不存在或读失败返回空串。
func queryRegString(hKey uintptr, name string) string {
	p := utf16Ptr(name)
	if p == nil {
		return ""
	}
	buf := make([]uint16, 2048)
	size := uint32(len(buf) * 2)
	r, _, _ := procRegQueryValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(p)),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// queryRegDWORD 读一个 REG_DWORD 值；不存在、类型不对或读失败返回 false。
func queryRegDWORD(hKey uintptr, name string) (uint32, bool) {
	p := utf16Ptr(name)
	if p == nil {
		return 0, false
	}
	var (
		value uint32
		typ   uint32
		size  uint32 = 4
	)
	r, _, _ := procRegQueryValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&value)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 || typ != regDWORD {
		return 0, false
	}
	return value, true
}

// systemProxyFunc 返回按系统代理设置选路的 Proxy 回调。
//
// 每次请求都重新读一次注册表：用户在「Internet 选项」里改了设置，不用重启程序
// 就能生效。读注册表很便宜，而同步节奏是 30 秒一轮，这点开销可以忽略。
func systemProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		enable, server, override := readSystemProxy()
		if !enable {
			return nil, nil // 系统没开代理 → 直连
		}
		if bypassProxy(req.URL.Hostname(), override) {
			return nil, nil
		}
		return systemProxyURL(server, req.URL.Scheme), nil
	}
}

// systemProxyURL 从 ProxyServer 字符串里取出某个 scheme 对应的代理地址。
//
// 两种格式（后者由「Internet 选项 → 高级」按协议分别设置时写出）：
//
//	127.0.0.1:7890                            所有协议共用一个
//	http=127.0.0.1:7890;https=127.0.0.1:7891   按协议分开
//
// 按协议分开时，没有列出当前 scheme 就直连——与「Internet 选项」的行为一致。
// 取不到（或解析不出来）返回 nil，由调用方当作直连。
func systemProxyURL(server, scheme string) *url.URL {
	server = strings.TrimSpace(server)
	if server == "" {
		return nil
	}
	addr := server
	if strings.Contains(server, "=") {
		addr = ""
		for _, part := range strings.Split(server, ";") {
			k, v, ok := strings.Cut(part, "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), scheme) {
				addr = strings.TrimSpace(v)
				break
			}
		}
	}
	if addr == "" {
		return nil
	}
	// 注册表里存的是裸 host:port（没有 scheme），补一个 http:// 交给 url.Parse。
	// 走 SOCKS 的系统代理（ProxyServer 里写 socks=...）不在支持范围内：
	// 需要 SOCKS 就用「SOCKS5 代理」显式填地址。
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

// bypassProxy 判断 host 是否落在 ProxyOverride（「跳过代理」列表）里。
//
// 支持的写法，与「Internet 选项」一致：
//
//	*             全部直连
//	<local>       不带点的主机名（局域网机器名）
//	127.*         通配符可以出现在任何位置——系统默认列表就是这种形式
//	*.example.com 子域
//	example.com   它自己与它的子域
//
// 这一条对「局域网自建 WebDAV」是必需的：ProxyOverride 的常见内容就是
// `127.*;192.168.*;10.*;<local>`，忽略了它，局域网服务器会被硬塞进代理，
// 直接连不上。
func bypassProxy(host, override string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, raw := range strings.Split(override, ";") {
		pat := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case pat == "":
			continue
		case pat == "*":
			return true
		case pat == "<local>":
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		// 前导点是「这个域及其子域」的老写法，去掉后由下面的后缀规则处理
		pat = strings.TrimPrefix(pat, ".")
		if pat == "" {
			continue
		}
		if matchWildcard(pat, host) {
			return true
		}
		// 不带通配符的裸域名，按「它自己 + 它的子域」理解。
		// 后缀必须落在点上，否则 notexample.com 会被 example.com 误伤。
		if !strings.Contains(pat, "*") && strings.HasSuffix(host, "."+pat) {
			return true
		}
	}
	return false
}

// matchWildcard 按 `*`（匹配任意长度的任意串，含空）匹配主机名。
// 没有 `*` 时退化成全等比较。
func matchWildcard(pat, host string) bool {
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == host
	}
	if !strings.HasPrefix(host, parts[0]) {
		return false
	}
	rest := host[len(parts[0]):]
	for i, p := range parts[1:] {
		if i == len(parts)-2 {
			// 最后一段必须落在末尾
			return strings.HasSuffix(rest, p)
		}
		j := strings.Index(rest, p)
		if j < 0 {
			return false
		}
		rest = rest[j+len(p):]
	}
	return true
}
