//go:build windows

package main

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows 托盘图标：纯 syscall 实现（Shell_NotifyIconW + 隐藏消息窗口），
// 不依赖 cgo 或第三方库，保持单文件分发。

var (
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")

	// 「外壳重建了任务栏」这条消息没有固定编号，只能用一个约定的名字换出来。
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")

	shell32              = syscall.NewLazyDLL("shell32.dll")
	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTrayCallback  = wmApp + 1

	// 电源事件广播（挂起前询问 / 唤醒等），wParam 说明是哪种事件。
	wmPowerBroadcast      = 0x0218
	pbtApmResumeAutomatic = 0x0012 // 系统自动唤醒（定时器等）
	pbtApmResumeSuspend   = 0x0007 // 用户操作唤醒

	// 窗口类风格：只有带 CS_DBLCLKS，系统才会把「快速两次按下」合成
	// WM_LBUTTONDBLCLK 发过来；否则只会收到两次 WM_LBUTTONUP。
	csDblClks = 0x0008

	mfString    = 0x0000
	mfGrayed    = 0x0001 // 灰色、不可点击：菜单顶部的状态行用它
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	// 气泡通知的图标类型
	niifInfo = 0x00000001

	// szTip 为 128 个 WCHAR，末尾要留 NUL
	maxTipRunes = 127
	// szInfo / szInfoTitle 的容量（同样要留 NUL）
	maxInfoRunes  = 255
	maxTitleRunes = 63
)

type pointT struct {
	x int32
	y int32
}

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      pointT
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

// 当前托盘实例。WndProc 是 C 回调，无法携带上下文，只能用包级变量。
var (
	trayInstanceMu sync.Mutex
	trayInstance   *winTray
)

type winTray struct {
	cb   *trayCallbacks
	hwnd uintptr
	icon uintptr
	nid  notifyIconDataW
	mu   sync.Mutex

	// taskbarCreatedMsg 是外壳那条「任务栏已重建」广播的消息号
	// （RegisterWindowMessageW 换出来的，0 表示没换到）。Run 里写一次，之后只读。
	taskbarCreatedMsg uint32

	// notify 弹气泡的实现，默认是 Balloon（真 Shell_NotifyIconW）。
	// 留这个字段是为了让「打开配置界面失败时会弹气泡」这条行为可测——
	// 气泡本身要真窗口，测试里既弹不出来也看不见。
	notify func(title, text string)

	// reAdd 是「重新把图标交给外壳」的实现，默认是 shellReAddIcon。
	// 留这个字段的理由和 notify 一样：测试里既加不了真图标，
	// 也不好断言「外壳丢了图标之后确实补了一次」，把动作换成假的才测得到。
	reAdd func() error
}

func newTray(cb *trayCallbacks) (trayApp, error) {
	t := &winTray{cb: cb}
	t.notify = t.Balloon
	t.reAdd = t.shellReAddIcon
	return t, nil
}

// logf / warnf 记一条托盘自己的日志（没接回调时静默忽略）。
func (t *winTray) logf(format string, args ...any) {
	if t.cb != nil && t.cb.Log != nil {
		t.cb.Log(format, args...)
	}
}

func (t *winTray) warnf(format string, args ...any) {
	if t.cb != nil && t.cb.Warn != nil {
		t.cb.Warn(format, args...)
	}
}

// trayClick 是托盘图标上的鼠标动作。
type trayClick int

const (
	trayClickNone trayClick = iota
	trayClickMenu
	trayClickOpenUI
)

// trayClickAction 决定一条托盘鼠标消息该触发什么。
//
// 单击（WM_LBUTTONUP）**有意什么都不做**：托盘图标就缩在任务栏角落，
// 误触概率很高，一碰就弹出浏览器非常烦人；打开配置界面改成双击。
//
// 注意：只有窗口类带 CS_DBLCLKS，系统才会把「快速两次按下」合成
// WM_LBUTTONDBLCLK 发过来，否则只会收到两次 WM_LBUTTONUP。
func trayClickAction(lparam uintptr) trayClick {
	switch lparam {
	case wmRButtonUp:
		return trayClickMenu
	case wmLButtonDblClk:
		return trayClickOpenUI
	}
	return trayClickNone
}

// trayWindowClass 组装托盘隐藏窗口的窗口类。
func trayWindowClass(hInst uintptr, className *uint16, proc uintptr) wndClassExW {
	return wndClassExW{
		cbSize: uint32(unsafe.Sizeof(wndClassExW{})),
		// 带 CS_DBLCLKS 才会收到 WM_LBUTTONDBLCLK（双击打开配置界面靠它）
		style:         csDblClks,
		lpfnWndProc:   proc,
		hInstance:     hInst,
		lpszClassName: className,
	}
}

// trayMsgAction 是一条窗口消息该引起的动作。
type trayMsgAction int

const (
	trayMsgNone trayMsgAction = iota
	// trayMsgRestoreIcon：图标可能被外壳丢掉了，重新交一次。
	trayMsgRestoreIcon
)

// trayWindowAction 判断一条窗口消息是不是「外壳把托盘图标丢了」的信号。
//
// **托盘图标不是进程画出来的，是外壳（explorer.exe）那里的一份登记。** 外壳只要重建了
// 任务栏——重启 explorer、从睡眠/休眠里醒过来、分辨率或缩放变化后重建——就会把所有
// 第三方图标一起丢掉，而**进程这边收不到任何错误**：Shell_NotifyIcon 不报错，消息循环
// 也没有异常。表现就是「托盘图标没了，但程序还在跑」，用户只能重启程序才找得回来。
//
// 两个信号都要接：
//
//   - TaskbarCreated：外壳重建完任务栏后广播的消息（编号由 RegisterWindowMessageW 换来，
//     同一个名字在所有进程里换到同一个编号，所以对得上）。
//   - WM_POWERBROADCAST + 唤醒事件：唤醒这一瞬间外壳往往正在重建任务栏，
//     而那条 TaskbarCreated 很可能**在我们被挂起的时候就广播过了**，只等它一定会漏。
//
// taskbarCreated 为 0（注册失败）时一律不认：0 就是 WM_NULL，消息循环里到处都是，
// 误判会变成「图标删了又加」没完没了。
func trayWindowAction(msg, wparam, taskbarCreated uint32) trayMsgAction {
	if taskbarCreated != 0 && msg == taskbarCreated {
		return trayMsgRestoreIcon
	}
	if msg == wmPowerBroadcast && (wparam == pbtApmResumeAutomatic || wparam == pbtApmResumeSuspend) {
		return trayMsgRestoreIcon
	}
	return trayMsgNone
}

// registerTaskbarCreated 换出「任务栏已重建」那条广播的消息号（0 表示没换到）。
func registerTaskbarCreated() uint32 {
	name := utf16Ptr("TaskbarCreated")
	if name == nil {
		return 0
	}
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name)))
	return uint32(r)
}

// handleWindowMessage 处理那些「图标可能被外壳丢了」的消息，返回 true 表示已消化。
func (t *winTray) handleWindowMessage(msg, wparam uint32) bool {
	if trayWindowAction(msg, wparam, t.taskbarCreatedMsg) != trayMsgRestoreIcon {
		return false
	}
	t.reAddIcon()
	return true
}

// reAddIcon 重新把图标交给外壳，并把结果记进日志。
//
// 记日志是刻意的：补图标这件事用户看不见（补上了就等于「什么都没发生过」），
// 但**日志里必须留痕**——否则「托盘图标偶尔消失」到底是外壳丢的、还是压根没加上，
// 事后完全没法判断。
func (t *winTray) reAddIcon() {
	if t.reAdd == nil {
		return
	}
	if err := t.reAdd(); err != nil {
		t.warnf("系统重建了任务栏，托盘图标重新添加失败：%v", err)
		return
	}
	t.logf("系统重建了任务栏，已重新添加托盘图标")
}

// shellReAddIcon 是 reAdd 的默认实现。
//
// 先 NIM_DELETE 再 NIM_ADD：外壳是按 (hWnd, uID) 认这份登记的，图标还在时直接
// NIM_ADD 不保证会替换（文档没有承诺），先删一次才能保证「加」真的生效。
// 删一个不存在的图标是无害的，失败也不当错误。
func (t *winTray) shellReAddIcon() error {
	t.mu.Lock()
	nid := t.nid
	t.mu.Unlock()
	if nid.hWnd == 0 {
		return nil // 还没启动完，没有图标要补
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	if r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
		return fmt.Errorf("Shell_NotifyIconW 添加图标失败：%w", err)
	}
	return nil
}

// trayWndProc 处理托盘窗口消息。
func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	trayInstanceMu.Lock()
	t := trayInstance
	trayInstanceMu.Unlock()

	if t == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	}

	// 先判「要不要补图标」：TaskbarCreated 的消息号是运行期才拿到的，
	// 写不进下面的 switch，只能在这里对一次。
	if t.handleWindowMessage(uint32(msg), uint32(wparam)) {
		return 0
	}

	switch msg {
	case wmTrayCallback:
		switch trayClickAction(lparam) {
		case trayClickMenu:
			t.showMenu()
		case trayClickOpenUI:
			t.openUI()
		}
		return 0
	case wmCommand:
		t.handleCommand(int(wparam & 0xffff))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// Run 创建隐藏窗口与托盘图标，并在当前（已锁定）线程上运行消息循环。
func (t *winTray) Run() error {
	// 窗口与其消息循环必须在同一个线程上
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr("XimeClipSyncTrayWindow")
	if className == nil {
		return fmt.Errorf("构造窗口类名失败")
	}

	wc := trayWindowClass(hInst, className, syscall.NewCallback(trayWndProc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassExW 失败：%w", err)
	}

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败：%w", err)
	}
	t.mu.Lock()
	t.hwnd = hwnd
	t.mu.Unlock()

	// 换出「任务栏已重建」的广播消息号，收到就补一次图标（见 trayWindowAction）。
	// 换不到只是「外壳重建后图标回不来」，不该让整个托盘起不来，所以只记一条 warn。
	if m := registerTaskbarCreated(); m != 0 {
		t.taskbarCreatedMsg = m
	} else {
		t.warnf("注册 TaskbarCreated 消息失败，外壳重建任务栏后托盘图标没法自动恢复")
	}

	icon, err := createAppIcon()
	if err != nil {
		return err
	}
	t.icon = icon

	t.mu.Lock()
	t.nid = notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback,
		hIcon:            icon,
	}
	t.writeTip(t.tooltip())
	nid := t.nid
	t.mu.Unlock()

	if r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
		destroyAppIcon(icon)
		return fmt.Errorf("Shell_NotifyIconW 添加图标失败：%w", err)
	}
	t.mu.Lock()
	t.nid = nid
	t.mu.Unlock()

	// 图标就绪后才暴露实例，避免消息回调读到未初始化的状态
	trayInstanceMu.Lock()
	trayInstance = t
	trayInstanceMu.Unlock()

	// 消息循环
	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) == -1 || r == 0 { // -1 出错，0 是 WM_QUIT
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
	destroyAppIcon(t.icon)
	return nil
}

// window 返回隐藏消息窗口的句柄（0 表示尚未创建完成）。
func (t *winTray) window() uintptr {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hwnd
}

// Stop 结束消息循环并移除托盘图标。
func (t *winTray) Stop() {
	if hwnd := t.window(); hwnd != 0 {
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
	}
}

// PostCommand 向托盘窗口投递一条菜单命令（开发自检用）。
func (t *winTray) PostCommand(id int) {
	if hwnd := t.window(); hwnd != 0 {
		procPostMessageW.Call(hwnd, wmCommand, uintptr(id), 0)
	}
}

// SetTooltip 更新悬停提示。
func (t *winTray) SetTooltip(s string) {
	if t.window() == 0 {
		return
	}
	t.mu.Lock()
	t.writeTip(s)
	nid := t.nid
	t.mu.Unlock()
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// writeTip 把提示文字写入 szTip（调用方需自行加锁）。
func (t *winTray) writeTip(s string) {
	writeUTF16(t.nid.szTip[:], s, maxTipRunes)
}

// Balloon 弹出气泡通知。用于程序静默驻留托盘时向用户传达必要信息
// （例如首次运行该怎么打开配置界面）。
//
// 气泡是异步消失的，不阻塞调用方；即使失败也不影响主流程。
func (t *winTray) Balloon(title, text string) {
	if t.window() == 0 {
		return
	}
	t.mu.Lock()
	t.nid.uFlags |= nifInfo
	t.nid.dwInfoFlags = niifInfo
	writeUTF16(t.nid.szInfoTitle[:], title, maxTitleRunes)
	writeUTF16(t.nid.szInfo[:], text, maxInfoRunes)
	nid := t.nid
	t.mu.Unlock()

	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))

	// 复位信息字段，避免后续 SetTooltip 的 nimModify 反复重弹气泡
	t.mu.Lock()
	t.nid.uFlags &^= nifInfo
	t.nid.dwInfoFlags = 0
	t.mu.Unlock()
}

// writeUTF16 把字符串写进定长 WCHAR 数组（截断并保证 NUL 结尾）。
func writeUTF16(dst []uint16, s string, maxRunes int) {
	for i := range dst {
		dst[i] = 0
	}
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	copy(dst, utf16.Encode(runes))
}

func (t *winTray) tooltip() string {
	if t.cb.Tooltip != nil {
		return t.cb.Tooltip()
	}
	return "Xime Clip Sync"
}

// openUI 打开配置界面（放在独立 goroutine 里，避免阻塞消息循环）。
//
// 打不开时**弹气泡**，而不是只写一条日志：用户点了「设置」却什么都没发生，
// 是最难自查的一种故障——无控制台的构建里日志躺在文件里，没人会去翻。
// 气泡是这时候唯一能主动够到用户的手段。
func (t *winTray) openUI() {
	if t.cb.OpenUI == nil {
		return
	}
	notify := t.notify
	if notify == nil {
		notify = t.Balloon
	}
	go func() {
		if err := t.cb.OpenUI(); err != nil {
			notify("无法打开配置界面", err.Error())
		}
	}()
}

// menuItem 描述右键菜单里的一条项目。
//
// 抽成纯数据是为了让菜单布局能被单测断言：真正的 AppendMenuW 是 Win32 调用，
// 测试里既没法跑也看不出内容，而"顶部那条状态项必须是灰色的"这种需求
// 恰恰是最容易被后续改动破坏的。
type menuItem struct {
	Flags uintptr
	ID    int
	Text  string
}

// trayMenuLayout 计算右键菜单的内容（从上到下）：
//
//	同步中 🔄                ← 状态行（灰色不可点）
//	──────────
//	推送
//	拉取
//	──────────
//	设置
//	退出
//
// 三段：状态 / 高频的手动同步 / 低频的「程序级」入口。
// 状态文字由调用方传入（现取），菜单的**结构**本身是静态的，所以这里只做拼装。
func trayMenuLayout(st trayMenuStatus) []menuItem {
	return []menuItem{
		{mfGrayed, trayIDStatusSync, st.Sync},
		{mfSeparator, 0, ""},
		{mfString, trayIDPush, "推送"},
		{mfString, trayIDPull, "拉取"},
		{mfSeparator, 0, ""},
		{mfString, trayIDOpenUI, "设置"},
		{mfString, trayIDQuit, "退出"},
	}
}

// menuStatus 取当前要显示的状态文字。
//
// 没接回调时渲染一次「同步异常」，而不是留空串：
// 菜单顶部出现一行空白，比写一句「未同步」更让人困惑。
func (t *winTray) menuStatus() trayMenuStatus {
	if t.cb.MenuStatus == nil {
		return trayMenuStatus{Sync: trayMenuSyncLabel("")}
	}
	return t.cb.MenuStatus()
}

// showMenu 弹出右键菜单。
func (t *winTray) showMenu() {
	hwnd := t.window()
	if hwnd == 0 {
		return
	}
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	for _, it := range trayMenuLayout(t.menuStatus()) {
		appendMenu(hMenu, it.Flags, uintptr(it.ID), it.Text)
	}

	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 必须先成为前台窗口，否则点击菜单外部时菜单不会消失
	procSetForegroundWindow.Call(hwnd)
	procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmBottomAlign,
		uintptr(pt.x), uintptr(pt.y),
		0, hwnd, 0,
	)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
}

func appendMenu(hMenu uintptr, flags, id uintptr, text string) {
	var p *uint16
	if text != "" {
		p = utf16Ptr(text)
	}
	procAppendMenuW.Call(hMenu, flags, id, uintptr(unsafe.Pointer(p)))
}

// handleCommand 分发菜单命令。
func (t *winTray) handleCommand(id int) {
	switch id {
	case trayIDOpenUI:
		t.openUI()
	case trayIDPush:
		if t.cb.Push != nil {
			go t.cb.Push()
		}
	case trayIDPull:
		if t.cb.Pull != nil {
			go t.cb.Pull()
		}
	case trayIDQuit:
		if t.cb.Quit != nil {
			go t.cb.Quit()
		}
	case trayIDStatusSync:
		// 状态行是灰色禁用项，鼠标根本选不中；但 Win32 在键盘导航等路径
		// 仍会为禁用项发 WM_COMMAND，所以必须**显式**忽略，
		// 免得哪天有人把状态行的 ID 挪进上面的分支，变成「点了状态行却弹出浏览器」。
	}
}
