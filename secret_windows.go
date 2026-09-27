//go:build windows

package main

import (
	"encoding/base64"
	"fmt"
	"syscall"
	"unsafe"
)

// WebDAV 密码不能明文落盘，这里用 Windows 自带的 DPAPI 加密
// （crypt32.dll 的 CryptProtectData / CryptUnprotectData）。
//
// 选 DPAPI 而不是自己搞一套密钥，理由：
//   - 纯 syscall，不需要 cgo、不需要第三方库，符合「零依赖」的底线
//   - 密钥由 Windows 保管（绑定当前用户账户），程序里没有可被翻出来的主密钥
//   - 用户不需要额外记一个「主密码」——这是个开机自启的托盘小程序，
//     每次启动都要求输密码是行不通的
//
// 代价：密文只能在**同一台机器的同一个 Windows 用户**下解开。
// 换用户 / 换机器后密码解不开，需要重新填一次（程序会在日志里说明）。
// 对于「配置和程序放一起、拷到 U 盘也能用」的场景，这个代价是可接受的；
// 真要跨机器，用户重新输一次密码即可。

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

// CRYPTPROTECT_UI_FORBIDDEN：绝不允许弹 UI。
// 托盘程序可能在无人值守时启动，弹窗会把启动流程卡死。
const cryptProtectUIForbidden = 0x1

// dataBlob 对应 Win32 的 DATA_BLOB。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

// protectSecret 把明文加密成可安全写盘的 base64 字符串。空串返回空串。
func protectSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	in := []byte(plain)
	blob := dataBlob{cbData: uint32(len(in)), pbData: &in[0]}

	var out dataBlob
	r, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&blob)),
		0, // szDataDescr（不需要描述）
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", fmt.Errorf("CryptProtectData 失败：%w", callErr)
	}
	// DPAPI 用 LocalAlloc 分配输出，必须 LocalFree，不能交给 Go 的 GC
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))

	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.pbData, out.cbData)), nil
}

// unprotectSecret 把 base64 密文解回明文。空串返回空串。
func unprotectSecret(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("密码字段不是合法的 base64：%w", err)
	}
	if len(raw) == 0 {
		return "", nil
	}
	blob := dataBlob{cbData: uint32(len(raw)), pbData: &raw[0]}

	var out dataBlob
	r, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&blob)),
		0, // ppszDataDescr（不需要）
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", fmt.Errorf("密码解密失败（多半是换了 Windows 用户或换了机器）：%w", callErr)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))

	return string(unsafe.Slice(out.pbData, out.cbData)), nil
}
