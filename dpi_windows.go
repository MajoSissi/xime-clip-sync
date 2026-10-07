//go:build windows

package main

import "syscall"

// 进程 DPI 感知。
//
// 不声明的话，进程默认是「DPI 不感知」的：系统会把整个界面按缩放比例做位图拉伸。
// 对托盘图标的后果非常具体——在 150% 缩放的屏幕上，任务栏需要 24×24 的图标，
// 但 DPI 不感知的进程调 GetSystemMetrics(SM_CXSMICON) 只能拿到虚拟化后的 16，
// 于是我们交出 16×16 的 HICON，由外壳放大到 24×24 绘制，看上去就是「糊」。
//
// 在创建任何窗口之前声明 DPI 感知，GetSystemMetrics 才会返回真实尺寸，
// 我们就能从 ICO 里挑到对应的那一档，做到 1:1 绘制。
var (
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")

	shcore                     = syscall.NewLazyDLL("shcore.dll")
	procSetProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")
)

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 的值为 -4，
	// 以 uintptr 传递时要写成补码形式。
	dpiAwarenessPerMonitorV2 = ^uintptr(3)
	// PROCESS_PER_MONITOR_DPI_AWARE
	processPerMonitorDPIAware = 2
)

// enableDPIAwareness 尽力把当前进程声明为 DPI 感知，返回是否成功。
//
// 必须**在创建任何窗口之前**调用，否则设置不生效。
// 系统版本越老，可用的 API 越少，所以逐级回退；全部失败也不算致命
// （最坏就是托盘图标被系统拉伸，程序照常工作）。
func enableDPIAwareness() bool {
	// Windows 10 1703+
	if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); r != 0 {
		return true
	}
	// Windows 8.1+：返回 HRESULT，S_OK(0) 表示成功
	if r, _, _ := procSetProcessDpiAwareness.Call(processPerMonitorDPIAware); r == 0 {
		return true
	}
	// Vista+
	if r, _, _ := procSetProcessDPIAware.Call(); r != 0 {
		return true
	}
	return false
}
