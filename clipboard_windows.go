//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Windows 剪贴板实现：直接调用 user32/kernel32，不依赖 cgo 或第三方库，
// 因此可以静态编译、单文件分发。
//
// 内存拷贝统一走 kernel32!RtlMoveMemory，而不是把 uintptr 转成 unsafe.Pointer，
// 这样既避开 Go 指针规则的风险，也让 go vet 保持干净。

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc   = kernel32.NewProc("GlobalAlloc")
	procGlobalFree    = kernel32.NewProc("GlobalFree")
	procGlobalLock    = kernel32.NewProc("GlobalLock")
	procGlobalUnlock  = kernel32.NewProc("GlobalUnlock")
	procGlobalSize    = kernel32.NewProc("GlobalSize")
	procRtlMoveMemory = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
	// 剪贴板被其他程序占用时重试的次数与间隔
	clipboardRetries = 20
	clipboardRetry   = 10 * time.Millisecond
)

type windowsClipboard struct{}

func newClipboard() Clipboard { return windowsClipboard{} }

// copyMemory 在非托管内存之间复制字节。
func copyMemory(dst, src, size uintptr) {
	procRtlMoveMemory.Call(dst, src, size)
}

// openClipboard 打开剪贴板并重试（其他程序可能短暂占用）。
func openClipboard() (bool, error) {
	var last error
	for i := 0; i < clipboardRetries; i++ {
		r, _, err := procOpenClipboard.Call(0)
		if r != 0 {
			return true, nil
		}
		last = err
		time.Sleep(clipboardRetry)
	}
	if last == nil {
		last = errors.New("剪贴板被占用")
	}
	return false, fmt.Errorf("无法打开剪贴板：%w", last)
}

// GetText 读取 CF_UNICODETEXT 内容。
func (windowsClipboard) GetText() (string, error) {
	ok, err := openClipboard()
	if !ok {
		return "", err
	}
	defer procCloseClipboard.Call()

	if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", nil // 剪贴板中没有文本
	}
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 {
		return "", nil
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", errors.New("GlobalLock 失败")
	}
	defer procGlobalUnlock.Call(h)

	raw := make([]byte, size)
	if len(raw) == 0 {
		return "", nil
	}
	copyMemory(uintptr(unsafe.Pointer(&raw[0])), ptr, size)

	// CF_UNICODETEXT 是 UTF-16LE，以 NUL 结尾
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u := uint16(raw[i]) | uint16(raw[i+1])<<8
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units)), nil
}

// SetText 写入 CF_UNICODETEXT 内容。
func (windowsClipboard) SetText(text string) error {
	encoded := utf16.Encode([]rune(text))
	units := make([]uint16, len(encoded)+1) // 末尾保留 NUL
	copy(units, encoded)
	size := uintptr(len(units) * 2)

	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc 失败：%w", err)
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return errors.New("GlobalLock 失败")
	}

	raw := make([]byte, size)
	for i, u := range units {
		raw[i*2] = byte(u)
		raw[i*2+1] = byte(u >> 8)
	}
	copyMemory(ptr, uintptr(unsafe.Pointer(&raw[0])), size)
	procGlobalUnlock.Call(h)

	ok, err := openClipboard()
	if !ok {
		procGlobalFree.Call(h)
		return err
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		// 失败时内存仍归本进程所有，需要释放
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData 失败：%w", err)
	}
	// 成功后内存所有权移交系统，不可再释放
	return nil
}

// Seq 返回剪贴板变更序号（便宜，无需锁定剪贴板）。
func (windowsClipboard) Seq() (uint32, bool) {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r), true
}
