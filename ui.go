package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// Server 提供本地配置界面（只监听 127.0.0.1）。
type Server struct {
	engine  *SyncEngine
	log     *LogBuffer
	cfgPath string
	// ui 用来在端口被改动时立刻重新绑定；未启动配置界面时为 nil。
	ui *uiServer
}

func NewServer(engine *SyncEngine, log *LogBuffer, cfgPath string) *Server {
	return &Server{engine: engine, log: log, cfgPath: cfgPath}
}

// localOnly 拒绝非本机来源的请求：
//   - 只监听回环地址
//   - 校验 Host 头，防止 DNS rebinding
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		switch strings.ToLower(host) {
		case "127.0.0.1", "localhost", "::1", "[::1]":
		default:
			http.Error(w, "仅允许本机访问", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// warnings 累积「配置已经保存了，但某个附带动作没做成」的提示。
//
// 保存配置会顺带做好几件事（写自启项、切日志文件、重绑界面端口），
// 任何一件失败都只影响它自己，不该让保存整体失败——但**必须让用户看见**，
// 否则就是「设置看起来存上了，实际没生效」，比直接报错更难查。
//
// 早先的写法是 `if warning == "" { warning = ... }`，只保留第一条：
// 用户一次保存同时踩中两件事（比如端口被系统保留 + 日志目录写不了）时，
// 界面上只弹一条，另一条只躺在日志里。这里改成全部拼起来。
type warnings struct{ msgs []string }

func (w *warnings) add(format string, args ...any) {
	w.msgs = append(w.msgs, fmt.Sprintf(format, args...))
}

// String 用「；」拼接，空时返回空串（调用方据此决定要不要带 warning 字段）。
func (w *warnings) String() string { return strings.Join(w.msgs, "；") }

// publicConfig 是给浏览器的配置视图：**不含任何密码材料**，
// 只用一个布尔告诉界面「密码已经设置过了」。
type publicConfig struct {
	Config
	PasswordSaved bool `json:"passwordSaved"`
}

// publicView 去掉配置里的明文密码与密文，并附上「是否已设置密码」。
func publicView(cfg Config) publicConfig {
	saved := cfg.HasPassword()
	cfg.Password = ""
	cfg.PasswordEnc = ""
	return publicConfig{Config: cfg, PasswordSaved: saved}
}

// incomingConfig 解析浏览器提交的配置，并补上只有服务端才知道的东西：
//
//   - 忽略提交上来的 passwordEnc —— 不给浏览器往配置里塞任意 blob 的机会，
//     密文一律由服务端从明文现算
//   - 密码留空表示「不修改」（界面不会回显明文），沿用已保存的那份
func (s *Server) incomingConfig(r *http.Request) (Config, error) {
	var cfg Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		return cfg, err
	}
	cfg.PasswordEnc = ""
	if cfg.Password == "" {
		cfg.Password = s.engine.Config().Password
	}
	cfg.normalize()
	return cfg, nil
}

// Handler 返回配置界面的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	}))

	mux.HandleFunc("/favicon.ico", localOnly(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// 状态
	mux.HandleFunc("/api/status", localOnly(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.engine.Status())
	}))

	// 读取配置（不含密码材料）
	mux.HandleFunc("/api/config", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, publicView(s.engine.Config()))
	}))

	// 保存配置
	mux.HandleFunc("/api/config/save", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cfg, err := s.incomingConfig(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数解析失败：" + err.Error()})
			return
		}
		if err := saveConfig(s.cfgPath, cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败：" + err.Error()})
			return
		}
		s.engine.UpdateConfig(cfg)

		// 应用开机自启设置；失败只提示，不影响其余配置生效
		var warn warnings
		if autostartEnabled() != cfg.Autostart {
			if err := setAutostart(cfg.Autostart); err != nil {
				warn.add("开机自启设置失败：%v", err)
				s.log.Warnf("设置开机自启失败：%v", err)
			} else if cfg.Autostart {
				s.log.Infof("已设置开机自启")
			} else {
				s.log.Infof("已关闭开机自启")
			}
		}

		// 应用日志落盘设置（含按天轮转、保留期清理与单文件上限）
		if err := s.log.SetLogFile(logFileOptions(s.cfgPath, cfg)); err != nil {
			warn.add("日志落盘设置失败：%v", err)
			s.log.Warnf("设置日志落盘失败：%v", err)
		} else if cfg.LogToFile {
			s.log.Infof("日志落盘：%s", logPolicyText(cfg))
		}

		// 端口变了就立刻重新绑定，不用重启程序。
		// 绑定失败（新端口被别的程序占了）时配置照旧保存，只提示一句，
		// 界面继续留在原端口上——不会因为改了个端口就把用户关在门外。
		// 但必须说清后果：新端口已经写进配置，下次启动界面会起不来。
		newUIURL := ""
		if s.ui != nil && cfg.UIPort != s.ui.Port() {
			url, err := s.ui.Rebind(cfg.UIPort)
			if err != nil {
				warn.add("端口 %d 用不了，界面仍留在 %s；请改配置文件里的 uiPort 再重启",
					cfg.UIPort, s.ui.URL())
				s.log.Warnf("切换配置界面端口失败：%v", err)
			} else {
				newUIURL = url
			}
		}

		s.log.Infof("配置已保存到 %s", s.cfgPath)
		resp := map[string]any{"ok": true, "config": publicView(cfg)}
		if w := warn.String(); w != "" {
			resp["warning"] = w
		}
		if newUIURL != "" {
			// 告诉前端「本页所在的端口已经关了」，让它引导用户去新地址
			resp["uiUrl"] = newUIURL
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	// 测试连接
	mux.HandleFunc("/api/test", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cfg, err := s.incomingConfig(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "参数解析失败：" + err.Error()})
			return
		}
		if err := TestConnection(cfg, s.log); err != nil {
			s.log.Errorf("连接测试失败：%v", err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.log.Infof("连接测试成功：%s", cfg.DavURL)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "连接成功，服务器可访问"})
	}))

	// 推送（界面左下角「推送」按钮 / 托盘菜单「推送」）
	mux.HandleFunc("/api/push", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.engine.PushNow(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已推送本地剪贴板"})
	}))

	// 拉取（界面左下角「拉取」按钮 / 托盘菜单「拉取」）
	mux.HandleFunc("/api/pull", localOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.engine.PullNow(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已拉取远端剪贴板"})
	}))

	// 日志
	mux.HandleFunc("/api/logs", localOnly(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"lines": s.log.Lines(300)})
	}))

	// 日志实时推送（SSE）
	mux.HandleFunc("/api/events", localOnly(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "不支持流式响应", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch, cancel := s.log.Subscribe()
		defer cancel()

		status := time.NewTicker(2 * time.Second)
		defer status.Stop()

		send := func(event string, v any) bool {
			data, err := json.Marshal(v)
			if err != nil {
				return true
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		if !send("status", s.engine.Status()) {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case line, ok := <-ch:
				if !ok {
					return
				}
				if !send("log", line) {
					return
				}
			case <-status.C:
				if !send("status", s.engine.Status()) {
					return
				}
			}
		}
	}))

	// 退出配置界面的入口只在托盘菜单里（界面上不再放「退出程序」按钮）。
	// 这里刻意不提供 /api/quit：一个能被网页 POST 的关机接口没必要存在。

	return mux
}
