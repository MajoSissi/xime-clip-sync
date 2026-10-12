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
	"strings"
	"time"
)

// ErrRateLimited 表示服务器限流（HTTP 503，如坚果云免费版 600 次/30 分钟）。
// 同步引擎据此进入退避，避免继续请求延长封禁窗口。
var ErrRateLimited = errors.New("服务器限流（HTTP 503）")

// maxResponseBytes 限制远端**剪贴板 JSON** 的读取上限，避免异常大文件占满内存。
const maxResponseBytes = 4 << 20 // 4 MiB

// maxBlobResponseBytes 是附件（图片）的读取上限，比 JSON 文件宽得多：
// 图片动辄几百 KB 到几 MB，用 4 MiB 会把稍大的图**静默截断**成坏文件。
//
// 与 config.go 的 maxImageMBLimit（50 MB）联动——配置允许的最大图片必须小于这里，
// 否则用户把上限调满之后，图片传得上去却拉不回来。64 MiB 给 50 MB 留了余量。
const maxBlobResponseBytes = 64 << 20 // 64 MiB

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

// warnf 记一条同步日志的 warn；没接日志时（单元测试）静默丢弃。
//
// 走同步通道：这里记的全是传输层的重试与建目录，属于「同步为什么慢/为什么失败」
// 这一类，正是用户在「同步日志」里要找的东西。
func (c *webdavClient) warnf(format string, args ...any) {
	if c.log != nil {
		c.log.SyncWarnf(format, args...)
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
	status, hdr, _, err := c.doWithBody(method, url, body, extra)
	return status, hdr, err
}

// doWithBody 发送一次请求，并**顺带把响应体读回一小段**。
//
// 成功路径上用不到这段 body；它是给失败路径准备的。WebDAV 服务器在 403 / 409
// 这类拒绝里几乎都会带一句原因（XML 的 <D:error> 或纯文本），早先直接
// io.Discard 掉，用户就只能对着一个状态码猜「是程序问题还是远端问题」。
func (c *webdavClient) doWithBody(method, url string, body []byte, extra map[string]string) (int, http.Header, string, error) {
	resp, err := c.request(method, url, body, extra)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyMax))
	// 再把剩余读掉，让连接能被复用（不读干净的话 Go 会直接关掉这条连接）。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, compactSnippet(head), nil
}

// errBodyMax 是错误信息里附带的服务端响应体上限（字符）。
//
// 只留一小段：目的是让用户看到服务器**为什么**拒绝（403/409 的说明通常就一两行），
// 不是把整个响应体搬进界面提示。
const errBodyMax = 400

// compactSnippet 把响应体压成适合塞进错误信息的一行短文本。
//
// XML/HTML 的尖括号会破坏界面排版，统一压成空格；换行与连续空白也压成一个空格。
func compactSnippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	// 按 rune 截断，避免把多字节字符劈成半个
	if r := []rune(s); len(r) > errBodyMax {
		s = string(r[:errBodyMax]) + "…"
	}
	return s
}

// isOKStatus 判断 2xx。
func isOKStatus(status int) bool { return status >= 200 && status < 300 }

// isMissingParentStatus 判断「PUT 的目标目录不存在」这类状态码。
//
// 409 是标准答案；404 见 Alist / Nextcloud 等（对 PUT 缺父目录返回 404）；
// 403 见部分坚果云与反向代理后的 WebDAV——它们对「往不存在的目录里写文件」
// 不给 409 而给 403。插件 main.ts 只处理了前两种，所以这里多兜一种。
//
// 把 403 也纳入**不会掩盖真问题**：目录建不起来时 ensureDirectories 会失败，
// 调用方会把原始状态和建目录的失败一起报出来（见 putWithDirRetry）。
func isMissingParentStatus(status int) bool {
	return status == http.StatusConflict ||
		status == http.StatusNotFound ||
		status == http.StatusForbidden
}

// statusHint 给常见状态码补一句人话，帮用户判断该改配置还是该等服务器。
// 拿不准的返回空串——不硬编解释，免得把用户带偏。
func statusHint(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "认证失败，请检查用户名与密码（坚果云要用「应用密码」，不是登录密码）"
	case http.StatusForbidden:
		return "服务器拒绝写入：可能是父目录不存在且未能自动创建、账号无写入权限，或服务器策略限制"
	case http.StatusConflict:
		return "父目录不存在"
	case http.StatusRequestEntityTooLarge:
		return "文件超过服务器允许的体积上限"
	case http.StatusInsufficientStorage:
		return "服务器存储空间不足或超出配额"
	case http.StatusTooManyRequests:
		return "请求过于频繁，被服务器限流"
	default:
		return ""
	}
}

// httpStatusError 把非 2xx 状态码拼成给用户看的错误：状态码 + 人话 + 服务端原话。
func httpStatusError(action string, status int, body string) error {
	msg := fmt.Sprintf("%s（HTTP %d）", action, status)
	if hint := statusHint(status); hint != "" {
		msg += "：" + hint
	}
	if body != "" {
		msg += "；服务器返回：" + body
	}
	return errors.New(msg)
}

// putWithDirRetry 处理「父目录不存在」类失败：逐级 MKCOL 建目录后重试一次原请求。
//
// status/body 是**首次**尝试的结果。目录建不起来时把两者一并报出——
// 这样 403 究竟是「目录不存在」还是「账号没写权限」，用户从错误信息里就能分辨，
// 而不是被一句「上传失败」糊弄过去。
func (c *webdavClient) putWithDirRetry(
	action, method, url string, payload []byte, extra map[string]string, status int, body string,
) (int, http.Header, string, error) {
	c.warnf("%s %s → HTTP %d（疑似父目录不存在），逐级 MKCOL 后重试", method, url, status)
	if err := c.ensureDirectories(url); err != nil {
		return 0, nil, body, fmt.Errorf("%w；尝试自动创建目录也失败：%v",
			httpStatusError(action, status, body), err)
	}
	return c.doWithBody(method, url, payload, extra)
}

// put 写入剪贴板文件。返回响应头（用于缓存 ETag）。
func (c *webdavClient) put(payload []byte) (http.Header, error) {
	url, ok := c.fileURL()
	if !ok {
		return nil, errors.New("未配置 WebDAV 服务器地址")
	}
	headers := map[string]string{"Content-Type": "application/json"}

	status, hdr, body, err := c.doWithBody(http.MethodPut, url, payload, headers)
	if err != nil {
		return nil, err
	}
	if isOKStatus(status) {
		return hdr, nil
	}
	if status == http.StatusServiceUnavailable {
		return nil, ErrRateLimited
	}
	// 父目录不存在（409/404/403）→ 逐级 MKCOL 后重试一次
	if isMissingParentStatus(status) {
		status, hdr, body, err = c.putWithDirRetry("上传失败", http.MethodPut, url, payload, headers, status, body)
		if err != nil {
			return nil, err
		}
		if isOKStatus(status) {
			return hdr, nil
		}
		if status == http.StatusServiceUnavailable {
			return nil, ErrRateLimited
		}
	}
	return nil, httpStatusError("上传失败", status, body)
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

// ensureDirURL 从 davURL 之后逐级 MKCOL 创建 dirURL 这一串目录（**含 dirURL 自身**）。
// 已存在（405/301/302/307/308）视为成功，与插件实现一致。
func (c *webdavClient) ensureDirURL(dirURL string) error {
	base := stripTrailingSlashes(c.baseURL)
	if base == "" {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	current := base
	for _, part := range relativePathParts(base, dirURL) {
		current = current + "/" + part
		status, _, body, err := c.doWithBody("MKCOL", current, nil, nil)
		if err != nil {
			return fmt.Errorf("MKCOL %s 失败：%w", current, err)
		}
		switch {
		case isOKStatus(status):
			// 已创建
		case status == 405 || status == 301 || status == 302 || status == 307 || status == 308:
			// 已存在 / 重定向：视为目录可用
		default:
			// 带上服务端原话：MKCOL 被拒（403/409）时它往往直接说明原因，
			// 比如「父目录不存在」「超出配额」。
			return httpStatusError("MKCOL "+current+" 失败", status, body)
		}
	}
	return nil
}

// ensureDirectories 创建 fileURL **所在目录**（含逐级父目录）。
func (c *webdavClient) ensureDirectories(fileURL string) error {
	dir := buildDirURL(fileURL)
	if dir == "" {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	return c.ensureDirURL(dir)
}

// ensureRemoteLayout 确保远端目录结构就位：剪贴板文件所在目录 + 它下面的 blobs 附件目录。
//
// 这是「配置连接时建一次」的入口（见 TestConnection）。建好之后，同步过程中的 PUT
// 就再也不必依赖「父目录不存在会以哪个状态码返回」这个猜测了——HTTP 403 那次故障
// 正是栽在这个猜测上：有的服务器对缺失父目录不给标准的 409，而给 403。
//
// 幂等：目录已存在时 MKCOL 返回 405，被当作成功，所以反复点「测试连接」是安全的。
func (c *webdavClient) ensureRemoteLayout() error {
	fileURL, ok := c.fileURL()
	if !ok {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	dir := buildDirURL(fileURL)
	if dir == "" {
		return errors.New("远程路径非法，取不到所在目录")
	}
	if err := c.ensureDirURL(dir); err != nil {
		return err
	}
	blobs, ok := blobsDirURL(fileURL)
	if !ok {
		return errors.New("远程路径非法，取不到附件目录")
	}
	return c.ensureDirURL(blobs)
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

// contentTypeOf 按附件扩展名给出 Content-Type。
//
// 图片原图直传、不压缩，所以类型只影响服务器/对端怎么理解这段字节。
// 认不出的扩展名退化成二进制流（对端按字节存就行）。与插件 EXT_CONTENT_TYPE 一致。
func contentTypeOf(dataName string) string {
	ext := ""
	if i := strings.LastIndex(dataName, "."); i >= 0 {
		ext = strings.ToLower(dataName[i+1:])
	}
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	case "heic", "heif":
		return "image/heic"
	case "avif":
		return "image/avif"
	default:
		return "application/octet-stream"
	}
}

// putBlob 上传附件字节到 {剪贴板目录}/blobs/{dataName}。
//
// blob 名是内容寻址（<sha256>.png）⇒ 重复 PUT 幂等，因此**不做 HEAD 预检**：
// 省一次请求，也少一次撞坚果云「600 次/30 分钟」限流的机会。
//
// 目录不存在的处理与 current.json 完全同策略（409/404/**403** → 逐级 MKCOL → 重试一次）。
func (c *webdavClient) putBlob(dataName string, data []byte) error {
	fileURL, ok := c.fileURL()
	if !ok {
		return errors.New("未配置 WebDAV 服务器地址")
	}
	url, ok := buildBlobURL(fileURL, dataName)
	if !ok {
		return fmt.Errorf("附件名非法：%q", dataName)
	}
	headers := map[string]string{"Content-Type": contentTypeOf(dataName)}

	status, _, body, err := c.doWithBody(http.MethodPut, url, data, headers)
	if err != nil {
		return err
	}
	if isOKStatus(status) {
		return nil
	}
	if status == http.StatusServiceUnavailable {
		return ErrRateLimited
	}
	if isMissingParentStatus(status) {
		status, _, body, err = c.putWithDirRetry("上传附件失败", http.MethodPut, url, data, headers, status, body)
		if err != nil {
			return err
		}
		if isOKStatus(status) {
			return nil
		}
		if status == http.StatusServiceUnavailable {
			return ErrRateLimited
		}
	}
	return httpStatusError("上传附件失败", status, body)
}

// getBlob 下载附件字节。
//
// 返回的是**二进制原样**（绝不当文本处理）；读取上限用 maxBlobResponseBytes。
func (c *webdavClient) getBlob(dataName string) ([]byte, error) {
	fileURL, ok := c.fileURL()
	if !ok {
		return nil, errors.New("未配置 WebDAV 服务器地址")
	}
	url, ok := buildBlobURL(fileURL, dataName)
	if !ok {
		return nil, fmt.Errorf("附件名非法：%q", dataName)
	}
	resp, err := c.request(http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, ErrRateLimited
	case resp.StatusCode == http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("远端附件不存在：%s", dataName)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("下载附件失败（HTTP %d）", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBlobResponseBytes))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("远端附件为空")
	}
	return data, nil
}
