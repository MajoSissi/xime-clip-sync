//go:build windows

package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// 开机自启：写入 HKCU\Software\Microsoft\Windows\CurrentVersion\Run。
// 只改当前用户、不需要管理员权限，也不污染系统级配置。

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

const (
	hkeyCurrentUser   = 0x80000001
	keyQueryValue     = 0x0001
	keySetValue       = 0x0002
	regSZ             = 1
	errorFileNotFound = 2

	autostartSubKey = `Software\Microsoft\Windows\CurrentVersion\Run`
)

// openAutostartKey 打开（必要时创建）Run 键。
func openAutostartKey() (uintptr, error) {
	subKey := utf16Ptr(autostartSubKey)
	if subKey == nil {
		return 0, fmt.Errorf("构造注册表路径失败")
	}
	var hKey uintptr
	r, _, _ := procRegCreateKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0, 0, 0,
		keyQueryValue|keySetValue,
		0,
		uintptr(unsafe.Pointer(&hKey)),
		0,
	)
	if r != 0 {
		return 0, fmt.Errorf("打开注册表 Run 键失败（错误码 %d）", r)
	}
	return hKey, nil
}

// setAutostart 开启或关闭开机自启。
func setAutostart(enabled bool) error {
	if !enabled {
		return removeAutostartValue(autostartName)
	}
	hKey, err := openAutostartKey()
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(hKey)

	name := utf16Ptr(autostartName)
	if name == nil {
		return fmt.Errorf("构造注册表项名失败")
	}

	exe, err := currentExePath()
	if err != nil {
		return err
	}
	// 路径带引号，避免路径中含空格时被拆开
	value := `"` + exe + `"`
	data := utf16Ptr(value)
	if data == nil {
		return fmt.Errorf("构造自启命令失败")
	}
	r, _, _ := procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(name)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(data)),
		uintptr(len(value)+1)*2,
	)
	if r != 0 {
		return fmt.Errorf("写入自启项失败（错误码 %d）", r)
	}
	return nil
}

// removeAutostartValue 删除 Run 键下的某个值。
// 值本来就不存在（ERROR_FILE_NOT_FOUND）也算成功。
func removeAutostartValue(name string) error {
	hKey, err := openAutostartKey()
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(hKey)

	p := utf16Ptr(name)
	if p == nil {
		return fmt.Errorf("构造注册表项名失败")
	}
	r, _, _ := procRegDeleteValueW.Call(hKey, uintptr(unsafe.Pointer(p)))
	if r != 0 && r != errorFileNotFound {
		return fmt.Errorf("删除自启项失败（错误码 %d）", r)
	}
	return nil
}

// readAutostartValue 读取 Run 键下某个值的命令串（不存在返回 false）。
func readAutostartValue(name string) (string, bool) {
	subKey := utf16Ptr(autostartSubKey)
	p := utf16Ptr(name)
	if subKey == nil || p == nil {
		return "", false
	}
	var hKey uintptr
	r, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		keyQueryValue,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r != 0 {
		return "", false
	}
	defer procRegCloseKey.Call(hKey)

	buf := make([]uint16, 1024)
	size := uint32(len(buf) * 2)
	r, _, _ = procRegQueryValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(p)),
		0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

// autostartEnabled 检查自启项是否指向当前这个可执行文件。
func autostartEnabled() bool {
	exe, err := currentExePath()
	if err != nil {
		return false
	}
	value, ok := readAutostartValue(autostartName)
	if !ok {
		return false
	}
	// 去掉可能的引号再比较，避免大小写/引号差异造成误判
	return strings.EqualFold(strings.Trim(value, `"`), exe)
}

// repairAutostart 处理「挪动程序」给开机自启留下的烂摊子：
//
// 配置里开着自启、但注册表里没有这一项（或指向旧路径）时，按当前 exe 补写一次。
// 绿色便携程序经常被整个目录拷来拷去，Run 项里存的绝对路径会随之失效。
//
// 幂等，每次启动跑一遍没有副作用。
func repairAutostart(cfg Config, log *LogBuffer) {
	if !cfg.Autostart || autostartEnabled() {
		return
	}
	if err := setAutostart(true); err != nil {
		log.Warnf("开机自启项已失效（程序挪动过），重新写入失败：%v", err)
		return
	}
	log.Infof("开机自启项已更新为当前程序路径")
}
