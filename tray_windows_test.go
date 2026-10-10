//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// 托盘图标的手势：单击不响应，双击打开配置界面，右键弹菜单。
//
// 这条需求很容易在后续重构里被"顺手改回去"（比如把 OpenUI 挪回
// WM_LBUTTONUP），所以用测试钉死。
func TestTrayClickAction(t *testing.T) {
	cases := []struct {
		name   string
		lparam uintptr
		want   trayClick
	}{
		{"左键单击不响应", wmLButtonUp, trayClickNone},
		{"左键双击打开配置界面", wmLButtonDblClk, trayClickOpenUI},
		{"右键弹菜单", wmRButtonUp, trayClickMenu},
		{"其他鼠标消息不响应", 0x0200 /* WM_MOUSEMOVE */, trayClickNone},
	}
	for _, c := range cases {
		if got := trayClickAction(c.lparam); got != c.want {
			t.Errorf("%s：trayClickAction(%#x) = %d，期望 %d", c.name, c.lparam, got, c.want)
		}
	}
}

// 窗口类必须带 CS_DBLCLKS，否则系统只会送来两次 WM_LBUTTONUP，
// 永远不会有 WM_LBUTTONDBLCLK——双击打开配置界面就会静默失效。
func TestTrayWindowClassHasDblClks(t *testing.T) {
	wc := trayWindowClass(0, nil, 0)
	if wc.style&csDblClks == 0 {
		t.Errorf("窗口类 style=%#x 缺少 CS_DBLCLKS(%#x)，双击消息不会被生成", wc.style, csDblClks)
	}
	if wc.cbSize != uint32(unsafe.Sizeof(wndClassExW{})) {
		t.Errorf("cbSize=%d，与结构体大小不符", wc.cbSize)
	}
}

// 托盘图标不是进程画的，是外壳（explorer.exe）那里的登记：外壳一重建任务栏
// （重启 explorer、从睡眠唤醒、缩放变化），图标就全没了，而进程这边收不到任何错误。
// 所以这两类消息必须被认出来，否则表现就是「图标没了、程序还在跑」。
func TestTrayWindowAction(t *testing.T) {
	// 真实编号是运行期用 RegisterWindowMessageW 换出来的，测试里随便挑个非零值。
	const taskbarCreated = 0xC123

	cases := []struct {
		name   string
		msg    uint32
		wparam uint32
		want   trayMsgAction
	}{
		{"外壳广播「任务栏已重建」", taskbarCreated, 0, trayMsgRestoreIcon},
		{"系统自动唤醒", wmPowerBroadcast, pbtApmResumeAutomatic, trayMsgRestoreIcon},
		{"用户操作唤醒", wmPowerBroadcast, pbtApmResumeSuspend, trayMsgRestoreIcon},
		// 下面这些都不能误判：误判的代价是图标被删掉又加回来（闪一下、任务栏里顺序也乱）
		{"挂起前的询问不是唤醒", wmPowerBroadcast, 0x0000 /* PBT_APMQUERYSUSPEND */, trayMsgNone},
		{"真正进入挂起不是唤醒", wmPowerBroadcast, 0x0004 /* PBT_APMSUSPEND */, trayMsgNone},
		{"托盘鼠标消息", wmTrayCallback, 0, trayMsgNone},
		{"菜单命令", wmCommand, 0, trayMsgNone},
		{"窗口销毁", wmDestroy, 0, trayMsgNone},
	}
	for _, c := range cases {
		if got := trayWindowAction(c.msg, c.wparam, taskbarCreated); got != c.want {
			t.Errorf("%s：trayWindowAction(%#x, %#x) = %d，期望 %d",
				c.name, c.msg, c.wparam, got, c.want)
		}
	}

	// 消息号没换到（注册失败）时一律不认：0 就是 WM_NULL，消息循环里到处都是，
	// 把它当成 TaskbarCreated 会变成「每收到一条空消息就把图标删了重加」。
	if got := trayWindowAction(0, 0, 0); got != trayMsgNone {
		t.Errorf("消息号注册失败（0）时不该把 0 号消息当成 TaskbarCreated，得到 %d", got)
	}
}

// 认出信号之后必须真的去补一次图标，并且留下日志。
//
// 日志是这里唯一能被观察到的结果：图标补回来了就等于「什么都没发生过」，
// 事后翻日志时「外壳丢过图标」这条线索不能缺。
func TestTrayRestoresIconOnShellRebuild(t *testing.T) {
	const taskbarCreated = 0xC123
	var adds int
	var logs []string
	tr := &winTray{
		cb: &trayCallbacks{
			Log:  func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
			Warn: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		},
		taskbarCreatedMsg: taskbarCreated,
		reAdd:             func() error { adds++; return nil },
	}

	// 普通消息不许触发补救（否则每次点菜单都会把图标删了又加）
	if tr.handleWindowMessage(wmCommand, 0) {
		t.Error("WM_COMMAND 被当成了图标补救信号")
	}
	if tr.handleWindowMessage(wmPowerBroadcast, 0x0004 /* PBT_APMSUSPEND */) {
		t.Error("进入挂起被当成了图标补救信号")
	}
	if adds != 0 {
		t.Fatalf("普通消息触发了 %d 次补图标", adds)
	}

	for _, m := range []struct {
		name   string
		msg    uint32
		wparam uint32
	}{
		{"任务栏重建", taskbarCreated, 0},
		{"唤醒恢复", wmPowerBroadcast, pbtApmResumeAutomatic},
	} {
		if !tr.handleWindowMessage(m.msg, m.wparam) {
			t.Errorf("%s：消息没有被消化", m.name)
		}
	}
	if adds != 2 {
		t.Errorf("补图标次数 = %d，期望 2", adds)
	}
	if len(logs) != 2 {
		t.Fatalf("日志条数 = %d，期望 2：%v", len(logs), logs)
	}
	for _, s := range logs {
		if strings.TrimSpace(s) == "" {
			t.Error("补图标留了一条空日志")
		}
		if !strings.Contains(s, "托盘图标") {
			t.Errorf("日志 %q 里看不出是在说托盘图标", s)
		}
	}
}

// 补图标失败必须留痕，而且要带上原因：外壳刚重建那一瞬间 NIM_ADD 可能还失败，
// 静默的话「图标还是没回来」就成了无头案。
func TestTrayRestoreFailureIsLoggedAsWarning(t *testing.T) {
	const taskbarCreated = 0xC123
	var warns []string
	tr := &winTray{
		cb:                &trayCallbacks{Warn: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }},
		taskbarCreatedMsg: taskbarCreated,
		reAdd:             func() error { return errors.New("外壳还没准备好") },
	}

	tr.handleWindowMessage(taskbarCreated, 0)

	if len(warns) != 1 {
		t.Fatalf("失败时记了 %d 条 warn，期望 1 条：%v", len(warns), warns)
	}
	if !strings.Contains(warns[0], "外壳还没准备好") {
		t.Errorf("warn=%q 里没有失败原因", warns[0])
	}
}

// 没接任何回调时不许 panic：托盘可以在没有日志的情况下单独用（自检、测试）。
func TestTrayRestoreWorksWithoutCallbacks(t *testing.T) {
	var adds int
	for _, tr := range []*winTray{
		{}, // cb 为 nil，reAdd 也为 nil
		{cb: &trayCallbacks{}, reAdd: func() error { adds++; return nil }},
	} {
		tr.taskbarCreatedMsg = 0xC123
		tr.handleWindowMessage(0xC123, 0)
	}
	if adds != 1 {
		t.Errorf("接了 reAdd 的那次没被调用（adds=%d）", adds)
	}
}

// 真广播一次「任务栏已重建」，验证隐藏窗口**确实收得到**这条广播。
//
// 上面几条测的是「收到消息之后怎么办」，这条测的是另一个前提：外壳广播时我们收不收得到。
// 它依赖隐藏窗口是个**顶层**窗口（CreateWindowExW 的父窗口传 0）——哪天有人把关掉的
// 父窗口改成 HWND_MESSAGE（消息专用窗口），广播就再也送不到，图标丢了永远补不回来，
// 而上面那些单测会照样全绿。这条是唯一能挡住那种改动的测试。
//
// -short 跳过：它要真的建窗口、加托盘图标，跑起来任务栏里会闪一下图标。
func TestTrayReceivesTaskbarCreatedBroadcast(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：要用真窗口收真广播，跳过")
	}
	msg := registerTaskbarCreated()
	if msg == 0 {
		t.Skip("本机换不到 TaskbarCreated 消息号，跳过")
	}

	restored := make(chan struct{}, 4)
	runErr := make(chan error, 1)
	tr := &winTray{cb: &trayCallbacks{}}
	tr.notify = tr.Balloon
	// 只验分发，把「补图标」换成假的：这条测的是消息到没到，
	// 真的去动托盘图标只会让任务栏闪，验不出更多东西。
	tr.reAdd = func() error { restored <- struct{}{}; return nil }

	go func() { runErr <- tr.Run() }()
	t.Cleanup(tr.Stop)

	deadline := time.Now().Add(5 * time.Second)
	for tr.window() == 0 {
		if time.Now().After(deadline) {
			t.Skipf("这台机器上建不出托盘窗口（多半没有交互桌面），跳过：%v", <-runErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 跟外壳的做法一样：往所有顶层窗口广播这两条消息。
	// 唤醒那条也是真广播——系统从睡眠里醒过来时就是这么发的。
	for _, b := range []struct {
		name      string
		msg       uintptr
		wparam    uintptr
		mayRefuse bool
	}{
		{"TaskbarCreated", uintptr(msg), 0, false},
		{"WM_POWERBROADCAST + 唤醒", wmPowerBroadcast, pbtApmResumeAutomatic, true},
	} {
		r, _, err := procPostMessageW.Call(0xFFFF /* HWND_BROADCAST */, b.msg, b.wparam, 0)
		if r == 0 {
			// 系统消息（小于 WM_USER）不允许由进程广播，我这么伪造一下可能被挡住。
			// 挡住的只是「测试自己假装唤醒」，不影响要接的那条路径（系统自己发得出去）。
			if b.mayRefuse {
				t.Logf("%s 不允许由进程广播（%v），跳过这一条", b.name, err)
				continue
			}
			t.Fatalf("广播 %s 失败：%v", b.name, err)
		}

		select {
		case <-restored:
		case <-time.After(5 * time.Second):
			t.Fatalf("广播 %s 之后托盘没去补图标：隐藏窗口收不到广播（是不是变成消息专用窗口了？）", b.name)
		}

		// 收到广播之后才读这个字段：Run 写它、这里读它，中间隔着上面那次通道收发，
		// 有明确的前后关系（放在循环前面读就是实打实的数据竞争）。
		if tr.taskbarCreatedMsg != msg {
			t.Errorf("Run 里注册的消息号 = %#x，期望 %#x", tr.taskbarCreatedMsg, msg)
		}
	}
}

// 右键菜单从上到下必须是：
//
//	同步中 🔄                ← 状态行（灰色不可点）
//	──────────
//	推送
//	拉取
//	──────────
//	设置
//	退出
//
// 整条菜单一起断言，是因为"顺序"和"分段"本身就是需求：
// 只检查各项存在的话，把「设置」挪到最上面照样能通过。
func TestTrayMenuLayoutOrder(t *testing.T) {
	items := trayMenuLayout(trayMenuStatus{Sync: traySyncOkText})
	want := []menuItem{
		{mfGrayed, trayIDStatusSync, traySyncOkText},
		{mfSeparator, 0, ""},
		{mfString, trayIDPush, "推送"},
		{mfString, trayIDPull, "拉取"},
		{mfSeparator, 0, ""},
		{mfString, trayIDOpenUI, "设置"},
		{mfString, trayIDQuit, "退出"},
	}
	if len(items) != len(want) {
		t.Fatalf("菜单项数=%d，期望 %d：%+v", len(items), len(want), items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("第 %d 项 = %+v，期望 %+v", i+1, items[i], want[i])
		}
	}
}

// 顶部状态行：必须在最上面、必须灰色不可点、必须显示**传进来的**当前状态
// （而不是写死的字符串），并且和下面的动作项用分隔线隔开。
func TestTrayMenuStatusItemsOnTop(t *testing.T) {
	items := trayMenuLayout(trayMenuStatus{Sync: traySyncBadText})
	if len(items) < 2 {
		t.Fatalf("菜单项太少：%+v", items)
	}

	it := items[0]
	if it.ID != trayIDStatusSync || it.Text != traySyncBadText {
		t.Errorf("第 1 项 = %+v，期望 id=%d、文字 %q", it, trayIDStatusSync, traySyncBadText)
	}
	if it.Flags&mfGrayed == 0 {
		t.Errorf("状态行必须是灰色不可点的：%+v", it)
	}
	// 状态段和动作段之间要有分隔线，否则状态行看起来像个能点的按钮
	if items[1].Flags&mfSeparator == 0 {
		t.Errorf("状态行后面应有一条分隔线，实际：%+v", items[1])
	}

	// 反向哨兵：灰色项只允许是那行状态，动作项一律不许灰
	// （灰色项点不动，用户会以为程序卡住了）。
	for _, it := range items {
		if it.Flags&mfSeparator != 0 {
			continue
		}
		if it.ID == 0 {
			t.Errorf("非分隔线项必须有命令 ID：%+v", it)
		}
		if it.Flags&mfGrayed != 0 && it.ID != trayIDStatusSync {
			t.Errorf("只有状态行可以是灰色：%+v", it)
		}
	}
}

// 菜单里每个可点项的 ID 必须互不相同，且不能和状态行撞号。
//
// 这是「点了状态行却弹出浏览器」的唯一防线：handleCommand 的 switch 没有 default，
// 撞号不会报错，只会让状态行悄悄变成一个能点的动作项。
func TestTrayMenuIDsAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for _, it := range trayMenuLayout(trayMenuStatus{Sync: traySyncOkText}) {
		if it.Flags&mfSeparator != 0 {
			continue
		}
		if prev, ok := seen[it.ID]; ok {
			t.Errorf("菜单 ID %d 被 %q 和 %q 同时使用", it.ID, prev, it.Text)
		}
		seen[it.ID] = it.Text
	}
	if _, ok := seen[trayIDStatusSync]; !ok {
		t.Errorf("状态行 id=%d 不在菜单里", trayIDStatusSync)
	}
}

// 没接状态回调时也不能出现空行；接了就用回调现取的值。
func TestTrayMenuStatusSource(t *testing.T) {
	empty := (&winTray{cb: &trayCallbacks{}}).menuStatus()
	if want := trayMenuSyncLabel(""); empty.Sync != want {
		t.Errorf("没有回调时状态行 = %q，期望 %q", empty.Sync, want)
	}
	if strings.TrimSpace(empty.Sync) == "" {
		t.Error("没有回调时状态行成了空行")
	}

	tr := &winTray{cb: &trayCallbacks{MenuStatus: func() trayMenuStatus {
		return trayMenuStatus{Sync: traySyncBadText}
	}}}
	if got := tr.menuStatus(); got.Sync != traySyncBadText {
		t.Errorf("menuStatus = %+v，期望回调返回的值", got)
	}
}

// 状态行只回答「在不在正常同步」：只有 ok 走「同步中」那句，其余一律走「未同步」那句。
//
// 刻意不做成 5 个状态词——菜单是「扫一眼」的地方，用户要的是结论。
func TestTrayMenuSyncLabel(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"ok", traySyncOkText},
		{"disabled", traySyncBadText},
		{"stopped", traySyncBadText},
		{"backoff", traySyncBadText},
		{"error", traySyncBadText},
		// 空状态（没接回调的占位路径）也算异常，不能落到「正常」那句
		{"", traySyncBadText},
	}
	for _, c := range cases {
		if got := trayMenuSyncLabel(c.state); got != c.want {
			t.Errorf("state=%q → %q，期望 %q", c.state, got, c.want)
		}
	}
}

// 状态文案必须**短**，而且不能出现占位符字面量。
//
// 用户明确提过：早先这一行是「配置界面 127.0.0.1:50811」（20 个字），
// 在一扫而过的弹出菜单里根本读不完。现在文案写死在 tray.go 里，更容易被顺手改长，
// 所以这里把它钉住。
//
// 占位符替换（{port} / {reason}）随「配置界面」状态行一起删掉了：菜单里只做原文显示，
// 文案里写个 { 就会**原样**出现在菜单上。
func TestTraySyncTextsAreShort(t *testing.T) {
	const maxRunes = 14

	for _, st := range []string{"ok", "disabled", "stopped", "backoff", "error", ""} {
		label := trayMenuSyncLabel(st)
		if n := len([]rune(label)); n > maxRunes {
			t.Errorf("同步状态 %q 的文案 %q 有 %d 个字，超过 %d", st, label, n, maxRunes)
		}
		if strings.TrimSpace(label) == "" {
			t.Errorf("同步状态 %q 渲染出了空文案", st)
		}
	}
	for _, s := range []string{traySyncOkText, traySyncBadText} {
		if strings.ContainsAny(s, "{}") {
			t.Errorf("状态文案 %q 里有占位符，菜单会原样显示出来", s)
		}
	}
}

// 菜单里也不该再有「暂停同步 / 启用同步」这类同步开关项。
//
// 开关只在配置界面里改：托盘右键菜单离鼠标太近，误点一下就把同步关了，
// 而且关掉之后菜单里没有任何痕迹，用户很难联想到是自己点出来的。
func TestTrayMenuHasNoToggleItem(t *testing.T) {
	for _, it := range trayMenuLayout(trayMenuStatus{Sync: traySyncOkText}) {
		if strings.Contains(it.Text, "暂停") || strings.Contains(it.Text, "启用") {
			t.Errorf("菜单里不该出现同步开关项：%+v", it)
		}
	}
}

// 菜单项的文案（界面上、菜单里都统一成两个字）。
func TestTrayMenuLabels(t *testing.T) {
	want := map[int]string{
		trayIDPush:   "推送",
		trayIDPull:   "拉取",
		trayIDOpenUI: "设置",
		trayIDQuit:   "退出",
	}
	for _, it := range trayMenuLayout(trayMenuStatus{Sync: traySyncOkText}) {
		if label, ok := want[it.ID]; ok {
			if it.Text != label {
				t.Errorf("菜单项 %d 文字=%q，期望 %q", it.ID, it.Text, label)
			}
			delete(want, it.ID)
		}
	}
	for id := range want {
		t.Errorf("菜单里找不到 id=%d 的项", id)
	}
}

// 每个菜单项都必须能分发到对应的回调，未知 ID 与状态行必须什么都不做。
//
// 回调是 `go cb.X()` 起的，所以要等，不能同步断言。
func TestTrayHandleCommandDispatches(t *testing.T) {
	calls := make(chan string, 8)
	tr := &winTray{cb: &trayCallbacks{
		OpenUI: func() error { calls <- "ui"; return nil },
		Push:   func() { calls <- "push" },
		Pull:   func() { calls <- "pull" },
		Quit:   func() { calls <- "quit" },
	}}

	// 状态行是灰色禁用项，鼠标选不中；但 Win32 在键盘导航等路径仍会发 WM_COMMAND，
	// 所以必须验证它被显式忽略（否则「点了状态行却弹出浏览器」）。
	tr.handleCommand(trayIDStatusSync)
	// 未知 ID：防的是将来加项时漏配回调。
	tr.handleCommand(999)

	want := map[int]string{
		trayIDOpenUI: "ui",
		trayIDPush:   "push",
		trayIDPull:   "pull",
		trayIDQuit:   "quit",
	}
	for id := range want {
		tr.handleCommand(id)
	}

	got := map[string]bool{}
	timeout := time.After(3 * time.Second)
	for len(got) < len(want) {
		select {
		case c := <-calls:
			if _, ok := got[c]; ok {
				t.Errorf("回调 %q 被触发了两次", c)
			}
			got[c] = true
		case <-timeout:
			t.Fatalf("超时：已收到 %v，期望 %v", got, want)
		}
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("回调 %q 没被触发", name)
		}
	}

	// 状态行与未知 ID 不该有任何反应（上面那几次若被分发，这里就会捞到）。
	select {
	case c := <-calls:
		t.Errorf("状态行或未知命令 ID 触发了回调 %q", c)
	case <-time.After(200 * time.Millisecond):
	}
}

// 打开配置界面失败时必须弹气泡说明原因。
//
// 无控制台的构建里，用户点了「设置」却什么都没发生是最难自查的故障：
// 他既看不到窗口，也想不到要去翻日志文件。气泡是唯一能主动够到他的手段。
func TestOpenUIReportsFailureOnBalloon(t *testing.T) {
	got := make(chan string, 4)
	tr := &winTray{
		cb:     &trayCallbacks{OpenUI: func() error { return errors.New("配置界面没有启动") }},
		notify: func(title, text string) { got <- title + "|" + text },
	}
	tr.openUI()

	select {
	case s := <-got:
		if !strings.Contains(s, "无法打开配置界面") || !strings.Contains(s, "配置界面没有启动") {
			t.Errorf("气泡内容 = %q，期望标题和原因都在里面", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("打开配置界面失败时没有弹气泡")
	}
}

// 打开成功时不该弹气泡：每次双击托盘都弹一个气泡很吵。
func TestOpenUISuccessStaysSilent(t *testing.T) {
	got := make(chan string, 4)
	tr := &winTray{
		cb:     &trayCallbacks{OpenUI: func() error { return nil }},
		notify: func(title, text string) { got <- title },
	}
	tr.openUI()

	select {
	case s := <-got:
		t.Errorf("打开成功不该弹气泡，实际弹了 %q", s)
	case <-time.After(300 * time.Millisecond):
	}
}

// 配置界面起不来时，托盘气泡里要说清是**哪一类**问题。
//
// Windows 给的是 WSA 错误码，不是 POSIX 的 EADDRINUSE（10048 vs 98），
// 这个映射很容易写错；写错就会把「端口冲突」显示成「未运行」，
// 把用户引向完全错误的方向（去翻日志，而不是去改端口）。
func TestUIFailReasonOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"地址已被占用",
			&net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10048)}},
			"端口冲突"},
		{"落在系统保留段",
			&net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10013)}},
			"端口冲突"},
		{"多包一层也认得出来",
			fmt.Errorf("都绑不上：%w", syscall.Errno(10013)),
			"端口冲突"},
		{"别的毛病", errors.New("系统资源不足"), "未运行"},
		{"没有错误", nil, "未运行"},
	}
	for _, c := range cases {
		if got := uiFailReasonOf(c.err); got != c.want {
			t.Errorf("%s：uiFailReasonOf = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 再拿**真实**的绑定失败跑一次：合成的错误链可能和 net 包实际包装的层次不一样。
//
// 50811 是这台机器上被系统保留的端口之一（`netsh int ipv4 show excludedportrange`）。
// 保留段因机器而异，万一它能绑上就跳过——这不是失败，只是没条件复现。
func TestUIFailReasonOnRealBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:50811")
	if err == nil {
		ln.Close()
		t.Skip("本机 50811 没被保留，跳过真实场景校验")
	}
	if got := uiFailReasonOf(err); got != "端口冲突" {
		t.Errorf("真实绑定失败被识别成 %q，期望「端口冲突」（原始错误：%v）", got, err)
	}
}

// 状态词必须短：它是悬停提示的内容，会显示在任务栏那个小气泡里。
//
// 之前是「服务器限流，退避中」这种半句话，出错时更是把整段错误信息
// （常常是一长串 URL 加 HTTP 状态）原样塞进去。
// 这里拿真实状态跑一遍，钉死「每个状态词都不超过 4 个字、且不带标点」。
func TestTrayStatusLabelIsShort(t *testing.T) {
	mk := func(mut func(*SyncEngine)) *SyncEngine {
		e := NewSyncEngine(Config{Enabled: true, DavURL: "https://example.com/dav/"}, nil, NewLogBuffer(10))
		mut(e)
		return e
	}
	cases := []struct {
		name  string
		eng   *SyncEngine
		state string
		want  string
	}{
		{"正常", mk(func(*SyncEngine) {}), "ok", "同步中"},
		{"开关关闭", mk(func(e *SyncEngine) { e.cfg.Enabled = false }), "disabled", "已停用"},
		{"未配置服务器", mk(func(e *SyncEngine) { e.cfg.DavURL = "" }), "stopped", "未配置"},
		{"限流退避", mk(func(e *SyncEngine) { e.backoffUntil = time.Now().Add(time.Minute) }), "backoff", "限流退避"},
		{"出错", mk(func(e *SyncEngine) {
			e.lastError = "Put https://dav.example.com/dav/xime/clipboard/current.json: 503 Service Unavailable"
		}), "error", "同步出错"},
	}
	for _, c := range cases {
		if got := c.eng.Status().State; got != c.state {
			t.Fatalf("%s：构造出来的状态是 %q，期望 %q（用例自己写错了）", c.name, got, c.state)
		}
		label := trayStatusLabel(c.eng)
		if label != c.want {
			t.Errorf("%s：trayStatusLabel = %q，期望 %q", c.name, label, c.want)
		}
		if n := len([]rune(label)); n > 4 {
			t.Errorf("%s：状态词 %q 有 %d 个字，超过 4 个", c.name, label, n)
		}
		if strings.ContainsAny(label, "，。：；,.:;") {
			t.Errorf("%s：状态词 %q 里不该有标点", c.name, label)
		}
	}
}
