package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// uiServer 管理配置界面的监听器。
//
// 端口支持运行时切换：用户在界面上改了端口就立刻重新绑定，不必重启程序。
// 监听成功后会把这个地址写进程序目录的 xime-clip-sync.ui，
// 供「第二个实例」找到已经在跑的那个界面。
type uiServer struct {
	handler http.Handler
	log     *LogBuffer
	ctx     context.Context

	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	port int // 正在监听的端口；0 表示没在跑

	// serveErr 记录监听器**意外**退出的原因（正常关闭不算）。
	// 端口还占着、地址还写着，但已经没人应答了——uiStatus 要靠它
	// 区分「在跑」和「起过又没了」，否则会报一个访问不通的地址。
	serveErr error

	urlFile string
}

// uiURLFile 是记录当前配置界面地址的文件（与配置同目录）。
func uiURLFile(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "xime-clip-sync.ui")
}

func newUIServer(ctx context.Context, handler http.Handler, log *LogBuffer, cfgPath string) *uiServer {
	return &uiServer{
		handler: handler,
		log:     log,
		ctx:     ctx,
		urlFile: uiURLFile(cfgPath),
	}
}

// URL 返回当前配置界面地址（未启动时为空串）。
func (u *uiServer) URL() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/", u.port)
}

// Port 返回当前实际监听的端口（0 表示未启动）。
func (u *uiServer) Port() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.port
}

// Failed 报告监听器是否意外退出过。
//
// 正常关闭（Close / Rebind 关掉旧监听器）走的是 http.ErrServerClosed，不算失败。
func (u *uiServer) Failed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.serveErr != nil
}

// listen 在 port 上建立监听。
//
// 端口就是配置里的那一个，绑不上就直接报错——不顺延、也不换随机端口。
// 地址是用户要存成书签的东西，程序悄悄换一个比明说「这个端口用不了」更让人困惑。
//
// 常见的两种绑不上：
//   - 被别的程序占着；
//   - 落在 Windows 的保留端口段里（`netsh int ipv4 show excludedportrange protocol=tcp`，
//     Hyper-V / WSL / Docker 会整段占掉）。
//
// 错误用 %w 包住底层错误：uiFailReasonOf 靠 errors.As 取 WSA 错误码，
// 认出「端口冲突」，托盘气泡里才能显示成人话。换成 %v 会被误报成「未运行」。
func (u *uiServer) listen(port int) (net.Listener, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("端口 %d 不是合法端口号", port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("端口 %d 用不了（被占用，或落在系统保留端口段里）：%w", port, err)
	}
	return ln, nil
}

// serve 在新监听器上开始服务，并把它记为「当前」。
func (u *uiServer) serve(ln net.Listener, port int) {
	srv := &http.Server{Handler: u.handler}

	u.mu.Lock()
	u.ln, u.srv, u.port = ln, srv, port
	// 新监听器起来了，之前的失败不再代表现状。
	// （旧监听器的关闭走 ErrServerClosed，不会把这里的清零又写回去。）
	u.serveErr = nil
	u.mu.Unlock()

	u.writeURLFile()

	go func() {
		<-u.ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go func() {
		err := srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		u.mu.Lock()
		// 只认「当前这个监听器」的错误。
		//
		// 换端口和关闭时，旧监听器是被我们主动关掉的，它的返回值不代表现状。
		// 而且光靠 errors.Is(err, ErrServerClosed) 挡不住：Close() 是先关 ln
		// 再关 srv，Serve 拿到的其实是「连接已关闭」。不比较一下 srv，
		// 用户改一次端口就会让「设置」永久报「已停止」。
		current := u.srv == srv
		if current {
			u.serveErr = err
		}
		u.mu.Unlock()
		if !current {
			return
		}
		u.log.Errorf("配置界面异常退出：%v", err)
	}()
}

// Start 在指定端口上启动配置界面，返回可访问地址。
//
// 端口绑不上时直接返回错误（界面不启动）。调用方要把这个错误明确告诉用户，
// 并引导他去改配置文件里的端口——托盘图标看起来一切正常，不说清楚他不会知道。
func (u *uiServer) Start(port int) (string, error) {
	ln, err := u.listen(port)
	if err != nil {
		return "", err
	}
	u.serve(ln, port)
	u.log.Infof("配置界面已启动：%s", u.URL())
	return u.URL(), nil
}

// Rebind 切换到新端口并返回新地址。
//
// 绑定是**同步**做的：端口不可用时直接把错误返回给调用方，
// 界面就能立刻提示「端口被占用」，而不是先把配置存了再发现切不过去。
//
// 旧监听器延迟一小会儿再关：当前这次 HTTP 响应还没送到浏览器，
// 立刻关掉的话用户看到的是「请求失败」，而实际上配置已经保存成功了。
func (u *uiServer) Rebind(port int) (string, error) {
	ln, err := u.listen(port)
	if err != nil {
		return "", err
	}

	u.mu.Lock()
	oldLn, oldSrv := u.ln, u.srv
	u.mu.Unlock()

	u.serve(ln, port)

	go func() {
		time.Sleep(600 * time.Millisecond)
		if oldLn != nil {
			oldLn.Close()
		}
		if oldSrv != nil {
			oldSrv.Close()
		}
	}()

	url := u.URL()
	u.log.Infof("配置界面已切换到 %s", url)
	return url, nil
}

// Close 停止服务并清理地址文件。
func (u *uiServer) Close() {
	u.mu.Lock()
	ln, srv := u.ln, u.srv
	u.ln, u.srv, u.port = nil, nil, 0
	u.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	if srv != nil {
		srv.Close()
	}
	if u.urlFile != "" {
		os.Remove(u.urlFile)
	}
}

// writeURLFile 把当前地址写下来，供第二个实例查找。
func (u *uiServer) writeURLFile() {
	if u.urlFile == "" {
		return
	}
	if err := os.WriteFile(u.urlFile, []byte(u.URL()), 0o644); err != nil {
		u.log.Warnf("写入配置界面地址文件失败：%v", err)
	}
}

// runningUIURL 找出「已经在运行的那个实例」的配置界面地址。
//
// 优先读对方启动时写下的地址文件（只有运行中的实例才知道界面到底起没起来），
// 读不到再退回配置里的端口。两者都拿不到就返回空串，调用方只报告「已在运行」。
func runningUIURL(cfgPath string, cfg Config) string {
	if b, err := os.ReadFile(uiURLFile(cfgPath)); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if cfg.UIPort > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d/", cfg.UIPort)
	}
	return ""
}
