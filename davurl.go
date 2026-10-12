package main

import (
	"regexp"
	"strings"
)

// 远端文件 URL 组装。
//
//	远端文件：{davUrl}/{remotePath}
//	  davUrl     = 服务器根（如 https://dav.jianguoyun.com/dav/）
//	  remotePath = 远端路径，**相对服务器根、具体到文件**（如 rime/clip/1.json）
//	远端附件：{远端文件所在目录}/blobs/{dataName}
//	  图片原图放在剪贴板文件旁边的 blobs 目录里（与插件 libs/dav-url.ts 一致）
//
// 这里只做纯字符串拼接：remotePath 留空就直接指向服务器根，不做任何默认值处理。
// 「留空时用默认文件 xime/clipboard/current.json」是配置层的策略，
// 由 Config.RemotePathOrDefault() 负责，别混进这个函数。

// ClipboardFile 是协议约定的剪贴板文件名（相对默认目录）。
//
// 只有「远程路径」留空、走默认布局时才用它拼出 DefaultRemotePath；
// 用户填了具体文件路径时，文件名由用户自己决定，这里不参与。
const ClipboardFile = "clipboard/current.json"

// BlobDirName 是附件目录名（相对剪贴板文件所在目录），与插件 libs/dav-url.ts 的 BLOB_DIR 一致。
const BlobDirName = "blobs"

// blobNamePattern 是附件名白名单：内容寻址名（<sha256>.<ext>）只含 [A-Za-z0-9._-]。
//
// dataName 来自远端 JSON（data_name 字段）或本地生成，但**绝不能**直接拼进路径：
// 万一对端（或被篡改的远端文件）给出 "../../evil" 这样的值，就会写到剪贴板目录之外。
// 白名单顺带挡掉空名、路径分隔符与前导点。与插件 BLOB_NAME_PATTERN 同一套规则。
var blobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

// blobsDirURL 返回远端附件**目录**的 URL：{剪贴板文件所在目录}/blobs。
//
// 与 buildBlobURL 的区别是「要不要具体到某个附件文件」：
//   - buildBlobURL 需要一个合法的 dataName，用于真正读写某个附件；
//   - blobsDirURL 只要目录本身，用于配置阶段「先把目录建好」——那时还没有任何附件。
func blobsDirURL(fileURL string) (string, bool) {
	dir := buildDirURL(fileURL)
	if dir == "" {
		return "", false
	}
	return dir + "/" + BlobDirName, true
}

// buildBlobURL 返回远端附件 URL：{剪贴板文件所在目录}/blobs/{dataName}。
//
// 附件名先过白名单（见 blobNamePattern），非法名一律返回 ok=false，
// **绝不**把远端给的名字直接拼进路径——那是路径穿越的入口。
func buildBlobURL(fileURL, dataName string) (string, bool) {
	if !blobNamePattern.MatchString(dataName) {
		return "", false
	}
	dir, ok := blobsDirURL(fileURL)
	if !ok {
		return "", false
	}
	return dir + "/" + dataName, true
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

// relativePathParts 把 url 的路径拆成相对 davURL 的层级。
//
// url 按**目录**理解（不剥最后一段）；要处理「文件 URL 的所在目录」用 relativeDirParts。
// 从用户配置的 davURL（通常已存在）之后开始创建，避免 MKCOL 服务器根被拒。
// 纯字符串前缀匹配，避免正则中 . - + 等特殊字符或前缀误匹配。
func relativePathParts(baseURL, url string) []string {
	basePath := stripTrailingSlashes(stripHost(baseURL))
	dirPart := stripHost(url)
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

// relativeDirParts 解析出 fileURL **所在目录**相对 davURL 的层级（不含文件名），供逐级 MKCOL 使用。
func relativeDirParts(baseURL, fileURL string) []string {
	return relativePathParts(baseURL, buildDirURL(fileURL))
}
