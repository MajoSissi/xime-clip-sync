package main

import (
	"path/filepath"
	"testing"
)

// TestSingleInstanceLock 验证单实例锁：同一时刻只有一个持有者，
// 释放后可以重新获取。Windows 走独占文件句柄，Unix 走 flock。
func TestSingleInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xime-clip-sync.lock")

	release1, ok1, err := acquireSingleInstance(path)
	if err != nil {
		t.Fatalf("首次获取锁出错：%v", err)
	}
	if !ok1 {
		t.Fatal("首次获取锁应成功")
	}

	// 第二个实例必须拿不到锁，否则会出现两条同步循环互相覆盖剪贴板
	_, ok2, err2 := acquireSingleInstance(path)
	if err2 != nil {
		t.Fatalf("第二次获取锁不应报错，实际：%v", err2)
	}
	if ok2 {
		t.Error("第二次获取锁应失败（已有实例在运行）")
	}

	// 释放后应能重新获取
	release1()
	release2, ok3, err3 := acquireSingleInstance(path)
	if err3 != nil {
		t.Fatalf("释放后重新获取锁出错：%v", err3)
	}
	if !ok3 {
		t.Error("释放后应能重新获取锁")
	}
	release2()
}
