//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// 托盘图标直接从内嵌的 img/logo.ico 取一档，用 CreateIconFromResourceEx
// 变成 HICON。ICO 里的图像数据本来就是 DIB（BITMAPINFOHEADER + XOR + AND），
// 正是这个 API 期望的格式，所以不需要重新编码，也不需要落盘临时文件。

var (
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
)

const (
	// CreateIconFromResourceEx 的 dwVer：图标固定 0x00030000
	iconResourceVersion = 0x00030000
	// GetSystemMetrics 的索引：小图标边长
	smCxSmIcon = 49
	smCySmIcon = 50
)

// createAppIcon 创建托盘图标，返回 HICON（用完需 DestroyIcon）以及采用的边长。
//
// 返回的边长用于日志：托盘图标「糊不糊」取决于这一档是不是正好等于系统要的尺寸，
// 而没有控制台的构建里只能靠日志看到它。
func createAppIcon() (uintptr, int, error) {
	entries, err := parseICO(logoICO)
	if err != nil {
		return 0, 0, fmt.Errorf("读取内嵌图标失败：%w", err)
	}

	want := smallIconSize()
	entry, ok := pickICO(entries, want)
	if !ok {
		return 0, 0, fmt.Errorf("内嵌图标里没有可用尺寸")
	}
	// 交给 Windows 的尺寸就用 want：DPI 感知生效时它就是任务栏真正要的边长
	// （100% 缩放 16、150% 缩放 24……），此时 pickICO 会挑到同一档，1:1 绘制。
	cx, cy := want, want

	h, _, callErr := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&entry.Data[0])),
		uintptr(len(entry.Data)),
		1, // fIcon
		iconResourceVersion,
		uintptr(cx), uintptr(cy),
		0, // LR_DEFAULTCOLOR
	)
	if h == 0 {
		return 0, 0, fmt.Errorf("CreateIconFromResourceEx 失败：%w", callErr)
	}
	return h, entry.Width, nil
}

// smallIconSize 返回系统当前要求的小图标边长。
//
// 注意这个值**依赖进程的 DPI 感知**：DPI 不感知的进程在任何缩放下都只会拿到 16，
// 而任务栏在 150% 缩放下实际需要 24——拿 16 去交差就会被外壳放大成糊图。
// 所以 main 里必须先调 enableDPIAwareness。
// 取不到就退回 16。
func smallIconSize() int {
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	n := int(cx)
	if cy > cx {
		n = int(cy)
	}
	if n <= 0 {
		return 16
	}
	return n
}

func destroyAppIcon(h uintptr) {
	if h != 0 {
		procDestroyIcon.Call(h)
	}
}

// utf16Ptr 是 syscall.UTF16PtrFromString 的简写。
func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}
