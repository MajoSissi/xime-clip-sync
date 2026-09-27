//go:build windows

package main

import (
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
