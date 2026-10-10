package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const version = "1.3.0"

func main() {
	var (
		cfgFlag      = flag.String("config", "", "配置文件路径（默认与程序同目录的 config.json）")
		openUI       = flag.Bool("open-ui", false, "启动时自动打开配置界面（默认只在托盘后台运行）")
		noUI         = flag.Bool("no-ui", false, "不启动配置界面服务")
		noTray       = flag.Bool("no-tray", false, "不显示系统托盘图标")
		doTest       = flag.Bool("test", false, "测试 WebDAV 连接后退出")
		once         = flag.String("once", "", "执行一次同步后退出：push（推送本地）或 pull（拉取远端）")
		showVer      = flag.Bool("version", false, "显示版本号")
		traySelfTest = flag.Bool("tray-test", false, "开发用：自检托盘菜单命令分发后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("Xime Clip Sync %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	// 必须在创建任何窗口之前声明 DPI 感知：否则在高缩放比的屏幕上，
	// 系统会按比例拉伸界面，托盘图标会被放大成糊图。
	dpiOK := enableDPIAwareness()

	cfgPath := configPath(*cfgFlag)

	cfg, cfgWarnings, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败：%v\n", err)
		os.Exit(1)
	}
	// 是否还没配好服务器。
	//
	// 判断的是「配置是否可用」而不是「配置文件是否存在」：loadConfig 会在首次运行时
	// 就把配置文件生成出来，所以文件存在只说明「跑过一次」，不代表服务器配好了——
	// 用户完全可能一直没填就关掉。
	// 用文件存在性判断会让引导气泡只在第一次启动时弹一次，之后再也不会提醒。
	needSetup := strings.TrimSpace(cfg.DavURL) == ""

	log := NewLogBuffer(500)
	// 日志落盘：无控制台的桌面版出问题时，这是唯一的排查线索。
	// 按天分文件存放，并按保留天数自动清理旧文件。
	if err := log.SetLogFile(logFileOptions(cfgPath, cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "日志文件不可写：%v\n", err)
	}

	// 配置与日志都放在程序目录下（绿色便携），目录不可写会直接影响使用，
	// 所以启动时就明确告警，而不是等用户点保存时才失败。
	if dir := filepath.Dir(cfgPath); !dirWritable(dir) {
		log.Warnf("程序目录不可写（%s）：配置与日志都无法保存，请把程序放到可写目录", dir)
	}
	for _, w := range cfgWarnings {
		log.Warnf("%s", w)
	}
	// 只记失败：成功是常态，逐条播报没有诊断价值；失败才是「图标为什么发虚」的线索。
	if !dpiOK {
		log.Warnf("DPI 感知未能启用，托盘图标可能被系统拉伸而发虚")
	}
	if cfg.LogToFile {
		log.Infof("日志已落盘：%s（%s）", logDirPath(cfgPath), logPolicyText(cfg))
	} else {
		log.Infof("日志未写入文件，仅保留在内存中")
	}

	// 绿色便携程序经常被整个目录拷来拷去，注册表 Run 项里存的绝对路径会随之失效。
	// 启动时按当前路径补写一次（幂等，无副作用）。
	repairAutostart(cfg, log)

	clip := newClipboard()
	engine := NewSyncEngine(cfg, clip, log)

	// ---- 命令行一次性模式 ----
	if *doTest {
		if err := TestConnection(cfg, log); err != nil {
			fmt.Fprintf(os.Stderr, "连接测试失败：%v\n", err)
			os.Exit(1)
		}
		fmt.Println("连接测试成功")
		return
	}
	if *once != "" {
		switch *once {
		case "push":
			if err := engine.PushNow(); err != nil {
				fmt.Fprintf(os.Stderr, "推送失败：%v\n", err)
				os.Exit(1)
			}
		case "pull":
			if err := engine.PullNow(); err != nil {
				fmt.Fprintf(os.Stderr, "拉取失败：%v\n", err)
				os.Exit(1)
			}
		default:
			fmt.Fprintln(os.Stderr, "-once 只支持 push 或 pull")
			os.Exit(2)
		}
		fmt.Println("完成")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- 单实例保护 ----
	// 两个副本同时同步同一个剪贴板会互相覆盖、来回推送，必须挡住。
	lockPath := filepath.Join(filepath.Dir(cfgPath), "xime-clip-sync.lock")
	release, acquired, lockErr := acquireSingleInstance(lockPath)
	switch {
	case lockErr != nil:
		log.Warnf("单实例检查失败：%v（继续运行）", lockErr)
	case !acquired:
		url := runningUIURL(cfgPath, cfg)
		switch {
		case url == "":
			fmt.Println("Xime Clip Sync 已在运行。")
		case uiReachable(url):
			if err := openBrowser(url); err != nil {
				fmt.Printf("Xime Clip Sync 已在运行，配置界面：%s\n", url)
			} else {
				fmt.Println("Xime Clip Sync 已在运行，已打开其配置界面。")
			}
		default:
			fmt.Printf("Xime Clip Sync 已在运行（配置界面 %s 暂时无响应）\n", url)
		}
		return
	default:
		defer release()
	}

	// ---- 同步循环 ----
	go engine.Start(ctx)

	// ---- 日志保留期清理 ----
	// 启动时清一次，之后每 6 小时清一次（跨天时新文件由写入路径自动切换）。
	go func() {
		log.Cleanup()
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Cleanup()
			}
		}
	}()

	// ---- 配置界面 ----
	// 端口可以在界面上改，所以托盘和提示都通过闭包取「当前」地址，
	// 而不是记下启动那一刻的快照。
	var ui *uiServer
	uiURL := func() string { return "" }
	uiFailed := func() bool { return false }
	// 配置界面起不来时的**简短**原因，用在托盘气泡和「设置」打不开时的提示里。
	uiFailReason := ""
	if !*noUI {
		srv := NewServer(engine, log, cfgPath)
		ui = newUIServer(ctx, srv.Handler(), log, cfgPath)
		srv.ui = ui // 保存配置时要用它切换端口
		if _, err := ui.Start(cfg.UIPort); err != nil {
			// 端口是配置里的固定值，绑不上界面就直接起不来。这里必须把话说全：
			// 改哪个文件的哪一项、改完要重启——托盘图标看起来一切正常，
			// 用户不会自己想到是端口的事。
			log.Errorf("配置界面没有启动（%v）。请修改 %s 里的 uiPort（当前 %d）换一个没被占用的端口，再重新运行程序",
				err, cfgPath, cfg.UIPort)
			uiFailReason = uiFailReasonOf(err)
			ui = nil
		} else {
			uiURL = ui.URL
			uiFailed = ui.Failed
		}
	}
	defer func() {
		if ui != nil {
			ui.Close()
		}
	}()

	// uiStatus 报告配置界面当前能不能打开，供托盘的「设置」和引导气泡用。
	// 返回 (地址, 简短原因)：地址非空就能直接打开，否则原因说明为什么不行。
	uiStatus := func() (string, string) {
		url := uiURL()
		if url == "" {
			return "", uiFailReason
		}
		if uiFailed() {
			// 起过，但服务已经异常退出：地址还留着，可那个地址已经没人应答了。
			// 照报一个访问不通的端口比不报还误导——用户会以为是浏览器的问题。
			return "", "已停止"
		}
		return url, ""
	}

	// ---- 系统托盘 ----
	var tray trayApp
	if !*noTray {
		tray, err = startTray(ctx, engine, cfgPath, uiStatus, stop, log)
		if err != nil {
			log.Warnf("托盘图标不可用：%v（程序继续在后台运行）", err)
			tray = nil
		}
	}

	// 默认静默驻留托盘，不弹任何窗口。
	// 还没配置服务器时用托盘气泡提示用户怎么进配置界面——
	// 无控制台的构建里，气泡是唯一能主动告诉用户「我在哪」的手段。
	if needSetup {
		switch url, reason := uiStatus(); {
		case url != "" && tray != nil:
			log.Infof("尚未配置 WebDAV 服务器，已用托盘气泡提示用户打开配置界面")
			tray.Balloon("Xime Clip Sync 已在后台运行",
				"还没有配置 WebDAV 服务器。双击托盘图标，或右键选择「设置」开始配置。")
		case url != "":
			log.Warnf("尚未配置 WebDAV 服务器：请访问 %s 完成配置", url)
		default:
			// 既没配服务器、配置界面又起不来 —— 这时用户面对的是一个完全没反应的
			// 托盘图标，连从哪下手都不知道。必须主动说出来。
			log.Errorf("尚未配置 WebDAV 服务器，且配置界面起不来（%s）", reason)
			if tray != nil {
				tray.Balloon("配置界面没有启动",
					"还没有配置 WebDAV 服务器，配置界面也起不来（"+reason+"）。详见日志。")
			}
		}
	} else if uiFailReason != "" && tray != nil {
		// 服务器配好了、只是配置界面起不来：托盘图标看起来一切正常，
		// 用户点「设置」也只会得到一句「不可用」，不知道要改什么。
		// 气泡里直接给出「改哪儿、然后重启」。
		tray.Balloon("配置界面没有启动", fmt.Sprintf(
			"端口 %d 用不了（%s）。请修改 config.json 里的 uiPort，"+
				"换一个没被占用的端口后重新运行。", cfg.UIPort, uiFailReason))
	}

	// 显式要求时才打开配置界面
	if *openUI && uiURL() != "" {
		if err := openBrowser(uiURL()); err != nil {
			log.Warnf("无法打开浏览器，请手动访问 %s", uiURL())
		}
	}

	if *traySelfTest {
		runTraySelfTest(tray, log)
	}

	<-ctx.Done()
	log.Infof("正在退出…")
	if tray != nil {
		tray.Stop()
	}
	time.Sleep(300 * time.Millisecond)
}

// startTray 创建并运行托盘图标。
//
// uiStatus 报告配置界面的当前情况：返回 (地址, 简短原因)，
// 地址非空表示可以打开，否则原因说明为什么不行。
// 传的是**取值函数**而不是快照：用户在界面上改了端口之后，
// 托盘里的「设置」必须跟到新地址去。
func startTray(ctx context.Context, engine *SyncEngine, cfgPath string, uiStatus func() (string, string), quit func(), log *LogBuffer) (trayApp, error) {
	cb := &trayCallbacks{
		OpenUI: func() error {
			url, reason := uiStatus()
			if url == "" {
				log.Warnf("配置界面不可用（%s），无法打开", reason)
				if reason == "" {
					reason = "原因不明"
				}
				return fmt.Errorf("配置界面不可用（%s），详见日志", reason)
			}
			// 先记日志再开浏览器：开浏览器失败也不影响「用户确实触发了」这条事实，
			// 而且无控制台的构建里只有日志能观察到托盘手势生效了。
			log.Infof("打开配置界面：%s", url)
			if err := openBrowser(url); err != nil {
				log.Warnf("打开配置界面失败：%v", err)
				return fmt.Errorf("无法打开浏览器：%v", err)
			}
			return nil
		},
		Push: func() {
			// 成功不再单独打一行：pushRemote 已经记了「⬆ 推送（N 字符）」。
			if err := engine.PushNow(); err != nil {
				log.Errorf("手动推送失败：%v", err)
			}
		},
		Pull: func() {
			// 成功不再单独打一行：有变化时 pullRemote 记「⬇ 拉取（N 字符）」。
			// 没变化就什么都不记——这条路径每个轮询周期都会走，不能刷屏。
			if err := engine.PullNow(); err != nil {
				log.Errorf("手动拉取失败：%v", err)
			}
		},
		Quit: quit,
		MenuStatus: func() trayMenuStatus {
			// 状态现取：每次弹菜单都按当下的同步状态渲染，不用缓存。
			return trayMenuStatus{
				Sync: trayMenuSyncLabel(engine.Status().State),
			}
		},
		Tooltip: func() string { return trayTooltip(engine) },
		Log:     log.Infof,
		Warn:    log.Warnf,
	}

	t, err := newTray(cb)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := t.Run(); err != nil {
			log.Warnf("托盘图标运行失败：%v", err)
		}
	}()

	// 定时刷新悬停提示
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.SetTooltip(trayTooltip(engine))
			}
		}
	}()
	return t, nil
}

// trayTooltip 生成托盘悬停提示。
func trayTooltip(engine *SyncEngine) string {
	return "Xime Clip Sync · " + trayStatusLabel(engine)
}

// trayStatusLabel 把同步状态渲染成一个极短的状态词（不带程序名）。
//
// 同时给悬停提示和右键菜单顶部的状态行用，所以措辞要两边都成立：
// 统一压到 4 个字以内，一律是「状态词」而不是「一句话」。
// 具体原因（哪次请求失败、退避到几点）留给配置界面的「运行状态」页，
// 那里有地方写清楚，也抄得下来。
func trayStatusLabel(engine *SyncEngine) string {
	st := engine.Status()
	switch st.State {
	case "ok":
		return "同步中"
	case "disabled":
		return "已停用"
	case "stopped":
		return "未配置"
	case "backoff":
		return "限流退避"
	case "error":
		return "同步出错"
	default:
		return "状态未知"
	}
}

// uiFailReasonOf 把配置界面启动失败的原因压成一句话里放得下的两三个字。
//
// Start 的失败只有一种来源：端口绑不上（listen 不做顺延，绑不上就是绑不上）。
// 但用户能采取的行动不同，所以还是按错误码分一下：
// 端口冲突能去改配置文件里的 uiPort，「未运行」只能看日志。
func uiFailReasonOf(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// Windows 给的是 WSA 错误码，不是 POSIX 的 EADDRINUSE：
		//   10048 WSAEADDRINUSE  地址已被占用
		//   10013 WSAEACCES      落在系统保留段里，绑定被拒
		// 对用户来说都是「这个端口用不了」，气泡里统一说「端口冲突」。
		switch uintptr(errno) {
		case 10048, 10013:
			return "端口冲突"
		}
	}
	return "未运行"
}

// runTraySelfTest 向托盘窗口投递菜单命令，验证消息分发链路是否通畅。
func runTraySelfTest(tray trayApp, log *LogBuffer) {
	tester, ok := tray.(traySelfTestable)
	if !ok {
		log.Warnf("[自检] 当前平台不支持托盘自检")
		return
	}
	go func() {
		// 自检只投「会真的做点事」的项：
		//   - 「设置」会拉起浏览器，没必要为了验分发链路弹个窗口出来；
		//   - 「退出」单独放在最后投，因为它是终点。
		steps := []struct {
			id   int
			name string
		}{
			{trayIDPush, "推送"},
			{trayIDPull, "拉取"},
		}
		time.Sleep(700 * time.Millisecond)
		for _, s := range steps {
			log.Infof("[自检] 投递菜单命令：%s", s.name)
			tester.PostCommand(s.id)
			time.Sleep(700 * time.Millisecond)
		}
		log.Infof("[自检] 投递菜单命令：退出")
		tester.PostCommand(trayIDQuit)
	}()
}

// uiReachable 探测已有实例的配置界面是否可访问。
func uiReachable(url string) bool {
	client := http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url + "api/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// fileExists 判断路径是否为一个已存在的普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// openBrowser 用系统默认浏览器打开 URL。
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
