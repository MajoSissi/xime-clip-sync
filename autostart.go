package main

import (
	"fmt"
	"os"
)

// 自启项名称（Windows 注册表项名 / macOS LaunchAgent Label / Linux .desktop 文件名）
const autostartName = "Xime Clip Sync"

// currentExePath 返回当前可执行文件的绝对路径。
func currentExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取程序路径失败：%w", err)
	}
	return exe, nil
}
