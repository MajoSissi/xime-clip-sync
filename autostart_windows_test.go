//go:build windows

package main

import "testing"

// TestAutostartRegistry 验证开机自启项的写入、读取与删除。
//
// 该测试会临时写入 HKCU\Software\Microsoft\Windows\CurrentVersion\Run，
// 结束时一定删除；若检测到已有自启项则跳过，以免影响现网配置。
// 用 -short 可跳过。
func TestAutostartRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过注册表测试")
	}
	if autostartEnabled() {
		t.Skip("已存在 Xime Clip Sync 自启项，跳过以免影响现有配置")
	}
	// 无论成功与否都要清理干净
	defer func() {
		if err := setAutostart(false); err != nil {
			t.Errorf("清理自启项失败：%v", err)
		}
	}()

	if err := setAutostart(true); err != nil {
		t.Fatalf("开启自启失败：%v", err)
	}
	if !autostartEnabled() {
		t.Error("开启后 autostartEnabled 应为 true")
	}

	if err := setAutostart(false); err != nil {
		t.Fatalf("关闭自启失败：%v", err)
	}
	if autostartEnabled() {
		t.Error("关闭后 autostartEnabled 应为 false")
	}

	// 重复关闭应幂等（值不存在时也要算成功）
	if err := setAutostart(false); err != nil {
		t.Errorf("重复关闭应幂等，实际报错：%v", err)
	}
}
