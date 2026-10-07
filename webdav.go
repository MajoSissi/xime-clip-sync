package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrRateLimited 表示服务器限流（HTTP 503，如坚果云免费版 600 次/30 分钟）。
// 同步引擎据此进入退避，避免继续请求延长封禁窗口。
var ErrRateLimited = errors.New("服务器限流（HTTP 503）")

// maxResponseBytes 限制远端文件读取上限，避免异常大文件占满内存。
const maxResponseBytes = 4 << 20 // 4 MiB

// transferRetryDelay 是传输层出错后立刻重试一次前的等待。
//
// 网络抖动（DNS 打嗝、连接被中间设备重置、代理刚重启）通常在几百毫秒内自愈，
// 而同步节奏是 30 秒一轮——不重试就要白等半分钟。等一下再试，
// 是为了不把两次尝试打在同一个瞬时故障上。
const transferRetryDelay = 500 * time.Millisecond

// transportErrIsRetriable 判断传输层错误是否值得立刻重试一次。
//
// **超时类错误一律不重试**：重试只会再等一个完整的 Client.Timeout（30 秒），
// 而这段时间里 syncMu 被占着，连本地剪贴板的推送都跟着停摆。
// 这条规则也保证了「重试」只给最坏情况增加 500 毫秒，而不是 30 秒。
func transportErrIsRetriable(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return true
}

// webdavClient 是 WebDAV 直连客户端，对齐插件 main.ts 的请求语义：
//   - Basic Auth（用户名为空时不带 Authorization 头）
//   - PUT 缺父目录返回 409/404 时逐级 MKCOL 后重试一次
//   - GET 带 If-None-Match，304 视为无变更
//   - 连接测试用 PROPFIND（Depth: 0），因为部分服务对 HEAD 返回 503
type webdavClient struct {
	baseURL string
	// remotePath 是远端文件路径（相对 baseURL，具体到文件）
	remotePath string
	username   string
	password   string
	http       *http.Client
	// log 只用来记「传输层出错、正在重试」这一条；单元测试里可以是 nil。
	log *LogBuffer
}

func newWebDAVClient(cfg Config, log *LogBuffer) *webdavClient {
	transport := &http.Transport{
		// 局域网自建 WebDAV 常见自签证书，允许用户显式放宽校验
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
		// 代理策略：nil = 直连，见 proxy.go / proxy_windows.go
		Proxy: proxyFuncForConfig(cfg),
	}
	return &webdavClient{
		baseURL: cfg.DavURL,
		// 走 RemotePathOrDefault：留空时落到默认文件（见 config.go）
		remotePath: cfg.RemotePathOrDefault(),
		username:   cfg.Username,
		password:   cfg.Password,
		log:        log,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// warnf 记一条 warn；没接日志时（单元测试）静默丢弃。
func (c *webdavClient) warnf(format string, args ...any) {
	if c.log != nil {
		c.log.Warnf(format, args...)
	}
}

// fileURL 返回远端剪贴板文件 URL。
func (c *webdavClient) fileURL() (string, bool) {
	return buildFileURL(c.baseURL, c.remotePath)
}

// dirURL 返回远端文件**所在目录**的 URL（用于逐级 MKCOL 建目录）。
func (c *webdavClient) dirURL() (string, bool) {
	u, ok := c.fileURL()
	if !ok {
		return "", false
	}
	return buildDirURL(u), true
}

// authHeaders 构造请求头；用户名为空时按插件语义不带认证头。
func (c *webdavClient) authHeaders() map[string]string {
	h := map[string]string{}
	if c.username != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		h["Authorization"] = "Basic " + cred
	}
	return h
}

// request 发送一次请求；传输层出错时立刻重试一次（见 transportErrIsRetriable）。
//
// 每次尝试都重建 Request：PUT 的 body 是 bytes.Reader，复用同一个已经发过的
// Request 会让 body 停在末尾，第二次发出去的就是空内容。
func (c *webdavClient) request(method, url string, body []byte, extra map[string]string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, url, reader)
		if err != nil {
			return nil, err
		}
		for k, v := range c.authHeaders() {
			req.Header.Set(k, v)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err == nil || attempt > 0 || !transportErrIsRetriable(err) {
			return resp, err
		}
		c.warnf("%s 失败，%s 后重试一次：%v", method, transferRetryDelay, err)
		time.Sleep(transferRetryDelay)
	}
}

// do 发送一次请求并读空 body。
func (c *webdavClient) do(method, url string, body []byte, extra map[string]string) (int, http.Header, error) {
	resp, err := c.request(method, url, body, extra)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, nil
}

// put 写入剪贴板文件。返回响应头（用于缓存 ETag）。
func (c *webdavClient) put(payload []byte) (http.Header, error) {
	url, ok := c.fileURL()
	if !ok {
		return nil, errors.New("未配置 WebDAV 服务器地址")
	}
	headers := map[string]string{"Content-Type": "application/json"}

	status, hdr, err := c.do(http.MethodPut, url, payload, headers)
	if err != nil {
		return nil, err
	}
	switch {
	case status >= 200 && status < 300:
		return hdr, nil
	case status == http.StatusServiceUnavailable:
		return nil, ErrRateLimited
	case status == http.StatusConflict || status == http.StatusNotFound:
		// 目录不存在（部分服务器对 PUT 缺失父目录返回 404）→ 逐级 MKCOL 后重试一次
		if err := c.ensureDirectories(url); err != nil {
			return nil, fmt.Errorf("创建远程目录失败：%w", err)
		}
		status, hdr, err = c.do(http.MethodPut, url, payload, headers)
		if err != nil {
			return nil, err
		}
		if status >= 200 && status < 300 {
			return hdr, nil
		}
		if status == http.StatusServiceUnavailable {
			return nil, ErrRateLimited
		}
		return nil, fmt.Errorf("上传失败（HTTP %d）", status)
	default:
		return nil, fmt.Errorf("上传失败（HTTP %d）", status)
	}
}

// get 读取剪贴板文件。
//
// 返回 status：200 有内容、304 无变更、404 远端尚无文件。
func (c *webdavClient) get(ifNoneMatch string) (body []byte, hdr http.Header, status int, err error) {
	url, ok := c.fileURL()
	if !ok {
		return nil, nil, 0, errors.New("未配置 WebDAV 服务器地址")
	}
	headers := map[string]string{}
	if ifNoneMatch != "" {
		headers["If-None-Match"] = ifNoneMatch
	}
	resp, err := c.request(http.MethodGet, url, nil, headers)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.Header, resp.StatusCode, nil
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.Header, resp.StatusCode, ErrRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("下载失败（HTTP %d）", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, err
	}
	return data, resp.Header, resp.StatusCode, nil
}

// ensureDirectories 从 davURL 之后逐级 MKCOL 创建目录。
// 已存在（405/301/302/307/308）视为成功，与插件实现一致。
func (c *webdavClient) ensureDirectories(fileURL string) error {
	base := stripTrailingSlashes(c.baseURL)
	if base == "" {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	current := base
	for _, part := range relativeDirParts(base, fileURL) {
		current = current + "/" + part
		status, _, err := c.do("MKCOL", current, nil, nil)
		if err != nil {
			return fmt.Errorf("MKCOL %s 失败：%w", current, err)
		}
		switch {
		case status >= 200 && status < 300:
			// 已创建
		case status == 405 || status == 301 || status == 302 || status == 307 || status == 308:
			// 已存在 / 重定向：视为目录可用
		default:
			return fmt.Errorf("MKCOL %s 失败（HTTP %d）", current, status)
		}
	}
	return nil
}

// testConnection 用 PROPFIND（Depth: 0）探测目录，返回 nil 表示配置可用。
// 返回值语义对齐插件 test()：错误消息文本直接展示给用户。
func (c *webdavClient) testConnection() error {
	url, ok := c.dirURL()
	if !ok {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	status, _, err := c.do("PROPFIND", url, nil, map[string]string{"Depth": "0"})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("认证失败（HTTP %d）", status)
	case status == http.StatusServiceUnavailable:
		return errors.New("服务器限流（HTTP 503）：请求过于频繁。坚果云免费版每 30 分钟限 600 次请求，请稍后再试")
	case (status >= 200 && status < 300) || status == 404 || status == 405:
		// 207 Multi-Status / 2xx / 404（目录尚不存在，可创建）均视为连接成功
		return nil
	default:
		return fmt.Errorf("连接失败（HTTP %d）", status)
	}
}

// cacheETag 从响应头提取 ETag，供下次 GET 的 If-None-Match 使用。
func cacheETag(hdr http.Header) string {
	if hdr == nil {
		return ""
	}
	return hdr.Get("ETag")
}
