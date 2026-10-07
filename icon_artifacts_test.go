package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// 本文件校验构建期产物（img/logo.ico / rsrc_windows_amd64.syso / xime-clip-sync.exe）。
//
// .syso 是手写的二进制（见 genico.go），一旦写错不会有编译错误，只会表现为
// 「exe 没图标」或「属性里没有版本号」，因此值得专门盯住。
// 产物不存在时自动跳过，这样在干净检出上 go test ./... 依然是绿的。

const (
	logoPath = "img/logo.ico"

	rsrcCharacteristics = 0x40000040 // CNT_INITIALIZED_DATA | MEM_READ
	rtIconID            = 3
	rtGroupIconID       = 14
	rtVersionID         = 16
	icoLangID           = 1033
)

func readArtifact(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		t.Skipf("%s 不存在；先运行 `go run genico.go icon.go`", name)
	}
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", name, err)
	}
	return b
}

// ---------------------------------------------------------------------------
// .ico
// ---------------------------------------------------------------------------

type testICOEntry struct {
	Width, Height int
	Planes, Bits  uint16
	Data          []byte
}

// parseICOTest 独立解析 .ico。刻意不复用 icon.go 的 parseICO——
// 两边独立实现，目录区字段的解读才算被交叉验证过。
func parseICOTest(t *testing.T, d []byte) []testICOEntry {
	t.Helper()
	if len(d) < 6 {
		t.Fatalf("ICO 只有 %d 字节", len(d))
	}
	reserved := binary.LittleEndian.Uint16(d[0:])
	typ := binary.LittleEndian.Uint16(d[2:])
	count := int(binary.LittleEndian.Uint16(d[4:]))
	if reserved != 0 || typ != 1 {
		t.Fatalf("ICO 头不对：reserved=%d type=%d（期望 0/1）", reserved, typ)
	}
	if count <= 0 {
		t.Fatal("ICO 里没有任何图像")
	}
	if len(d) < 6+16*count {
		t.Fatalf("ICO 目录区被截断")
	}

	out := make([]testICOEntry, 0, count)
	for i := 0; i < count; i++ {
		e := d[6+i*16:]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		planes := binary.LittleEndian.Uint16(e[4:])
		bpp := binary.LittleEndian.Uint16(e[6:])
		nbytes := int(binary.LittleEndian.Uint32(e[8:]))
		offset := int(binary.LittleEndian.Uint32(e[12:]))

		if w != h {
			t.Errorf("第 %d 档不是正方形：%dx%d", i, w, h)
		}
		if planes != 1 {
			t.Errorf("%d×%d 的 planes=%d，期望 1", w, h, planes)
		}
		if offset < 6+16*count || nbytes <= 0 || offset+nbytes > len(d) {
			t.Fatalf("%d×%d 的图像数据越界（offset=%d len=%d 文件长度=%d）",
				w, h, offset, nbytes, len(d))
		}
		out = append(out, testICOEntry{
			Width: w, Height: h, Planes: planes, Bits: bpp,
			Data: d[offset : offset+nbytes],
		})
	}
	return out
}

// TestLogoICOIsUsable 确认 img/logo.ico 里每一档都能当作图标资源使用。
//
// 两种合法形态都接受：DIB（BITMAPINFOHEADER + XOR + AND）和 PNG
// （Vista 起 RT_ICON 与 CreateIconFromResourceEx 都支持 PNG 压缩的档）。
func TestLogoICOIsUsable(t *testing.T) {
	entries := parseICOTest(t, readArtifact(t, logoPath))
	if len(entries) < 2 {
		t.Fatalf("logo.ico 只有 %d 档，至少要有大小两档", len(entries))
	}

	seen := map[int]bool{}
	has16, has32 := false, false
	for _, e := range entries {
		if seen[e.Width] {
			t.Errorf("尺寸 %d 出现了两次", e.Width)
		}
		seen[e.Width] = true
		if e.Width == 16 {
			has16 = true
		}
		if e.Width == 32 {
			has32 = true
		}
		checkIconImage(t, e)
	}
	// 16 给托盘（小图标），32 给资源管理器的列表视图——缺了图标会糊
	if !has16 {
		t.Error("logo.ico 缺少 16×16，托盘小图标会由系统缩放，容易糊")
	}
	if !has32 {
		t.Error("logo.ico 缺少 32×32")
	}
}

func checkIconImage(t *testing.T, e testICOEntry) {
	t.Helper()
	if bytes.HasPrefix(e.Data, []byte("\x89PNG\r\n\x1a\n")) {
		if !bytes.HasSuffix(e.Data, []byte("IEND\xaeB`\x82")) {
			t.Errorf("%d×%d 是 PNG 档但结尾不是 IEND，数据可能被截断", e.Width, e.Height)
		}
		return
	}
	if len(e.Data) < 40 {
		t.Fatalf("%d×%d 的 DIB 只有 %d 字节", e.Width, e.Height, len(e.Data))
	}
	le := binary.LittleEndian
	if hdr := le.Uint32(e.Data[0:]); hdr != 40 {
		t.Errorf("%d×%d 的 biSize=%d，期望 40（BITMAPINFOHEADER）", e.Width, e.Height, hdr)
	}
	w := int(int32(le.Uint32(e.Data[4:])))
	h2 := int(int32(le.Uint32(e.Data[8:])))
	if w != e.Width || h2 != e.Height*2 {
		t.Errorf("%d×%d 的 DIB 头是 %dx%d，期望 %d/%d（高度须含 XOR+AND 两部分）",
			e.Width, e.Height, w, h2, e.Width, e.Height*2)
	}
	bpp := le.Uint16(e.Data[14:])
	if bpp != 32 {
		// 32bpp 之外的档没有 alpha，托盘上会出现黑边，值得提醒
		t.Logf("提示：%d×%d 是 %dbpp，没有 alpha 通道", e.Width, e.Height, bpp)
	}
	xorLen := e.Width * e.Height * 4
	andRow := ((e.Width + 31) / 32) * 4
	andLen := andRow * e.Height
	if want := 40 + xorLen + andLen; len(e.Data) != want {
		t.Errorf("%d×%d 的 DIB 长度=%d，期望 %d（40 + XOR %d + AND %d）",
			e.Width, e.Height, len(e.Data), want, xorLen, andLen)
	}
}

// ---------------------------------------------------------------------------
// .syso（COFF 对象）
// ---------------------------------------------------------------------------

// loadSYSO 读出 .syso 的 .rsrc 段与资源目录树，并顺带校验 COFF 骨架。
func loadSYSO(t *testing.T) ([]byte, map[[3]uint32][2]uint32) {
	t.Helper()
	d := readArtifact(t, "rsrc_windows_amd64.syso")
	if len(d) < 60 {
		t.Fatalf("COFF 只有 %d 字节", len(d))
	}

	machine := binary.LittleEndian.Uint16(d[0:])
	nsec := binary.LittleEndian.Uint16(d[2:])
	symOff := int(binary.LittleEndian.Uint32(d[8:]))
	nsym := binary.LittleEndian.Uint32(d[12:])
	optSize := binary.LittleEndian.Uint16(d[16:])

	if machine != 0x8664 {
		t.Errorf("Machine=%#x，期望 0x8664（AMD64）", machine)
	}
	if nsec != 1 {
		t.Fatalf("NumberOfSections=%d，期望 1", nsec)
	}
	if optSize != 0 {
		t.Errorf("SizeOfOptionalHeader=%d，对象文件应为 0", optSize)
	}
	if nsym != 1 {
		t.Errorf("NumberOfSymbols=%d，期望 1（只需一个段符号）", nsym)
	}

	sec := d[20:60]
	if name := string(bytes.TrimRight(sec[0:8], "\x00")); name != ".rsrc" {
		t.Fatalf("段名=%q，期望 .rsrc", name)
	}
	vsize := binary.LittleEndian.Uint32(sec[8:])
	rawSize := binary.LittleEndian.Uint32(sec[16:])
	rawOff := int(binary.LittleEndian.Uint32(sec[20:]))
	relOff := int(binary.LittleEndian.Uint32(sec[24:]))
	nrel := int(binary.LittleEndian.Uint16(sec[32:]))
	chars := binary.LittleEndian.Uint32(sec[36:])

	if vsize != rawSize {
		t.Errorf("VirtualSize=%d != SizeOfRawData=%d，链接器读到的数据长度会和声明不符", vsize, rawSize)
	}
	// 这两个条件缺一不可：特征位不对，Go 链接器会直接跳过整个段，
	// 结果就是 exe 编出来了但没有图标（见 cmd/link/internal/loadpe/ldpe.go）。
	if chars != rsrcCharacteristics {
		t.Errorf("Characteristics=%#x，期望 %#x", chars, rsrcCharacteristics)
	}
	if chars&0x02000000 != 0 {
		t.Error("Characteristics 带了 MEM_DISCARDABLE，链接器会跳过该段")
	}
	if rawOff+int(rawSize) > len(d) {
		t.Fatalf("段数据越界：%d+%d > %d", rawOff, rawSize, len(d))
	}
	rsrc := d[rawOff : rawOff+int(rawSize)]

	// 段符号必须存在且是 STATIC，否则重定位解析不到目标
	if symOff+18 > len(d) {
		t.Fatalf("符号表越界")
	}
	sym := d[symOff:]
	if name := string(bytes.TrimRight(sym[0:8], "\x00")); name != ".rsrc" {
		t.Errorf("符号名=%q，期望 .rsrc", name)
	}
	if secNum := binary.LittleEndian.Uint16(sym[12:]); secNum != 1 {
		t.Errorf("符号 SectionNumber=%d，期望 1", secNum)
	}
	if sym[16] != 3 {
		t.Errorf("符号 StorageClass=%d，期望 3（IMAGE_SYM_CLASS_STATIC）", sym[16])
	}

	// 每处重定位都要指向段符号、类型正确、且落在段内
	if nrel == 0 {
		t.Fatal("没有重定位：资源目录里的 RVA 不会被修正，exe 里图标会失效")
	}
	for i := 0; i < nrel; i++ {
		if relOff+(i+1)*10 > len(d) {
			t.Fatalf("第 %d 处重定位越界", i)
		}
		e := d[relOff+i*10:]
		off := int(binary.LittleEndian.Uint32(e[0:]))
		idx := binary.LittleEndian.Uint32(e[4:])
		typ := binary.LittleEndian.Uint16(e[8:])
		if idx != 0 {
			t.Errorf("第 %d 处重定位的符号下标=%d，期望 0（段符号）", i, idx)
		}
		if typ != 0x0002 {
			t.Errorf("第 %d 处重定位类型=%#x，期望 0x0002（ADDR32）", i, typ)
		}
		if off+4 > len(rsrc) {
			t.Fatalf("第 %d 处重定位偏移 %d 超出段长 %d", i, off, len(rsrc))
		}
		// 重定位点上存放的是 addend（载荷的段内偏移），链接器会把它加上段基址
		if addend := binary.LittleEndian.Uint32(rsrc[off:]); int(addend) >= len(rsrc) {
			t.Errorf("第 %d 处 addend=%d 超出段长 %d", i, addend, len(rsrc))
		}
	}

	return rsrc, walkRsrc(t, rsrc)
}

func TestSYSOIsValidCOFF(t *testing.T) {
	rsrc, tree := loadSYSO(t)
	logo := parseICOTest(t, readArtifact(t, logoPath))
	checkRsrcTree(t, rsrc, tree, logo)
}

// TestSYSOIconMatchesLogoICO 断言 .syso 里的每份 RT_ICON 与 img/logo.ico
// 里对应的一档**逐字节一致**。
//
// 这一条守住的是「改了 img/logo.ico 却忘了重新生成 .syso」——
// 那种情况下编译照样通过，只有 exe 的图标会悄悄停留在旧图上。
func TestSYSOIconMatchesLogoICO(t *testing.T) {
	rsrc, tree := loadSYSO(t)
	logo := parseICOTest(t, readArtifact(t, logoPath))

	for i, e := range logo {
		id := uint32(i + 1)
		loc, ok := tree[[3]uint32{rtIconID, id, icoLangID}]
		if !ok {
			t.Fatalf("缺少 RT_ICON id=%d", id)
		}
		got := rsrc[loc[0] : loc[0]+loc[1]]
		if !bytes.Equal(got, e.Data) {
			t.Errorf("RT_ICON id=%d（%d×%d）与 logo.ico 不一致：%d 字节 vs %d 字节"+
				"（改了 logo.ico 没重新生成 .syso？）",
				id, e.Width, e.Height, len(got), len(e.Data))
		}
	}
}

// walkRsrc 遍历资源目录树，返回「[类型,名称,语言] -> [载荷偏移, 长度]」。
// 偏移是段内偏移（在 .syso 里还没被链接器改写成 RVA）。
func walkRsrc(t *testing.T, rsrc []byte) map[[3]uint32][2]uint32 {
	t.Helper()
	out := make(map[[3]uint32][2]uint32)

	var walk func(off uint32, path [3]uint32, depth int)
	walk = func(off uint32, path [3]uint32, depth int) {
		if depth > 2 {
			t.Fatalf("资源目录层级过深（depth=%d），说明树结构不对", depth)
		}
		if int(off)+16 > len(rsrc) {
			t.Fatalf("目录偏移 %d 越界（段长 %d）", off, len(rsrc))
		}
		named := binary.LittleEndian.Uint16(rsrc[off+12:])
		ids := binary.LittleEndian.Uint16(rsrc[off+14:])
		if named != 0 {
			t.Errorf("本项目不使用字符串资源名，但 NumberOfNamedEntries=%d", named)
		}
		for i := 0; i < int(ids); i++ {
			e := int(off) + 16 + i*8
			if e+8 > len(rsrc) {
				t.Fatalf("目录项越界（off=%d i=%d）", off, i)
			}
			id := binary.LittleEndian.Uint32(rsrc[e:])
			sub := binary.LittleEndian.Uint32(rsrc[e+4:])
			np := path
			np[depth] = id
			if sub&0x80000000 != 0 {
				walk(sub&0x7FFFFFFF, np, depth+1)
				continue
			}
			do := int(sub)
			if do+16 > len(rsrc) {
				t.Fatalf("数据项偏移 %d 越界", do)
			}
			payload := binary.LittleEndian.Uint32(rsrc[do:])
			size := binary.LittleEndian.Uint32(rsrc[do+4:])
			if int(payload)+int(size) > len(rsrc) {
				t.Errorf("%v 的载荷越界：偏移 %d 长度 %d，段长 %d", np, payload, size, len(rsrc))
			}
			out[np] = [2]uint32{payload, size}
		}
	}
	walk(0, [3]uint32{}, 0)
	return out
}

// checkRsrcTree 校验资源目录内容：三种类型、每档图标一个 RT_ICON、组图标指向正确。
func checkRsrcTree(t *testing.T, rsrc []byte, tree map[[3]uint32][2]uint32, logo []testICOEntry) {
	t.Helper()
	nIcons := len(logo)

	for i := 1; i <= nIcons; i++ {
		if _, ok := tree[[3]uint32{rtIconID, uint32(i), icoLangID}]; !ok {
			t.Errorf("缺少 RT_ICON id=%d lang=%d", i, icoLangID)
		}
	}
	group, ok := tree[[3]uint32{rtGroupIconID, 1, icoLangID}]
	if !ok {
		t.Fatalf("缺少 RT_GROUP_ICON id=1 lang=%d", icoLangID)
	}
	ver, ok := tree[[3]uint32{rtVersionID, 1, icoLangID}]
	if !ok {
		t.Fatalf("缺少 RT_VERSION id=1 lang=%d", icoLangID)
	}
	if len(tree) != nIcons+2 {
		t.Errorf("资源项共 %d 个，期望 %d 个", len(tree), nIcons+2)
	}

	// 组图标：条目数、尺寸、指向的 RT_ICON 大小都要对得上
	g := rsrc[group[0] : group[0]+group[1]]
	if len(g) < 6 {
		t.Fatalf("GRPICONDIR 太短：%d 字节", len(g))
	}
	if r, typ, n := binary.LittleEndian.Uint16(g[0:]), binary.LittleEndian.Uint16(g[2:]), binary.LittleEndian.Uint16(g[4:]); r != 0 || typ != 1 || int(n) != nIcons {
		t.Fatalf("GRPICONDIR 头不对：reserved=%d type=%d count=%d（期望 0/1/%d）", r, typ, n, nIcons)
	}
	if len(g) != 6+14*nIcons {
		t.Fatalf("GRPICONDIR 长度=%d，期望 %d", len(g), 6+14*nIcons)
	}
	for i := 0; i < nIcons; i++ {
		e := g[6+i*14:]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		nbytes := binary.LittleEndian.Uint32(e[8:])
		nid := binary.LittleEndian.Uint16(e[12:])
		if w != logo[i].Width {
			t.Errorf("GRPICONDIR 第 %d 项尺寸=%d，logo.ico 里是 %d", i, w, logo[i].Width)
		}
		if int(nid) != i+1 {
			t.Errorf("GRPICONDIR 第 %d 项指向 RT_ICON id=%d，期望 %d", i, nid, i+1)
		}
		icon, ok := tree[[3]uint32{rtIconID, uint32(nid), icoLangID}]
		if !ok {
			t.Fatalf("GRPICONDIR 第 %d 项指向了不存在的 RT_ICON id=%d", i, nid)
		}
		if icon[1] != nbytes {
			t.Errorf("RT_ICON id=%d 实际 %d 字节，组图标里声明 %d 字节", nid, icon[1], nbytes)
		}
	}

	// 版本资源：头部三个 WORD 必须自洽
	v := rsrc[ver[0] : ver[0]+ver[1]]
	if len(v) < 6 {
		t.Fatalf("VS_VERSIONINFO 太短：%d 字节", len(v))
	}
	wLength := binary.LittleEndian.Uint16(v[0:])
	wValueLength := binary.LittleEndian.Uint16(v[2:])
	wType := binary.LittleEndian.Uint16(v[4:])
	if int(wLength) != len(v) {
		t.Errorf("VS_VERSIONINFO wLength=%d，实际 %d 字节", wLength, len(v))
	}
	if wValueLength != 52 {
		t.Errorf("VS_VERSIONINFO wValueLength=%d，期望 52（VS_FIXEDFILEINFO 大小）", wValueLength)
	}
	if wType != 0 {
		t.Errorf("VS_VERSIONINFO wType=%d，期望 0", wType)
	}
	if !bytes.HasPrefix(v[6:], []byte("V\x00S\x00_\x00V\x00E\x00R\x00")) {
		t.Error("VS_VERSIONINFO 的 szKey 不是 VS_VERSION_INFO")
	}
	// VS_FIXEDFILEINFO 的签名
	if sig := binary.LittleEndian.Uint32(v[40:]); sig != 0xFEEF04BD {
		t.Errorf("VS_FIXEDFILEINFO dwSignature=%#x，期望 0xFEEF04BD", sig)
	}
}
