package main

import "strings"

// 远端文件 URL 组装。
//
//	远端文件：{davUrl}/{remotePath}
//	  davUrl     = 服务器根（如 https://dav.jianguoyun.com/dav/）
//	  remotePath = 远端路径，**相对服务器根、具体到文件**（如 rime/clip/1.json）
//
// 这里只做纯字符串拼接：remotePath 留空就直接指向服务器根，不做任何默认值处理。
// 「留空时用默认文件 xime/clipboard/current.json」是配置层的策略，
// 由 Config.RemotePathOrDefault() 负责，别混进这个函数。

// ClipboardFile 是协议约定的剪贴板文件名（相对默认目录）。
//
// 只有「远程路径」留空、走默认布局时才用它拼出 DefaultRemotePath；
// 用户填了具体文件路径时，文件名由用户自己决定，这里不参与。
const ClipboardFile = "clipboard/current.json"

// stripTrailingSlashes 去掉末尾斜杠（对应 TS 的 /\/+$/）。
func stripTrailingSlashes(value string) string {
	return strings.TrimRight(value, "/")
}

// stripSlashes 去掉首尾斜杠（对应 TS 的 /^\/+/ 与 /\/+$/）。
func stripSlashes(value string) string {
	return strings.Trim(value, "/")
}

// buildFileURL 返回远端文件 URL；davURL 为空时 ok=false（未配置服务器）。
//
// remotePath 为空时返回服务器根本身——这是纯字符串语义，实际调用点都会先经过
// Config.RemotePathOrDefault()，所以不会真的把文件写到根上。
func buildFileURL(davURL, remotePath string) (string, bool) {
	base := stripTrailingSlashes(davURL)
	if base == "" {
		return "", false
	}
	if p := stripSlashes(remotePath); p != "" {
		return base + "/" + p, true
	}
	return base, true
}

// buildDirURL 返回远端文件**所在目录**的 URL（去掉最后一段路径），供逐级 MKCOL 使用。
//
// 注意不是「协议约定的剪贴板目录」——「远程路径」现在由用户填到具体文件，
// 目录是哪一层完全取决于用户填了什么（如 rime/clip/1.json → rime/clip）。
func buildDirURL(fileURL string) string {
	i := strings.LastIndex(fileURL, "/")
	if i < 0 {
		return ""
	}
	return fileURL[:i]
}

// stripHost 去掉 "http(s)://host" 前缀，只保留路径部分。
func stripHost(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	rest := u[i+3:]
	if j := strings.Index(rest, "/"); j >= 0 {
		return rest[j:]
	}
	return ""
}

// relativeDirParts 解析出相对 davURL 的目录层级（不含文件名），供逐级 MKCOL 使用。
// 从用户配置的 davURL（通常已存在）之后开始创建，避免 MKCOL 服务器根被拒。
func relativeDirParts(baseURL, fileURL string) []string {
	basePath := stripTrailingSlashes(stripHost(baseURL))
	dirPart := stripHost(fileURL)
	// 去掉末尾的 "/文件名"
	if i := strings.LastIndex(dirPart, "/"); i >= 0 {
		dirPart = dirPart[:i]
	}
	if basePath != "" && strings.HasPrefix(dirPart, basePath) {
		dirPart = dirPart[len(basePath):]
	}
	parts := strings.Split(dirPart, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
