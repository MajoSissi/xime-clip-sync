//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// errorSharingViolation 对应 Win32 的 ERROR_SHARING_VIOLATION(32)：
// 文件已被其他进程以独占方式打开。
const errorSharingViolation syscall.Errno = 32

// acquireSingleInstance 通过独占打开锁文件实现单实例。
// 第二个实例会拿到 acquired=false，进程退出或崩溃时句柄由系统自动关闭，锁随之释放。
func acquireSingleInstance(path string) (release func(), acquired bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // 共享模式为 0：独占
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.CloseHandle(h)
		os.Remove(path)
	}, true, nil
}
