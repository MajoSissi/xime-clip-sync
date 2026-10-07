package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
)

// 应用图标来自 img/logo.ico（8 档尺寸，全部是 32bpp DIB）。
// 它同时供两处使用：
//
//	构建期：genico.go 把每档图像原样塞进 .syso 的 RT_ICON，成为 exe 的文件图标
//	运行期：icon_windows.go 取最合适的一档，用 CreateIconFromResourceEx 变成托盘 HICON
//
// 所以这个文件里只有「读 ICO」，没有任何绘制逻辑。
//
//go:embed img/logo.ico
var logoICO []byte

// icoEntry 是 .ico 里的一档图像。
//
// Data 是原始图像数据：BITMAPINFOHEADER + XOR 位图 + AND 掩码。
// 这正好就是 RT_ICON 需要的格式，也正好是 CreateIconFromResourceEx 接受的格式，
// 因此从 ICO 到资源 / 到 HICON 都是原样搬运，不需要重新编码。
type icoEntry struct {
	Width  int // 像素宽（ICO 目录里 256 用 0 表示）
	Height int
	Planes uint16
	Bits   uint16 // 每像素位数
	Data   []byte
}

// parseICO 解析 .ico 的目录区并切出各档图像数据。
//
// 只做边界与头部校验，不校验像素内容——ICO 里的数据本来就是 DIB。
func parseICO(b []byte) ([]icoEntry, error) {
	if len(b) < 6 {
		return nil, fmt.Errorf("ICO 只有 %d 字节，连文件头都不够", len(b))
	}
	if r := binary.LittleEndian.Uint16(b[0:]); r != 0 {
		return nil, fmt.Errorf("ICO reserved=%d，期望 0", r)
	}
	if t := binary.LittleEndian.Uint16(b[2:]); t != 1 {
		return nil, fmt.Errorf("ICO type=%d，期望 1（图标）", t)
	}
	n := int(binary.LittleEndian.Uint16(b[4:]))
	if n <= 0 {
		return nil, fmt.Errorf("ICO 里没有任何图像")
	}
	if len(b) < 6+16*n {
		return nil, fmt.Errorf("ICO 目录区被截断：%d 张图需要 %d 字节，实际 %d",
			n, 6+16*n, len(b))
	}

	out := make([]icoEntry, 0, n)
	for i := 0; i < n; i++ {
		e := b[6+i*16:]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:]))
		off := int(binary.LittleEndian.Uint32(e[12:]))
		if size <= 0 || off < 6+16*n || off+size > len(b) {
			return nil, fmt.Errorf("ICO 第 %d 档（%d×%d）数据越界：offset=%d size=%d 文件长度=%d",
				i, w, h, off, size, len(b))
		}
		out = append(out, icoEntry{
			Width:  w,
			Height: h,
			Planes: binary.LittleEndian.Uint16(e[4:]),
			Bits:   binary.LittleEndian.Uint16(e[6:]),
			Data:   b[off : off+size],
		})
	}
	return out, nil
}

// pickICO 选出最适合 want 边长的一档。
//
// 优先级：正好相等 > 比 want 大的里面最小的 > 最大的那一档。
// 之所以宁可选大一点再缩，也不选小一点再放：放大是「凭空插值」，边缘会发虚；
// 缩小是「丢像素」，观感明显更好。所以哪怕只有 16 和 256 两档，
// 系统要 20 时也应该拿 256 去缩，而不是把 16 拉大。
func pickICO(entries []icoEntry, want int) (icoEntry, bool) {
	if len(entries) == 0 {
		return icoEntry{}, false
	}

	best := entries[0]   // 兜底：整体最大的那一档（全都比 want 小时用它）
	larger := icoEntry{} // 比 want 大的里面最小的
	hasLarger := false

	for _, e := range entries {
		if e.Width == want {
			return e, true // 正好命中，没有更好的了
		}
		if e.Width > best.Width {
			best = e
		}
		if e.Width > want && (!hasLarger || e.Width < larger.Width) {
			larger, hasLarger = e, true
		}
	}
	if hasLarger {
		return larger, true
	}
	return best, true
}
