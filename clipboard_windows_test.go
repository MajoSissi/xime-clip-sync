//go:build windows

package main

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestRealClipboardRoundTrip 在真实的 Windows 剪贴板上验证读写实现
// （UTF-16 转换、RtlMoveMemory 拷贝、剪贴板占用重试）。
//
// 该测试会临时改写剪贴板，结束后恢复原内容；用 -short 可跳过。
func TestRealClipboardRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	cb := newClipboard()

	original, err := cb.GetText()
	if err != nil {
		t.Fatalf("读取原始剪贴板失败：%v", err)
	}
	defer func() {
		if err := cb.SetText(original); err != nil {
			t.Logf("恢复剪贴板失败：%v", err)
		}
	}()

	cases := []string{
		"hello",
		"中文剪贴板同步测试",
		"emoji 🙂🚀 混排",
		"tab\there\nnewline\r\nend",
		strings.Repeat("长文本", 500),
		"",
	}
	for _, want := range cases {
		if err := cb.SetText(want); err != nil {
			t.Fatalf("写入剪贴板失败（%q）：%v", preview(want, 20), err)
		}
		got, err := cb.GetText()
		if err != nil {
			t.Fatalf("读取剪贴板失败（%q）：%v", preview(want, 20), err)
		}
		if got != want {
			t.Errorf("往返不一致：写入 %q，读回 %q", preview(want, 40), preview(got, 40))
		}
	}

	// 序号应随写入递增，供本地变更检测使用
	if sc, ok := cb.(seqClipboard); ok {
		before, ok1 := sc.Seq()
		if !ok1 {
			t.Fatal("Seq 不可用")
		}
		if err := cb.SetText("seq-check"); err != nil {
			t.Fatal(err)
		}
		after, _ := sc.Seq()
		if after == before {
			t.Errorf("写入后剪贴板序号未变化：%d", after)
		}
	} else {
		t.Error("Windows 剪贴板应实现 seqClipboard")
	}
}

// 确认 UTF-16 编解码与 Windows 的 NUL 结尾约定一致
func TestUTF16Encoding(t *testing.T) {
	text := "abc中文🙂"
	units := utf16.Encode([]rune(text))
	if len(units) != 3+2+2 {
		t.Errorf("UTF-16 码元数量不符：%d", len(units))
	}
	if string(utf16.Decode(units)) != text {
		t.Errorf("UTF-16 往返失败")
	}
}

// TestRealClipboardImageRoundTrip 在真实 Windows 剪贴板上验证位图读写
// （PNG 注册格式 / CF_DIB 的取图顺序、SetImage 的两种格式写入、像素往返）。
//
// 会临时改写剪贴板（结束后恢复原文本）；用 -short 可跳过。
//
// 用**不透明**图片是有意的：imageToDIB32 写的是 32 位 BI_RGB，那种格式的 alpha
// 未定义、读回来按不透明解释。不透明像素在「PNG 格式」和「DIB 回退」两条路径上
// 结果一致，所以能顺带钉住「同一张图读回来字节不变」——回声抑制依赖这一点。
func TestRealClipboardImageRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	cb := newClipboard()
	ic, ok := cb.(imageClipboard)
	if !ok {
		t.Fatal("Windows 剪贴板应实现 imageClipboard")
	}

	original, err := cb.GetText()
	if err != nil {
		t.Fatalf("读取原始剪贴板失败：%v", err)
	}
	defer func() {
		if err := cb.SetText(original); err != nil {
			t.Logf("恢复剪贴板失败：%v", err)
		}
	}()

	// 纯文本剪贴板不该被认成图片
	if err := cb.SetText("只有文本"); err != nil {
		t.Fatal(err)
	}
	if ic.HasImage() {
		t.Error("纯文本剪贴板不该被报告为有图片")
	}

	// 造一张不透明的图（尺寸带奇数列，顺便验证行对齐）
	src := image.NewNRGBA(image.Rect(0, 0, 7, 5))
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 30), G: uint8(y * 50), B: 128, A: 255})
		}
	}
	png, err := encodePNG(src)
	if err != nil {
		t.Fatal(err)
	}

	if err := ic.SetImage(png); err != nil {
		t.Fatalf("写入剪贴板图片失败：%v", err)
	}
	if !ic.HasImage() {
		t.Fatal("写入后 HasImage 应为真")
	}
	// 图片剪贴板里不该再冒出文本（否则对端会把图片当成一段空文本）
	if got, err := cb.GetText(); err != nil || got != "" {
		t.Errorf("图片剪贴板读出的文本应为空，实际 %q（err=%v）", got, err)
	}

	got, err := ic.GetImage()
	if err != nil {
		t.Fatalf("读取剪贴板图片失败：%v", err)
	}
	if len(got) == 0 {
		t.Fatal("读回的图片为空")
	}
	back, err := decodePNG(got)
	if err != nil {
		t.Fatalf("读回的不是合法 PNG：%v", err)
	}
	if b := back.Bounds(); b.Dx() != 7 || b.Dy() != 5 {
		t.Fatalf("读回尺寸不符：%v", b)
	}
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			if a, b := color.NRGBAModel.Convert(back.At(x, y)).(color.NRGBA), src.NRGBAAt(x, y); a != b {
				t.Errorf("像素(%d,%d) 往返不一致：%+v vs %+v", x, y, a, b)
			}
		}
	}
	// 字节级稳定：同内容读两次必须一致（回声抑制比的是哈希）
	again, err := ic.GetImage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Error("同一张图两次读取字节不同，回声抑制会失效")
	}
}

// 传入不是合法 PNG 时 SetImage 必须报错，且**不破坏**剪贴板现有内容。
func TestSetImageRejectsGarbage(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	cb := newClipboard()
	ic, ok := cb.(imageClipboard)
	if !ok {
		t.Fatal("Windows 剪贴板应实现 imageClipboard")
	}

	original, err := cb.GetText()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cb.SetText(original) }()

	if err := cb.SetText("先放一段文本"); err != nil {
		t.Fatal(err)
	}
	if err := ic.SetImage([]byte("这不是 PNG")); err == nil {
		t.Error("非法 PNG 应当报错")
	}
	if got, err := cb.GetText(); err != nil || got != "先放一段文本" {
		t.Errorf("写入失败不该破坏剪贴板，实际 %q（err=%v）", got, err)
	}
}

// 空的 / 超短的输入不能让实现 panic。
func TestSetImageRejectsEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	ic, ok := newClipboard().(imageClipboard)
	if !ok {
		t.Fatal("Windows 剪贴板应实现 imageClipboard")
	}
	for _, bad := range [][]byte{nil, {}, {0x89}} {
		if err := ic.SetImage(bad); err == nil {
			t.Errorf("空/超短输入 %v 应当报错", bad)
		}
	}
}

// GetImage 必须在**释放剪贴板之后**才做解码与 PNG 编码。
//
// 这条约定来自一个真实故障：开着本程序时，截图工具「确认」后会卡一下，
// 而且刚截的图进不了 Ditto 的历史。根因就是取图全程占着剪贴板——
// 截图工具卡在 OpenClipboard 重试上（重试耗尽就放弃，图根本没写进剪贴板），
// Ditto 收到 WM_CLIPBOARDUPDATE 后同样打不开剪贴板，只能跳过这一条。
//
// 判定用 clipboardStats 的占用层数：Windows 的 OpenClipboard 同进程**可重入**
// （自己占着时再开一次照样成功），所以从进程内部问不出「剪贴板是不是被自己占着」，
// 只能靠实现里的计数器。
func TestGetImageReleasesClipboardBeforeConverting(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	cb := newClipboard()
	ic, ok := cb.(imageClipboard)
	if !ok {
		t.Fatal("Windows 剪贴板应实现 imageClipboard")
	}

	original, err := cb.GetText()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cb.SetText(original) }()

	// 造一张图写进剪贴板（SetImage 会同时放 CF_DIB 与 "PNG" 两种格式）
	src := image.NewNRGBA(image.Rect(0, 0, 9, 7))
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 20), G: uint8(y * 30), B: 200, A: 255})
		}
	}
	png, err := encodePNG(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := ic.SetImage(png); err != nil {
		t.Fatal(err)
	}

	orig := clipboardImageToPNG
	defer func() { clipboardImageToPNG = orig }()

	calls, depthAtConvert := 0, 0
	clipboardImageToPNG = func(raw []byte, fromPNG bool) []byte {
		calls++
		if d, _ := clipboardStats(); d != 0 {
			depthAtConvert = d
		}
		return orig(raw, fromPNG)
	}

	got, err := ic.GetImage()
	if err != nil {
		t.Fatalf("读取剪贴板图片失败：%v", err)
	}
	if calls == 0 {
		t.Fatal("没有走到转换步骤，这个测试什么也没验证到")
	}
	if depthAtConvert != 0 {
		t.Errorf("解码/编码时剪贴板占用层数为 %d，应当已经释放："+
			"否则截图工具会卡在 OpenClipboard 重试上、Ditto 也读不到刚截的图", depthAtConvert)
	}
	if len(got) == 0 {
		t.Error("读回的图片为空")
	}
	if d, _ := clipboardStats(); d != 0 {
		t.Errorf("GetImage 返回后剪贴板仍被占着（层数 %d）", d)
	}
}

// 纯文本剪贴板上取图不该锁剪贴板：三种格式的可用性查询都是无锁的。
//
// 这条路径每次复制文本都会走（checkLocal 先问文本、再问图片），
// 少锁一次就少一次和截图工具 / Ditto 抢剪贴板的机会。
func TestGetImageOnTextClipboardNeverLocks(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实剪贴板测试")
	}
	cb := newClipboard()
	ic, ok := cb.(imageClipboard)
	if !ok {
		t.Fatal("Windows 剪贴板应实现 imageClipboard")
	}

	original, err := cb.GetText()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cb.SetText(original) }()

	if err := cb.SetText("只有文本，没有位图"); err != nil {
		t.Fatal(err)
	}

	_, opensBefore := clipboardStats()
	got, err := ic.GetImage()
	_, opensAfter := clipboardStats()

	if err != nil {
		t.Errorf("纯文本剪贴板上取图不该失败：%v", err)
	}
	if n := opensAfter - opensBefore; n != 0 {
		t.Errorf("纯文本剪贴板上取图锁了 %d 次剪贴板，应当一次都不锁", n)
	}
	if len(got) != 0 {
		t.Errorf("纯文本剪贴板不该读出图片，实际 %d 字节", len(got))
	}
}
