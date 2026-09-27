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

	// notify 弹气泡的实现，默认是 Balloon（真 Shell_NotifyIconW）。
	// 留这个字段是为了让「打开配置界面失败时会弹气泡」这条行为可测——
	// 气泡本身要真窗口，测试里既弹不出来也看不见。
	notify func(title, text string)
}

func newTray(cb *trayCallbacks) (trayApp, error) {
	t := &winTray{cb: cb}
	t.notify = t.Balloon
	return t, nil
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

// trayWndProc 处理托盘窗口消息。
func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	trayInstanceMu.Lock()
	t := trayInstance
	trayInstanceMu.Unlock()

	if t == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
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

	icon, iconSize, err := createAppIcon()
	if err != nil {
		return err
	}
	t.icon = icon
	t.logf("托盘图标已就绪：%d×%d（系统要求 %d×%d，来自 img/logo.ico）",
		iconSize, iconSize, smallIconSize(), smallIconSize())

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

// logf 记录托盘自身的运行信息（没接日志时静默丢弃）。
func (t *winTray) logf(format string, args ...any) {
	if t.cb.Logf != nil {
		t.cb.Logf(format, args...)
	}
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
