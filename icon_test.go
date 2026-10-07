package main

import "testing"

// pickICO 的选择顺序：正好相等 > 比目标大的里面最小的 > 最大的那一档。
//
// 这条直接决定托盘图标糊不糊：宁可拿大图缩小（丢像素），也不要拿小图放大
// （凭空插值、边缘发虚）。系统要 24 时，手里有 16 和 32 就该给 32。
func TestPickICOPreference(t *testing.T) {
	mk := func(sizes ...int) []icoEntry {
		out := make([]icoEntry, 0, len(sizes))
		for _, s := range sizes {
			out = append(out, icoEntry{Width: s, Height: s})
		}
		return out
	}

	cases := []struct {
		name   string
		sizes  []int
		want   int
		expect int
	}{
		{"正好命中", []int{16, 24, 32}, 24, 24},
		{"没有正好，取更大的里面最小的", []int{16, 32, 48}, 24, 32},
		{"没有更大的，取最大的", []int{16, 20}, 24, 20},
		{"目标是最大档", []int{16, 32, 256}, 256, 256},
		{"单档也能用", []int{32}, 16, 32},
		{"乱序输入不影响结果", []int{256, 16, 48, 32, 24}, 24, 24},
		{"真实 logo 的档位：要 24 拿 24", []int{256, 128, 96, 64, 48, 32, 24, 16}, 24, 24},
		{"真实 logo 的档位：要 20 拿 24", []int{256, 128, 96, 64, 48, 32, 24, 16}, 20, 24},
	}
	for _, c := range cases {
		got, ok := pickICO(mk(c.sizes...), c.want)
		if !ok {
			t.Errorf("%s：应能选出结果", c.name)
			continue
		}
		if got.Width != c.expect {
			t.Errorf("%s：要 %d 时选中 %d，期望 %d（档位 %v）",
				c.name, c.want, got.Width, c.expect, c.sizes)
		}
	}

	if _, ok := pickICO(nil, 16); ok {
		t.Error("空列表应返回 false")
	}
}

// 内嵌的 logo.ico 必须能覆盖各缩放比下系统要的小图标尺寸——
// 100% 要 16、125% 要 20、150% 要 24、200% 要 32，缺了就只能缩放，缩完就糊。
func TestLogoICOHasSizesNeededByDPI(t *testing.T) {
	entries, err := parseICO(logoICO)
	if err != nil {
		t.Fatalf("解析内嵌 logo.ico 失败：%v", err)
	}

	var sizes []int
	for _, e := range entries {
		sizes = append(sizes, e.Width)
	}

	// 100% / 125% / 150% / 175% / 200% 缩放下系统要的小图标边长
	for _, need := range []int{16, 20, 24, 32} {
		got, ok := pickICO(entries, need)
		if !ok {
			t.Fatalf("选不出 %d 档", need)
		}
		// 允许「没有正好、取更大的」；但不允许拿更小的去放大
		if got.Width < need {
			t.Errorf("系统要 %d 时选中了更小的 %d（会被放大而发虚）；可用档位 %v",
				need, got.Width, sizes)
		}
	}
}
