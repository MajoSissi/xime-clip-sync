package main

// Windows 剪贴板位图（DIB）与 PNG 之间的编解码。
//
// 剪贴板里的位图是一段**设备无关位图**（DIB）：
//   - CF_DIB    = BITMAPINFOHEADER（40 字节）+ 调色板 + 像素
//   - CF_DIBV5  = BITMAPV5HEADER（124 字节，含 alpha 掩码）+ 像素
// 两种都不带 14 字节的 BMP 文件头（那是 BMP **文件**才有的）。
//
// 远端协议存的是「图片原图文件」，所以这里把 DIB 解成 image.Image，再统一编码成
// PNG（无损、跨端通用；插件端的 EXT_CONTENT_TYPE 也认 image/png）。
//
// 放在**没有 build tag** 的文件里：全是纯计算，不碰任何系统 API，
// 因此能在 CI 上直接单测（CI 是无交互会话，读不到真实剪贴板）。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// DIB 头大小（biSize 字段）。
const (
	dibHeaderCore = 12 // BITMAPCOREHEADER（OS/2 1.x）
	dibHeaderInfo = 40 // BITMAPINFOHEADER
	dibHeaderV2   = 52 // BITMAPV2INFOHEADER（头部内含 R/G/B 掩码）
	dibHeaderV3   = 56 // BITMAPV3INFOHEADER（再多一个 A 掩码）
)

// 压缩方式（biCompression）。
const (
	biRGB       = 0 // 未压缩
	biRLE8      = 1
	biRLE4      = 2
	biBitFields = 3 // 位域（16/32 位常用，可带 alpha）
	biJPEG      = 4
	biPNG       = 5
)

// dibToImage 把一段 DIB（CF_DIB / CF_DIBV5 的内容）解码成 image.Image。
//
// 只支持 BI_RGB 与 BI_BITFIELDS 两类。剪贴板里的位图几乎都是这两类；
// RLE / JPEG / PNG 压缩的 DIB 直接报错，由上层跳过并记日志——宁可不显示，
// 也不要把一张解错的图推给对端。
func dibToImage(dib []byte) (image.Image, error) {
	if len(dib) < 4 {
		return nil, errors.New("DIB 数据太短")
	}
	hdrSize := int(binary.LittleEndian.Uint32(dib[0:4]))
	if hdrSize == dibHeaderCore {
		return dibCoreToImage(dib)
	}
	if hdrSize < dibHeaderInfo {
		return nil, fmt.Errorf("不支持的 DIB 头大小 %d", hdrSize)
	}
	if len(dib) < hdrSize {
		return nil, errors.New("DIB 头被截断")
	}

	width := int(int32(binary.LittleEndian.Uint32(dib[4:8])))
	height := int(int32(binary.LittleEndian.Uint32(dib[8:12])))
	planes := int(binary.LittleEndian.Uint16(dib[12:14]))
	bpp := int(binary.LittleEndian.Uint16(dib[14:16]))
	compression := binary.LittleEndian.Uint32(dib[16:20])
	clrUsed := int(binary.LittleEndian.Uint32(dib[32:36]))

	if planes != 1 {
		return nil, fmt.Errorf("不支持的位面数 %d", planes)
	}
	if width <= 0 || height == 0 {
		return nil, fmt.Errorf("非法的位图尺寸 %dx%d", width, height)
	}
	// 高度为负表示自上而下（Windows 的约定），位图数据行序跟着反过来
	topDown := height < 0
	if topDown {
		height = -height
	}

	// 通道掩码：BI_BITFIELDS 时按头部里的定义取，BI_RGB 时按位深用约定值。
	var rMask, gMask, bMask, aMask uint32
	switch compression {
	case biRGB:
		switch bpp {
		case 16:
			rMask, gMask, bMask = 0x7C00, 0x03E0, 0x001F // X1R5G5B5
		case 24:
			rMask, gMask, bMask = 0xFF0000, 0x00FF00, 0x0000FF
		case 32:
			// BI_RGB 的 32 位**不定义 alpha**：第 4 个字节是无意义的填充。
			// 若把它当 alpha，绝大多数截图（那里恒为 0）会整张变透明。
			rMask, gMask, bMask = 0x00FF0000, 0x0000FF00, 0x000000FF
		}
	case biBitFields:
		if hdrSize >= dibHeaderV2 {
			rMask = binary.LittleEndian.Uint32(dib[40:44])
			gMask = binary.LittleEndian.Uint32(dib[44:48])
			bMask = binary.LittleEndian.Uint32(dib[48:52])
			if hdrSize >= dibHeaderV3 {
				aMask = binary.LittleEndian.Uint32(dib[52:56])
			}
		} else {
			// 40 字节头 + BI_BITFIELDS：三个掩码紧跟在头部之后（在调色板之前）
			if len(dib) < dibHeaderInfo+12 {
				return nil, errors.New("DIB 位域掩码被截断")
			}
			rMask = binary.LittleEndian.Uint32(dib[40:44])
			gMask = binary.LittleEndian.Uint32(dib[44:48])
			bMask = binary.LittleEndian.Uint32(dib[48:52])
		}
	default:
		return nil, fmt.Errorf("不支持的 DIB 压缩方式 %d", compression)
	}

	switch bpp {
	case 1, 4, 8, 16, 24, 32:
	default:
		return nil, fmt.Errorf("不支持的位深 %d", bpp)
	}

	// 像素起始偏移 = 头部 + （40 字节头 + BI_BITFIELDS 的内联掩码）+ 调色板
	offset := hdrSize
	if compression == biBitFields && hdrSize == dibHeaderInfo {
		offset += 12
	}
	var palette []color.NRGBA
	if bpp <= 8 {
		n := clrUsed
		if n <= 0 {
			n = 1 << bpp
		}
		if len(dib) < offset+4*n {
			return nil, errors.New("DIB 调色板被截断")
		}
		palette = make([]color.NRGBA, n)
		for i := 0; i < n; i++ {
			// 调色板项是 BGRA（第 4 字节保留），这里一律当不透明
			palette[i] = color.NRGBA{
				R: dib[offset+4*i+2],
				G: dib[offset+4*i+1],
				B: dib[offset+4*i],
				A: 255,
			}
		}
		offset += 4 * n
	}

	// 每行按 4 字节对齐（DWORD 对齐），这是 DIB 的硬约定
	stride := ((width*bpp + 31) / 32) * 4
	if len(dib) < offset+stride*height {
		return nil, errors.New("DIB 像素数据被截断")
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		srcRow := y
		if !topDown {
			srcRow = height - 1 - y
		}
		row := dib[offset+srcRow*stride : offset+srcRow*stride+stride]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for x := 0; x < width; x++ {
			var r, g, b uint8
			a := uint8(255)
			switch bpp {
			case 1:
				r, g, b = paletteRGB(palette, int((row[x>>3]>>(7-uint(x&7)))&1))
			case 4:
				v := row[x>>1]
				if x&1 == 0 {
					r, g, b = paletteRGB(palette, int(v>>4))
				} else {
					r, g, b = paletteRGB(palette, int(v&0x0F))
				}
			case 8:
				r, g, b = paletteRGB(palette, int(row[x]))
			case 16:
				v := uint32(binary.LittleEndian.Uint16(row[x*2 : x*2+2]))
				r, g, b = maskRGB(v, rMask, gMask, bMask)
				a = maskAlpha(v, aMask)
			case 24:
				b, g, r = row[x*3], row[x*3+1], row[x*3+2]
			case 32:
				v := binary.LittleEndian.Uint32(row[x*4 : x*4+4])
				r, g, b = maskRGB(v, rMask, gMask, bMask)
				a = maskAlpha(v, aMask)
			}
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, a
		}
	}
	return img, nil
}

// dibCoreToImage 解码 OS/2 的 BITMAPCOREHEADER 位图（12 字节头 + 3 字节调色板项）。
// 少见，但老程序的剪贴板里偶尔会碰到，顺手支持。
func dibCoreToImage(dib []byte) (image.Image, error) {
	if len(dib) < dibHeaderCore {
		return nil, errors.New("DIB 头被截断")
	}
	width := int(binary.LittleEndian.Uint16(dib[4:6]))
	height := int(binary.LittleEndian.Uint16(dib[6:8]))
	planes := int(binary.LittleEndian.Uint16(dib[8:10]))
	bpp := int(binary.LittleEndian.Uint16(dib[10:12]))
	if planes != 1 || width <= 0 || height <= 0 {
		return nil, errors.New("非法的 OS/2 位图头")
	}
	switch bpp {
	case 1, 4, 8, 24:
	default:
		return nil, fmt.Errorf("OS/2 位图不支持的位深 %d", bpp)
	}

	offset := dibHeaderCore
	var palette []color.NRGBA
	if bpp <= 8 {
		n := 1 << bpp
		if len(dib) < offset+3*n {
			return nil, errors.New("OS/2 调色板被截断")
		}
		palette = make([]color.NRGBA, n)
		for i := 0; i < n; i++ {
			// OS/2 调色板项是 3 字节 RGB（没有保留字节）
			palette[i] = color.NRGBA{R: dib[offset+3*i], G: dib[offset+3*i+1], B: dib[offset+3*i+2], A: 255}
		}
		offset += 3 * n
	}

	stride := ((width*bpp + 31) / 32) * 4
	if len(dib) < offset+stride*height {
		return nil, errors.New("OS/2 位图像素数据被截断")
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		// OS/2 位图恒为自下而上
		srcRow := height - 1 - y
		row := dib[offset+srcRow*stride : offset+srcRow*stride+stride]
		dst := img.Pix[y*img.Stride : y*img.Stride+width*4]
		for x := 0; x < width; x++ {
			var r, g, b uint8
			switch bpp {
			case 1:
				r, g, b = paletteRGB(palette, int((row[x>>3]>>(7-uint(x&7)))&1))
			case 4:
				v := row[x>>1]
				if x&1 == 0 {
					r, g, b = paletteRGB(palette, int(v>>4))
				} else {
					r, g, b = paletteRGB(palette, int(v&0x0F))
				}
			case 8:
				r, g, b = paletteRGB(palette, int(row[x]))
			case 24:
				b, g, r = row[x*3], row[x*3+1], row[x*3+2]
			}
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, 255
		}
	}
	return img, nil
}

// paletteRGB 取调色板颜色；下标越界（对端数据有损）时返回黑色而不是 panic。
func paletteRGB(p []color.NRGBA, i int) (uint8, uint8, uint8) {
	if i < 0 || i >= len(p) {
		return 0, 0, 0
	}
	c := p[i]
	return c.R, c.G, c.B
}

// maskChannel 取出 mask 圈定的那几位，并线性拉伸到 0~255。
// 8 位原样返回，5/6 位按比例放大（否则 0x1F 会变成 31，颜色整体发暗）。
func maskChannel(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := uint(0)
	for mask&1 == 0 {
		mask >>= 1
		shift++
	}
	bits := uint(0)
	for mask&1 == 1 {
		mask >>= 1
		bits++
	}
	if bits == 0 {
		return 0
	}
	val := (v >> shift) & (1<<bits - 1)
	max := uint32(1)<<bits - 1
	return uint8(val * 255 / max)
}

func maskRGB(v, rMask, gMask, bMask uint32) (uint8, uint8, uint8) {
	return maskChannel(v, rMask), maskChannel(v, gMask), maskChannel(v, bMask)
}

// maskAlpha 取 alpha；掩码为 0 表示「这张图没有 alpha 通道」→ 不透明。
//
// 这条是「32 位 BI_RGB 截图不会整张变透明」的关键：那种格式的 alpha 位未定义，
// 常见实现把它填 0，只有当 alpha 掩码非零时才该信它。
func maskAlpha(v, aMask uint32) uint8 {
	if aMask == 0 {
		return 255
	}
	return maskChannel(v, aMask)
}

// imageToDIB32 把图片编码成 CF_DIB 内容：BITMAPINFOHEADER(40) + 32 位 BGRA 像素（自下而上）。
//
// 用 32 位是兼容性最好的选择：画图 / Word / 浏览器都认。alpha 按真实值写入——
// BI_RGB 下它「未定义」，认的应用保留透明、不认的按不透明渲染，两种解释都不会出错。
func imageToDIB32(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	const hdr = dibHeaderInfo
	stride := w * 4
	out := make([]byte, hdr+stride*h)

	le := binary.LittleEndian
	le.PutUint32(out[0:4], hdr)
	le.PutUint32(out[4:8], uint32(int32(w)))
	le.PutUint32(out[8:12], uint32(int32(h))) // 正数 = 自下而上
	le.PutUint16(out[12:14], 1)               // biPlanes
	le.PutUint16(out[14:16], 32)              // biBitCount
	le.PutUint32(out[16:20], biRGB)           // biCompression
	le.PutUint32(out[20:24], uint32(stride*h))

	// 自下而上写入：DIB 的第一行对应图像的最后一行
	for y := 0; y < h; y++ {
		srcY := b.Max.Y - 1 - y
		dst := out[hdr+y*stride : hdr+y*stride+stride]
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, srcY)).(color.NRGBA)
			dst[x*4] = c.B
			dst[x*4+1] = c.G
			dst[x*4+2] = c.R
			dst[x*4+3] = c.A
		}
	}
	return out
}

// encodePNG 把图片规范化为 NRGBA 后编码成 PNG。
//
// 先转 NRGBA 是为了**输出确定**：同一份像素无论来源是 DIB 还是别的解码器，
// 编出来的字节完全一致。远端附件名是内容寻址（<sha256>.png），
// 字节稳定 ⇒ 同内容重复推送幂等，也才拿得到稳定的哈希做回声抑制。
func encodePNG(img image.Image) ([]byte, error) {
	b := img.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), img, b.Min, draw.Src)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, nrgba); err != nil {
		return nil, fmt.Errorf("PNG 编码失败：%w", err)
	}
	return buf.Bytes(), nil
}

// decodePNG 解码 PNG 字节。写剪贴板之前先过一遍，确认远端附件真的是张图，
// 免得把一段坏字节塞进剪贴板。
func decodePNG(data []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("PNG 解码失败：%w", err)
	}
	return img, nil
}
