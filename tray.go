package main

// 托盘菜单项 ID
//
// 菜单里只有「状态」和「动作」，没有同步开关：
//   - 不放「暂停同步 / 启用同步」：托盘图标缩在任务栏角落，右键菜单又长得很像，
//     误点一下就把同步关了、还看不出是谁关的，代价远大于省一次点击。开关只在配置界面里改。
//   - 顶部放一行同步状态：用户要的是「一眼看出有没有在同步」。悬停提示要等系统弹出来、
//     还会被任务栏里其他图标挤掉，不够「及时发现问题」。状态行是灰色不可点的，见 handleCommand。
//
// 这里曾经还有第二行「配置界面 - 50811 / 端口冲突」。端口固定成 31213 之后，
// 配置界面起不来会在启动日志和托盘气泡里把「改哪个文件的哪一项」说全，
// 菜单里这一行就没必要了。
//
// 那行状态的文案也曾经是配置项（traySyncOkText / traySyncBadText）。后来改回写死：
// 它只是一句状态词，用户改完还得弹一次菜单核对效果，不值得为它开两个配置项。
const (
	trayIDOpenUI = 1
	trayIDPush   = 2
	trayIDPull   = 3
	trayIDQuit   = 4

	// 顶部同步状态（灰色、不可点）。给它 ID 是为了让「菜单里有哪些项」
	// 这件事能被测试断言——AppendMenuW 是 Win32 调用，测试里跑不了也看不出内容。
	trayIDStatusSync = 5
)

// 菜单顶部那行同步状态的文案，**写死**，不开放配置。
//
// 只做二选一：同步在不在正常跑。刻意不做成 5 个状态词——
// 菜单是「扫一眼」的地方，用户要的是结论；想知道是哪种异常看悬停提示，
// 想知道为什么看配置界面「运行状态」页。
const (
	traySyncOkText  = "同步中 🔄"
	traySyncBadText = "未同步 🛑"
)

// trayMenuStatus 是右键菜单顶部状态区的文字。
//
// 每次弹菜单时现取，不用缓存：状态是给人「现在到底行不行」用的，过期就没意义了。
//
// 刻意做成**极短**：菜单是一扫而过的地方，句子长了（尤其是一串
// URL + HTTP 状态码）在弹出菜单里根本读不完。结论放这里，
// 具体是哪种异常看悬停提示，详细原因看配置界面「运行状态」页。
type trayMenuStatus struct {
	// Sync 是同步状态，二选一（「同步中 🔄」「未同步 🛑」）。
	Sync string
}

// trayMenuSyncLabel 渲染菜单里那行状态：只回答「同步在不在正常跑」。
//
// 注意「已停用」（用户自己在界面上关了同步）也算异常——它确实没在同步。
// 这是刻意的：菜单不替用户解释原因，只报事实。
func trayMenuSyncLabel(state string) string {
	if state == "ok" {
		return traySyncOkText
	}
	return traySyncBadText
}

// trayCallbacks 是托盘菜单各项触发的动作。
type trayCallbacks struct {
	// OpenUI 打开配置界面。返回错误时托盘会弹气泡说明原因——
	// 无控制台的构建里，「点了没反应」是最糟的体验：用户既看不到窗口，
	// 也想不到要去翻日志文件。
	OpenUI func() error
	Push   func()
	Pull   func()
	Quit   func()
	// Tooltip 返回悬停提示。
	Tooltip func() string
	// MenuStatus 返回右键菜单顶部的状态文字。
	MenuStatus func() trayMenuStatus
	// Logf 记录托盘自身的运行信息（例如实际采用的图标尺寸）。
	// 没有控制台的构建里，日志是唯一能观察到这些细节的渠道。
	Logf func(format string, args ...any)
}

// trayApp 是系统托盘图标。
type trayApp interface {
	// Run 运行消息循环，阻塞直到 Stop 被调用。
	Run() error
	// Stop 移除托盘图标并结束消息循环。
	Stop()
	// SetTooltip 更新悬停提示文字。
	SetTooltip(string)
	// Balloon 弹出一条气泡通知（用于首次运行的引导提示）。
	Balloon(title, text string)
}

// traySelfTestable 是开发用接口：向托盘窗口投递菜单命令，
// 用于在没有人点鼠标的情况下自检菜单分发链路。
type traySelfTestable interface {
	PostCommand(id int)
}
