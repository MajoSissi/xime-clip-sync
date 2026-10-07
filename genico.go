//go:build ignore

// genico 是构建期资源生成器，不属于应用本身（靠 //go:build ignore 排除）。
//
// 它把 img/logo.ico 转成 rsrc_windows_amd64.syso —— 一个 Windows 资源对象，
// go build 会自动链接进去，让 exe 在资源管理器、任务栏、Alt-Tab 里显示
// logo.ico 的图标，并在「属性」里显示名称与版本号。
//
// 图标**不在代码里画**：ico 里的每档图像本来就是 DIB，正好是 RT_ICON 需要的
// 格式，所以这里是原样搬运，只额外生成 RT_GROUP_ICON 与 RT_VERSION。
// 要换图标就换 img/logo.ico，不用改任何代码。
//
// 用法（在本目录下）：
//
//	go run genico.go icon.go
//
// 为什么自己生成 .syso 而不是用 windres：本项目坚持零第三方依赖、零外部
// 工具链，而 .syso 只是一个很简单的 COFF 对象文件（一个 .rsrc 段 + 若干
// 重定位 + 一个段符号），完全可以手写。版本号直接从 main.go 里解析，
// 因此永远不会和二进制里的 -version 输出不一致。
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func appU16(b []byte, v uint16) []byte { return append(b, byte(v), byte(v>>8)) }
func appU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// align4 把切片补齐到 4 字节边界（append 可能重新分配，务必接住返回值）。
func align4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// ---------------------------------------------------------------------------
// .ico 输入
// ---------------------------------------------------------------------------

// icoDim 把像素尺寸转成 ICO / GRPICONDIR 里的宽度字节（256 用 0 表示）。
func icoDim(size int) byte {
	if size >= 256 {
		return 0
	}
	return byte(size)
}

// ---------------------------------------------------------------------------
// 资源目录树
// ---------------------------------------------------------------------------

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	langID      = 1033 // 0x0409 en-US，与版本资源里的 Translation 保持一致
)

// resLeaf 是一份资源载荷，offset 在 writePayloads 阶段回填（相对 .rsrc 段首）。
type resLeaf struct {
	data   []byte
	offset uint32
}

// resEntry 是资源目录里的一项。本项目全部使用数字 ID，因此不需要字符串名称。
type resEntry struct {
	id   uint32
	dir  *resDir
	leaf *resLeaf
}

type resDir struct{ entries []resEntry }

// reloc 记录一处需要写 RVA 的位置。链接器会把 addend 替换成
// 段虚拟地址 + addend，所以 addend 就是载荷在段内的偏移。
type reloc struct {
	off  int
	leaf *resLeaf
}

type rsrcBuilder struct {
	buf    []byte
	relocs []reloc
}

// writeDir 把目录写到 buf 末尾并返回其段内偏移；子目录与数据项紧随其后。
//
// 注意：递归 append 会让 buf 重新分配，因此全程用整数下标访问，绝不缓存切片。
func (b *rsrcBuilder) writeDir(d *resDir) uint32 {
	off := uint32(len(b.buf))

	hdr := make([]byte, 16) // IMAGE_RESOURCE_DIRECTORY
	putU16(hdr[12:], 0)     // NumberOfNamedEntries
	putU16(hdr[14:], uint16(len(d.entries)))
	b.buf = append(b.buf, hdr...)

	entryStart := len(b.buf)
	b.buf = append(b.buf, make([]byte, 8*len(d.entries))...) // IMAGE_RESOURCE_DIRECTORY_ENTRY[]

	for i, e := range d.entries {
		var offField uint32
		if e.dir != nil {
			offField = b.writeDir(e.dir) | 0x80000000 // 最高位=1 表示指向子目录
		} else {
			b.buf = align4(b.buf)
			dataEntryOff := uint32(len(b.buf))
			pos := len(b.buf)
			b.buf = append(b.buf, make([]byte, 16)...) // IMAGE_RESOURCE_DATA_ENTRY
			putU32(b.buf[pos+4:], uint32(len(e.leaf.data)))
			b.relocs = append(b.relocs, reloc{off: pos, leaf: e.leaf})
			offField = dataEntryOff
		}
		p := entryStart + i*8
		putU32(b.buf[p:], e.id)
		putU32(b.buf[p+4:], offField)
	}
	return off
}

// writePayloads 把所有载荷追加到段尾，并回填 RVA 占位（链接器读取这里的
// 4 字节作为 addend）。
func (b *rsrcBuilder) writePayloads(leaves []*resLeaf) {
	for _, lf := range leaves {
		b.buf = align4(b.buf)
		lf.offset = uint32(len(b.buf))
		b.buf = append(b.buf, lf.data...)
	}
	for _, r := range b.relocs {
		putU32(b.buf[r.off:], r.leaf.offset)
	}
	b.buf = align4(b.buf)
}

// buildRsrc 组装完整的 .rsrc 段数据，并返回需要写 RVA 的重定位位置。
func buildRsrc(verPayload []byte, icons []icoEntry) ([]byte, []reloc) {
	var leaves []*resLeaf
	newLeaf := func(data []byte) *resLeaf {
		lf := &resLeaf{data: data}
		leaves = append(leaves, lf)
		return lf
	}
	// 叶子统一挂在「语言」这一层下面
	lang := func(id uint32, lf *resLeaf) resEntry {
		return resEntry{id: id, dir: &resDir{entries: []resEntry{{id: langID, leaf: lf}}}}
	}

	iconEntries := make([]resEntry, 0, len(icons))
	for i, ic := range icons {
		iconEntries = append(iconEntries, lang(uint32(i+1), newLeaf(ic.Data)))
	}

	root := &resDir{entries: []resEntry{
		{id: rtIcon, dir: &resDir{entries: iconEntries}},
		{id: rtGroupIcon, dir: &resDir{entries: []resEntry{
			lang(1, newLeaf(groupIconPayload(icons))),
		}}},
		{id: rtVersion, dir: &resDir{entries: []resEntry{
			lang(1, newLeaf(verPayload)),
		}}},
	}}

	b := &rsrcBuilder{}
	b.writeDir(root)
	b.writePayloads(leaves)
	return b.buf, b.relocs
}

// groupIconPayload 生成 RT_GROUP_ICON（GRPICONDIR）内容。
//
// 各字段直接沿用 ICO 目录里的原值（尺寸/planes/位数），
// 只有 nID 换成 RT_ICON 的编号（ICO 文件里存的是数据偏移，这里必须换掉）。
func groupIconPayload(icons []icoEntry) []byte {
	var b []byte
	b = appU16(b, 0)                  // idReserved
	b = appU16(b, 1)                  // idType = icon
	b = appU16(b, uint16(len(icons))) // idCount
	for i, ic := range icons {
		b = append(b, icoDim(ic.Width), icoDim(ic.Height), 0, 0)
		b = appU16(b, ic.Planes)            // wPlanes
		b = appU16(b, ic.Bits)              // wBitCount
		b = appU32(b, uint32(len(ic.Data))) // dwBytesInRes
		b = appU16(b, uint16(i+1))          // nID（指向 RT_ICON 的 ID）
	}
	return b
}

// ---------------------------------------------------------------------------
// 版本资源
// ---------------------------------------------------------------------------

// verNode 拼一个版本资源节点：
//
//	WORD wLength; WORD wValueLength; WORD wType; WCHAR szKey[];
//	<4 字节对齐> value; <4 字节对齐> children
func verNode(key string, valueLen, typ uint16, value []byte, children ...[]byte) []byte {
	var b []byte
	b = append(b, make([]byte, 6)...) // 头部占位，最后回填
	for _, r := range key {
		b = appU16(b, uint16(r))
	}
	b = appU16(b, 0) // 以 NUL 结尾
	b = align4(b)
	b = append(b, value...)
	b = align4(b)
	for _, c := range children {
		b = append(b, c...)
	}
	b = align4(b)
	putU16(b[0:], uint16(len(b)))
	putU16(b[2:], valueLen)
	putU16(b[4:], typ)
	return b
}

// verString 是 StringFileInfo 下的一个键值对，wValueLength 以「字符」计（含结尾 NUL）。
func verString(name, value string) []byte {
	var v []byte
	for _, r := range value {
		v = appU16(v, uint16(r))
	}
	v = appU16(v, 0)
	return verNode(name, uint16(len(v)/2), 1, v)
}

// buildVersionPayload 生成 RT_VERSION 内容。
func buildVersionPayload(version string, verMS, verLS uint32) []byte {
	ffi := make([]byte, 52) // VS_FIXEDFILEINFO
	putU32(ffi[0:], 0xFEEF04BD)
	putU32(ffi[4:], 0x00010000) // dwStrucVersion
	putU32(ffi[8:], verMS)      // dwFileVersionMS
	putU32(ffi[12:], verLS)     // dwFileVersionLS
	putU32(ffi[16:], verMS)     // dwProductVersionMS
	putU32(ffi[20:], verLS)     // dwProductVersionLS
	putU32(ffi[24:], 0x3F)      // dwFileFlagsMask
	putU32(ffi[28:], 0)         // dwFileFlags
	putU32(ffi[32:], 0x40004)   // dwFileOS = VOS_NT_WINDOWS32
	putU32(ffi[36:], 1)         // dwFileType = VFT_APP
	putU32(ffi[40:], 0)         // dwFileSubtype
	putU32(ffi[44:], 0)         // dwFileDateMS
	putU32(ffi[48:], 0)         // dwFileDateLS

	table := verNode("040904B0", 0, 1, nil,
		verString("CompanyName", "Xime"),
		verString("FileDescription", "Xime Clip Sync 剪贴板同步"),
		verString("FileVersion", version+".0"),
		verString("InternalName", "xime-clip-sync"),
		verString("OriginalFilename", "xime-clip-sync.exe"),
		verString("ProductName", "Xime Clip Sync"),
		verString("ProductVersion", version),
	)

	trans := make([]byte, 4)
	putU16(trans[0:], langID)
	putU16(trans[2:], 1200) // 0x04B0 = Unicode

	return verNode("VS_VERSION_INFO", 52, 0, ffi,
		verNode("StringFileInfo", 0, 1, nil, table),
		verNode("VarFileInfo", 0, 1, nil, verNode("Translation", 4, 0, trans)),
	)
}

// ---------------------------------------------------------------------------
// COFF 对象文件（.syso）
// ---------------------------------------------------------------------------

const (
	machineAMD64 = 0x8664

	// Go 链接器只接受这两个值之一，且两者数值相同
	relocADDR32 = 0x0002

	scnCntInitializedData = 0x00000040
	scnMemRead            = 0x40000000

	symClassStatic = 3
)

// buildCOFF 生成一个只含 .rsrc 段的 COFF 对象。
//
// 布局：COFF 头(20) + 段头(40) + 段数据 + 重定位表 + 符号表(1 项) + 字符串表。
// 重定位指向「段符号」，链接器于是把段内偏移改写成真正的 RVA。
func buildCOFF(machine uint16, data []byte, relocs []reloc) []byte {
	const (
		hdrSize  = 20
		sectSize = 40
		symSize  = 18
		relSize  = 10
	)

	data = align4(data) // 保证重定位表 4 字节对齐
	rawOff := hdrSize + sectSize
	rawLen := len(data)
	relOff := rawOff + rawLen
	symOff := relOff + len(relocs)*relSize
	strOff := symOff + symSize

	b := make([]byte, strOff+4)

	// ---- COFF 文件头 ----
	putU16(b[0:], machine)
	putU16(b[2:], 1) // NumberOfSections
	putU32(b[4:], 0) // TimeDateStamp
	putU32(b[8:], uint32(symOff))
	putU32(b[12:], 1) // NumberOfSymbols
	putU16(b[16:], 0) // SizeOfOptionalHeader（对象文件为 0）
	putU16(b[18:], 0) // Characteristics

	// ---- 段头 ----
	sec := b[hdrSize:]
	copy(sec[0:8], ".rsrc\x00\x00\x00")
	putU32(sec[8:], uint32(rawLen))  // VirtualSize
	putU32(sec[12:], 0)              // VirtualAddress（链接器填）
	putU32(sec[16:], uint32(rawLen)) // SizeOfRawData
	putU32(sec[20:], uint32(rawOff)) // PointerToRawData
	putU32(sec[24:], uint32(relOff)) // PointerToRelocations
	putU32(sec[28:], 0)              // PointerToLinenumbers
	putU16(sec[32:], uint16(len(relocs)))
	putU16(sec[34:], 0) // NumberOfLinenumbers
	// 关键：必须恰好是 CNT_INITIALIZED_DATA|MEM_READ，且不能带 MEM_DISCARDABLE，
	// 否则 Go 链接器会直接跳过这个段（见 cmd/link/internal/loadpe/ldpe.go）。
	putU32(sec[36:], scnCntInitializedData|scnMemRead)

	// ---- 段数据 ----
	copy(b[rawOff:], data)

	// ---- 重定位表 ----
	for i, r := range relocs {
		e := b[relOff+i*relSize:]
		putU32(e[0:], uint32(r.off))
		putU32(e[4:], 0) // 符号表下标 0 = 段符号
		putU16(e[8:], relocADDR32)
	}

	// ---- 符号表：一个段符号 ----
	sym := b[symOff:]
	copy(sym[0:8], ".rsrc\x00\x00\x00")
	putU32(sym[8:], 0)  // Value
	putU16(sym[12:], 1) // SectionNumber（1 起）
	putU16(sym[14:], 0) // Type
	sym[16] = symClassStatic
	sym[17] = 0 // NumberOfAuxSymbols

	// ---- 字符串表（最小：只含自身的长度字段）----
	putU32(b[strOff:], 4)
	return b
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

// versionRe 从 main.go 里抠出版本号，避免资源版本与 -version 输出不一致。
var versionRe = regexp.MustCompile(`\bversion\s*=\s*"([0-9][0-9.]*)"`)

func parseVersion(src []byte) (string, uint32, uint32, error) {
	m := versionRe.FindSubmatch(src)
	if m == nil {
		return "", 0, 0, fmt.Errorf("在 main.go 里没找到 version = \"x.y.z\"")
	}
	v := string(m[1])
	parts := []int{0, 0, 0, 0}
	fields := strings.Split(v, ".")
	if len(fields) > len(parts) {
		return "", 0, 0, fmt.Errorf("版本号 %q 段数过多", v)
	}
	for i, s := range fields {
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", 0, 0, fmt.Errorf("版本号 %q 解析失败：%w", v, err)
		}
		parts[i] = n
	}
	ms := uint32(parts[0])<<16 | uint32(parts[1])
	ls := uint32(parts[2])<<16 | uint32(parts[3])
	return v, ms, ls, nil
}

func main() {
	src, err := os.ReadFile("main.go")
	if err != nil {
		fatal("读取 main.go 失败：%v", err)
	}
	version, verMS, verLS, err := parseVersion(src)
	if err != nil {
		fatal("%v", err)
	}

	// 图标来自 img/logo.ico（内嵌在 icon.go 里）。要换图标就换这个文件。
	icons, err := parseICO(logoICO)
	if err != nil {
		fatal("解析 img/logo.ico 失败：%v", err)
	}
	sizes := make([]string, 0, len(icons))
	for _, ic := range icons {
		sizes = append(sizes, strconv.Itoa(ic.Width))
	}
	fmt.Printf("img/logo.ico        %d 档尺寸（%s），共 %d 字节\n",
		len(icons), strings.Join(sizes, "/"), len(logoICO))

	rsrc, relocs := buildRsrc(buildVersionPayload(version, verMS, verLS), icons)
	const sysoName = "rsrc_windows_amd64.syso"
	obj := buildCOFF(machineAMD64, rsrc, relocs)
	if err := os.WriteFile(sysoName, obj, 0o644); err != nil {
		fatal("写 %s 失败：%v", sysoName, err)
	}
	fmt.Printf("%-24s .rsrc %d 字节，%d 处重定位，对象共 %d 字节\n",
		sysoName, len(rsrc), len(relocs), len(obj))

	fmt.Printf("版本资源            %s（FileVersion %s.0）\n", version, version)
	fmt.Println("完成。重新 go build 即可让 exe 带上图标与版本信息。")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genico: "+format+"\n", args...)
	os.Exit(1)
}
