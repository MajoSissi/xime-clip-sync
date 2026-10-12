//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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
	procRegisterClipboardFormat    = user32.NewProc("RegisterClipboardFormatW")

	procGlobalAlloc   = kernel32.NewProc("GlobalAlloc")
	procGlobalFree    = kernel32.NewProc("GlobalFree")
	procGlobalLock    = kernel32.NewProc("GlobalLock")
	procGlobalUnlock  = kernel32.NewProc("GlobalUnlock")
	procGlobalSize    = kernel32.NewProc("GlobalSize")
	procRtlMoveMemory = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	// 位图相关的标准剪贴板格式。CF_DIB 是裸的 DIB（BITMAPINFOHEADER + 像素），
	// CF_DIBV5 用 BITMAPV5HEADER（带 alpha 掩码）。CF_BITMAP 是 HBITMAP 句柄，
	// 拿不到字节，这里不读它（CF_DIB 几乎总是一起存在）。
	cfDIB   = 8
	cfDIBV5 = 17

	gmemMoveable = 0x0002
	// 剪贴板被其他程序占用时重试的次数与间隔
	clipboardRetries = 20
	clipboardRetry   = 10 * time.Millisecond
)

// pngClipboardFormat 返回注册格式 "PNG" 的格式号；注册失败返回 0。
//
// 浏览器与不少现代程序复制图片时会放一份**原始 PNG 字节**，直接取它能做到
// 字节级保真（含 alpha），比从 DIB 反推像素更准。注册一次就够，结果缓存下来。
var pngClipboardFormat = sync.OnceValue(func() uint32 {
	name, err := syscall.UTF16PtrFromString("PNG")
	if err != nil {
		return 0
	}
	r, _, _ := procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(name)))
	return uint32(r)
})

type windowsClipboard struct{}

func newClipboard() Clipboard { return windowsClipboard{} }

// copyMemory 在非托管内存之间复制字节。
func copyMemory(dst, src, size uintptr) {
	procRtlMoveMemory.Call(dst, src, size)
}

// allocGlobalBytes 把字节拷进一块 GMEM_MOVEABLE 全局内存，返回句柄（0 = 失败）。
//
// 成功时内存所有权仍在**调用方**：只有 SetClipboardData 成功之后才移交系统，
// 失败时必须自己 GlobalFree，否则每次失败都漏一块内存。
func allocGlobalBytes(data []byte) uintptr {
	size := uintptr(len(data))
	if size == 0 {
		size = 1 // GlobalAlloc(0) 的行为不确定，给个最小块
	}
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return 0
	}
	if len(data) == 0 {
		return h
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return 0
	}
	copyMemory(ptr, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	procGlobalUnlock.Call(h)
	return h
}

// clipboardBytes 取出指定剪贴板格式的原始字节。
// **必须在剪贴板已打开时调用**（GetClipboardData 的前提）；格式不存在或为空返回 nil。
func clipboardBytes(format uint32) []byte {
	if r, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format)); r == 0 {
		return nil
	}
	h, _, _ := procGetClipboardData.Call(uintptr(format))
	if h == 0 {
		return nil
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 {
		return nil
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil
	}
	defer procGlobalUnlock.Call(h)

	raw := make([]byte, size)
	copyMemory(uintptr(unsafe.Pointer(&raw[0])), ptr, size)
	return raw
}

// 剪贴板占用观测：让「没有占着剪贴板」这条不变式**可被测试**。
//
// Windows 的 OpenClipboard 对**同进程是可重入的**——自己占着剪贴板时再开一次
// 照样成功（实测如此）。所以从进程内部问不出「我是不是还占着剪贴板」，
// 而这条恰恰是本程序最容易犯、后果又最严重的一类错：占着不放，
// 别的程序（截图工具、Ditto 这类剪贴板历史）就打不开剪贴板，
// 用户看到的是「开着它截图就复制不进去了」。计数器把这件事变得可断言。
//
// 用原子量而不是互斥锁：openClipboard 里任何可能让 goroutine 让出线程的操作
// 都会把「锁住 OS 线程」的努力白费掉（见下面的说明），而锁竞争正是这种操作。
var (
	clipboardDepth     atomic.Int32
	clipboardOpenCount atomic.Uint64
)

// clipboardStats 返回（当前占用层数，累计打开次数）。仅供测试观测。
func clipboardStats() (int, uint64) {
	return int(clipboardDepth.Load()), clipboardOpenCount.Load()
}

// openClipboard 打开剪贴板并重试（其他程序可能短暂占用）。
//
// **必须先把 goroutine 钉在 OS 线程上**：剪贴板的「谁打开」是按**线程**算的
// （CloseClipboard 只对打开它的那个线程有效），而从打开到关闭之间 goroutine
// 随时可能被调度到另一个线程上——那时 EmptyClipboard / SetClipboardData 会失败，
// 报 "Thread does not have a clipboard open"。锁住线程能保证
// open → 用 → close 全程落在同一个线程上。
func openClipboard() (bool, error) {
	runtime.LockOSThread()

	var last error
	for i := 0; i < clipboardRetries; i++ {
		r, _, err := procOpenClipboard.Call(0)
		if r != 0 {
			clipboardDepth.Add(1)
			clipboardOpenCount.Add(1)
			return true, nil
		}
		last = err
		time.Sleep(clipboardRetry)
	}
	runtime.UnlockOSThread()
	if last == nil {
		last = errors.New("剪贴板被占用")
	}
	return false, fmt.Errorf("无法打开剪贴板：%w", last)
}

// closeClipboard 释放剪贴板并解除线程绑定。
// 与 openClipboard 严格配对，一律用 defer 调用——漏一次就是把剪贴板永久锁死。
func closeClipboard() {
	if clipboardDepth.Add(-1) < 0 {
		// 只可能出现在实现写错时；把计数拨回 0，避免后面所有断言都被带偏。
		clipboardDepth.Store(0)
	}
	procCloseClipboard.Call()
	runtime.UnlockOSThread()
}

// GetText 读取 CF_UNICODETEXT 内容。
//
// 先做一次**无锁**的可用性查询：剪贴板里是图片时（没有 CF_UNICODETEXT）
// 直接返回空串，连剪贴板都不开。复制图片时每次都会走到这条路径，
// 少锁一次就少一次和截图工具 / Ditto 抢剪贴板的机会。
func (windowsClipboard) GetText() (string, error) {
	if !clipboardHas(cfUnicodeText) {
		return "", nil // 剪贴板中没有文本
	}
	ok, err := openClipboard()
	if !ok {
		return "", err
	}
	defer closeClipboard()

	raw := clipboardBytes(cfUnicodeText)
	if len(raw) == 0 {
		return "", nil // 在查询和取字节之间被别的程序清掉了
	}

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

	raw := make([]byte, len(units)*2)
	for i, u := range units {
		raw[i*2] = byte(u)
		raw[i*2+1] = byte(u >> 8)
	}
	h := allocGlobalBytes(raw)
	if h == 0 {
		return errors.New("GlobalAlloc 失败")
	}

	ok, err := openClipboard()
	if !ok {
		procGlobalFree.Call(h)
		return err
	}
	defer closeClipboard()

	procEmptyClipboard.Call()
	if r, _, serr := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		// 失败时内存仍归本进程所有，需要释放
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData 失败：%w", serr)
	}
	// 成功后内存所有权移交系统，不可再释放
	return nil
}

// Seq 返回剪贴板变更序号（便宜，无需锁定剪贴板）。
func (windowsClipboard) Seq() (uint32, bool) {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r), true
}

// ---------------------------------------------------------------- 图片
//
// 【不变式】剪贴板只在**取原始字节**的那一小会儿被占着，解码与编码一律在锁外做。
//
// 这条不是洁癖，是踩过的坑：4K 截图的 CF_DIB 有 30 多 MB，dibToImage 要再分配一份
// 30 MB 的位图，encodePNG 还要归一化一次并 deflate——整条链路几百毫秒。
// 这段时间里如果一直占着剪贴板：
//   - 截图工具（Snipping Tool 等）写剪贴板时会卡在 OpenClipboard 重试上，表现为
//     「确认截图后卡一下」，重试耗尽就直接放弃，图根本没进剪贴板；
//   - Ditto 这类剪贴板历史收到 WM_CLIPBOARDUPDATE 后要打开剪贴板才能取数据，
//     打不开就跳过这一条，表现为「历史里没有刚截的那张图」。
//
// 所以：clipboardHas 先无锁挑格式 → clipboardRawOf 锁一次只做内存拷贝 →
// clipboardImageToPNG 在锁外慢慢算。

// clipboardHas 报告剪贴板里有没有某个格式。
//
// IsClipboardFormatAvailable **不需要打开剪贴板**，所以可以随便问——
// 用它先把候选格式挑出来，只有真要取字节时才去锁剪贴板。
// format 传 0（注册失败）时返回 false。
func clipboardHas(format uint32) bool {
	if format == 0 {
		return false
	}
	r, _, _ := procIsClipboardFormatAvailable.Call(uintptr(format))
	return r != 0
}

// clipboardRawOf 取一个剪贴板格式的原始字节，**拿到就立刻释放剪贴板**。
//
// 锁内只做一次内存拷贝（clipboardBytes 里的 make + RtlMoveMemory）。
// 调用方必须在返回之后再做解码/编码——见上面「不变式」。
func clipboardRawOf(format uint32) ([]byte, error) {
	ok, err := openClipboard()
	if !ok {
		return nil, err
	}
	defer closeClipboard()
	return clipboardBytes(format), nil
}

// clipboardImageToPNG 把从剪贴板取到的原始字节转成 PNG；转不出来返回 nil。
//
// **必须在剪贴板锁之外调用**：这是整个取图流程里唯一耗时的部分。
// 声明成变量而不是普通函数，是为了让测试能替换成探针，钉住这条约定
// （见 TestGetImageReleasesClipboardBeforeConverting）。
//
// fromPNG 为 true 表示 raw 来自 "PNG" 注册格式，此时只做校验、原字节返回，
// 不重编码——那样才能字节级保真（含 alpha）。
var clipboardImageToPNG = func(raw []byte, fromPNG bool) []byte {
	if fromPNG {
		if _, err := decodePNG(raw); err == nil {
			return raw
		}
		return nil
	}
	img, err := dibToImage(raw)
	if err != nil {
		return nil
	}
	png, err := encodePNG(img)
	if err != nil {
		return nil
	}
	return png
}

// HasImage 报告剪贴板里是否有位图。
//
// 只查格式是否存在，不读数据、不解码、**不锁剪贴板**——每个本地检查周期都会先问一次。
// 优先看 PNG 格式：浏览器和现代程序复制图片时会放一份原始 PNG 字节。
func (windowsClipboard) HasImage() bool {
	if clipboardHas(pngClipboardFormat()) {
		return true
	}
	return clipboardHas(cfDIBV5) || clipboardHas(cfDIB)
}

// GetImage 读取剪贴板位图并编码为 PNG；没有可读的位图时返回 (nil, nil)。
//
// 取图顺序（保真度优先）：
//  1. "PNG" 注册格式 —— 拿到的就是原字节，含 alpha，不做任何重编码；
//  2. CF_DIBV5 —— 头部含 alpha 掩码，透明信息完整；
//  3. CF_DIB   —— 老格式，32 位时 alpha 未定义（按不透明解释）。
//
// 一路都取不到就返回 nil（比如剪贴板里只有 CF_BITMAP 句柄），
// 由上层按「没有图片」处理，而不是报错刷屏。
//
// 纯文本剪贴板在这里**一次都不会锁剪贴板**：三种格式的可用性查询都是无锁的。
func (windowsClipboard) GetImage() ([]byte, error) {
	if f := pngClipboardFormat(); clipboardHas(f) {
		raw, err := clipboardRawOf(f)
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			// 只认能解码的：格式存在但内容不是 PNG 时安静地往下走 DIB
			if png := clipboardImageToPNG(raw, true); png != nil {
				return png, nil
			}
		}
	}
	for _, f := range []uint32{cfDIBV5, cfDIB} {
		if !clipboardHas(f) {
			continue
		}
		raw, err := clipboardRawOf(f)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		if png := clipboardImageToPNG(raw, false); png != nil {
			return png, nil
		}
	}
	return nil, nil
}

// SetImage 把 PNG 字节写入剪贴板。
//
// 同时放两种格式：
//   - CF_DIB（32 位 BI_RGB）：画图 / Word / 老程序认这个，兼容面最广；
//   - "PNG" 注册格式：浏览器等现代程序直接拿原字节，透明信息不丢。
//
// 先解码再动手：传进来的不是合法 PNG 就直接报错返回，
// **不会**清空剪贴板留下一片空白（否则一次坏数据就把用户的剪贴板清掉了）。
func (windowsClipboard) SetImage(pngData []byte) error {
	img, err := decodePNG(pngData)
	if err != nil {
		return err
	}
	dib := imageToDIB32(img)
	if len(dib) == 0 {
		return errors.New("图片尺寸非法，无法写入剪贴板")
	}

	hDib := allocGlobalBytes(dib)
	if hDib == 0 {
		return errors.New("GlobalAlloc 失败（DIB）")
	}
	pngFmt := pngClipboardFormat()
	var hPng uintptr
	if pngFmt != 0 {
		hPng = allocGlobalBytes(pngData)
		if hPng == 0 {
			procGlobalFree.Call(hDib)
			return errors.New("GlobalAlloc 失败（PNG）")
		}
	}

	ok, oerr := openClipboard()
	if !ok {
		procGlobalFree.Call(hDib)
		if hPng != 0 {
			procGlobalFree.Call(hPng)
		}
		return oerr
	}
	defer closeClipboard()

	procEmptyClipboard.Call()
	// EmptyClipboard 之后第一个成功的 SetClipboardData 决定所有者；
	// 一旦成功，那块内存就归系统，本进程不能再释放。
	if r, _, serr := procSetClipboardData.Call(cfDIB, hDib); r == 0 {
		procGlobalFree.Call(hDib)
		if hPng != 0 {
			procGlobalFree.Call(hPng)
		}
		return fmt.Errorf("SetClipboardData(CF_DIB) 失败：%w", serr)
	}
	if hPng != 0 {
		if r, _, _ := procSetClipboardData.Call(uintptr(pngFmt), hPng); r == 0 {
			// DIB 已经交出去了，只回收 PNG 这块；剪贴板里仍有可用的位图
			procGlobalFree.Call(hPng)
		}
	}
	return nil
}
