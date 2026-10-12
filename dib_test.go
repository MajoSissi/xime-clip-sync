package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// ---------------------------------------------------------------- 造位图

// makeDIB24 造一张 w×h 的 24 位 BI_RGB 位图（自下而上），颜色由 px(x, y) 给出，
// 坐标 y 是**图像坐标**（0 在顶部）。
func makeDIB24(w, h int, px func(x, y int) (uint8, uint8, uint8)) []byte {
	stride := ((w*24 + 31) / 32) * 4
	out := make([]byte, 40+stride*h)
	le := binary.LittleEndian
	le.PutUint32(out[0:4], dibHeaderInfo)
	le.PutUint32(out[4:8], uint32(int32(w)))
	le.PutUint32(out[8:12], uint32(int32(h)))
	le.PutUint16(out[12:14], 1)
	le.PutUint16(out[14:16], 24)
	for y := 0; y < h; y++ {
		row := out[40+y*stride : 40+y*stride+stride]
		srcY := h - 1 - y // DIB 自下而上
		for x := 0; x < w; x++ {
			r, g, b := px(x, srcY)
			row[x*3], row[x*3+1], row[x*3+2] = b, g, r
		}
	}
	return out
}

// makeDIB32 造一张 w×h 的 32 位位图。compression 传 biRGB 或 biBitFields；
// bitFields 时用 56 字节的 V3 头（含 alpha 掩码）。
func makeDIB32(w, h int, compression uint32, px func(x, y int) (uint8, uint8, uint8, uint8)) []byte {
	hdr := dibHeaderInfo
	if compression == biBitFields {
		hdr = dibHeaderV3
	}
	stride := w * 4
	out := make([]byte, hdr+stride*h)
	le := binary.LittleEndian
	le.PutUint32(out[0:4], uint32(hdr))
	le.PutUint32(out[4:8], uint32(int32(w)))
	le.PutUint32(out[8:12], uint32(int32(h)))
	le.PutUint16(out[12:14], 1)
	le.PutUint16(out[14:16], 32)
	le.PutUint32(out[16:20], compression)
	if compression == biBitFields {
		le.PutUint32(out[40:44], 0x00FF0000) // R
		le.PutUint32(out[44:48], 0x0000FF00) // G
		le.PutUint32(out[48:52], 0x000000FF) // B
		le.PutUint32(out[52:56], 0xFF000000) // A
	}
	for y := 0; y < h; y++ {
		row := out[hdr+y*stride : hdr+y*stride+stride]
		srcY := h - 1 - y
		for x := 0; x < w; x++ {
			r, g, b, a := px(x, srcY)
			row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = b, g, r, a
		}
	}
	return out
}

// nrgbaAt 取像素的 NRGBA 值（测试断言用）。
func nrgbaAt(t *testing.T, img image.Image, x, y int) color.NRGBA {
	t.Helper()
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func wantPixel(t *testing.T, img image.Image, x, y int, want color.NRGBA) {
	t.Helper()
	got := nrgbaAt(t, img, x, y)
	if got != want {
		t.Errorf("像素(%d,%d) = %+v，期望 %+v", x, y, got, want)
	}
}

// ---------------------------------------------------------------- 解码

// 24 位 BI_RGB：最基本的截图格式，BGR 顺序、自下而上、每行 4 字节对齐。
func TestDibToImageRGB24(t *testing.T) {
	// 3 像素宽 → 每行 9 字节，要补齐到 12 字节（4 字节对齐）
	dib := makeDIB24(3, 2, func(x, y int) (uint8, uint8, uint8) {
		return uint8(10 * (x + 1)), uint8(20 * (y + 1)), 200
	})
	img, err := dibToImage(dib)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if b := img.Bounds(); b.Dx() != 3 || b.Dy() != 2 {
		t.Fatalf("尺寸不符：%v", b)
	}
	// y=0 是图像顶部 → 对应 px(x, 0) → g=20
	wantPixel(t, img, 0, 0, color.NRGBA{R: 10, G: 20, B: 200, A: 255})
	wantPixel(t, img, 2, 0, color.NRGBA{R: 30, G: 20, B: 200, A: 255})
	wantPixel(t, img, 0, 1, color.NRGBA{R: 10, G: 40, B: 200, A: 255})
	wantPixel(t, img, 2, 1, color.NRGBA{R: 30, G: 40, B: 200, A: 255})
}

// 32 位 BI_RGB 的 alpha 字节**未定义**，必须当不透明。
//
// 这条是防「整张图变透明」的关键：截图工具与不少程序会把那 4 个字节填 0，
// 真按 alpha 解释的话用户拿到的就是一张全透明的空图。
func TestDibToImageRGB32AlphaIgnored(t *testing.T) {
	dib := makeDIB32(2, 1, biRGB, func(x, y int) (uint8, uint8, uint8, uint8) {
		return 12, 34, 56, 0 // alpha 故意填 0
	})
	img, err := dibToImage(dib)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	wantPixel(t, img, 0, 0, color.NRGBA{R: 12, G: 34, B: 56, A: 255})
	wantPixel(t, img, 1, 0, color.NRGBA{R: 12, G: 34, B: 56, A: 255})
}

// 32 位 BI_BITFIELDS 带 alpha 掩码：这时 alpha 是**真**的，必须保留。
func TestDibToImageBitfieldsKeepsAlpha(t *testing.T) {
	dib := makeDIB32(2, 1, biBitFields, func(x, y int) (uint8, uint8, uint8, uint8) {
		return 100, 150, 200, 128
	})
	img, err := dibToImage(dib)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	wantPixel(t, img, 0, 0, color.NRGBA{R: 100, G: 150, B: 200, A: 128})
	wantPixel(t, img, 1, 0, color.NRGBA{R: 100, G: 150, B: 200, A: 128})
}

// 高度为负 = 自上而下。行序不能搞反，否则整张图上下颠倒。
func TestDibToImageTopDown(t *testing.T) {
	stride := 2 * 4
	dib := make([]byte, 40+stride*2)
	le := binary.LittleEndian
	negHeight := int32(-2) // 负高度 = 自上而下
	le.PutUint32(dib[0:4], dibHeaderInfo)
	le.PutUint32(dib[4:8], 2)
	le.PutUint32(dib[8:12], uint32(negHeight))
	le.PutUint16(dib[12:14], 1)
	le.PutUint16(dib[14:16], 32)
	// 第一行（图像顶部）纯红，第二行纯蓝
	put := func(row int, b, g, r uint8) {
		p := dib[40+row*stride:]
		p[0], p[1], p[2], p[3] = b, g, r, 255
		p[4], p[5], p[6], p[7] = b, g, r, 255
	}
	put(0, 0, 0, 255)
	put(1, 255, 0, 0)

	img, err := dibToImage(dib)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	wantPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255}) // 顶行红
	wantPixel(t, img, 0, 1, color.NRGBA{B: 255, A: 255}) // 底行蓝
}

// 8 位带调色板：调色板项是 BGRA，下标越界不能 panic。
func TestDibToImagePalette8(t *testing.T) {
	const n = 256
	dib := make([]byte, 40+4*n+4)
	le := binary.LittleEndian
	le.PutUint32(dib[0:4], dibHeaderInfo)
	le.PutUint32(dib[4:8], 4) // 4 像素宽 → 每行 4 字节，正好对齐
	le.PutUint32(dib[8:12], 1)
	le.PutUint16(dib[12:14], 1)
	le.PutUint16(dib[14:16], 8)
	le.PutUint32(dib[32:36], n)
	// 调色板 0 = 红，1 = 绿
	dib[40], dib[41], dib[42], dib[43] = 0, 0, 255, 0
	dib[44], dib[45], dib[46], dib[47] = 0, 255, 0, 0
	pix := dib[40+4*n:]
	pix[0], pix[1], pix[2], pix[3] = 0, 1, 0, 1

	img, err := dibToImage(dib)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	wantPixel(t, img, 0, 0, color.NRGBA{R: 255, A: 255})
	wantPixel(t, img, 1, 0, color.NRGBA{G: 255, A: 255})
}

// 不认识的压缩方式 / 截断的数据都必须报错，而不是解出一张错图。
func TestDibToImageRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		dib  []byte
	}{
		{"空数据", nil},
		{"只有 3 字节", []byte{1, 2, 3}},
		{"头大小不合法", []byte{7, 0, 0, 0, 0, 0, 0, 0}},
		{"RLE 压缩", func() []byte {
			d := makeDIB24(2, 2, func(x, y int) (uint8, uint8, uint8) { return 0, 0, 0 })
			binary.LittleEndian.PutUint32(d[16:20], biRLE8)
			return d
		}()},
		{"像素被截断", makeDIB24(8, 8, func(x, y int) (uint8, uint8, uint8) { return 0, 0, 0 })[:60]},
		{"尺寸为 0", func() []byte {
			d := makeDIB24(2, 2, func(x, y int) (uint8, uint8, uint8) { return 0, 0, 0 })
			binary.LittleEndian.PutUint32(d[4:8], 0)
			return d
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := dibToImage(c.dib); err == nil {
				t.Error("应当报错，却解出了图片")
			}
		})
	}
}

// ---------------------------------------------------------------- 编码

// imageToDIB32 → dibToImage 往返必须像素级一致，且上下方向不反。
//
// 用不透明的像素：imageToDIB32 写的是 32 位 **BI_RGB**，那种格式的 alpha
// 未定义，读回来按不透明解释（见 TestDibToImageRGB32AlphaIgnored）——
// 透明信息靠剪贴板上另外那份 PNG 格式保真，不走 DIB 这条线。
func TestDibRoundTrip(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 40), G: uint8(y * 80), B: uint8(x * y * 10), A: 255,
			})
		}
	}
	back, err := dibToImage(imageToDIB32(src))
	if err != nil {
		t.Fatalf("往返解码失败：%v", err)
	}
	if b := back.Bounds(); b.Dx() != 5 || b.Dy() != 3 {
		t.Fatalf("往返尺寸不符：%v", b)
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			if got, want := nrgbaAt(t, back, x, y), src.NRGBAAt(x, y); got != want {
				t.Errorf("往返像素(%d,%d) = %+v，期望 %+v", x, y, got, want)
			}
		}
	}
}

// 同一份像素编码两次必须逐字节相同。
//
// 远端附件名是内容寻址（<sha256>.png），而且回声抑制直接比哈希——
// 只要编码有一点不确定（比如随图像底层类型变），同内容就会被反复当成新内容推来推去。
func TestEncodePNGIsDeterministic(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255 // 全不透明：预乘与直通表示一致，便于下面比较两种底层类型
	}
	a, err := encodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("同一张图两次编码结果不同，回声抑制会失效")
	}
	if _, err := decodePNG(a); err != nil {
		t.Errorf("编码结果不是合法 PNG：%v", err)
	}
	// 换一种底层图像类型（RGBA）编同一份像素，字节也应当一致——
	// encodePNG 先规范化到 NRGBA 就是为了这个。
	alt := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			alt.Set(x, y, img.NRGBAAt(x, y))
		}
	}
	c, err := encodePNG(alt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Error("不同底层类型编出的 PNG 不一致，规范化没生效")
	}
}

// 解码失败要报错，不能返回半张图。
func TestDecodePNGRejectsGarbage(t *testing.T) {
	if _, err := decodePNG([]byte("not a png")); err == nil {
		t.Error("坏数据应当报错")
	}
}
