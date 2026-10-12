package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Profile 是远端 clipboard/current.json 的 wire 结构。
//
// 字段名与 ximed 的 Profile 同构（snake_case），与插件 main.ts 中
// JSON.stringify 出来的字段完全一致，因此可以直接用 encoding/json 编解码。
type Profile struct {
	Type     string  `json:"type"`
	Hash     string  `json:"hash"`
	Text     string  `json:"text"`
	HasData  bool    `json:"has_data"`
	DataName *string `json:"data_name"`
	Size     int     `json:"size"`
	Source   *string `json:"source"`
}

// newTextProfile 构造一个纯文本剪贴板 Profile。
//
// size 与插件一致，取 text 的 UTF-8 字节长度
// （对应 TS 的 new TextEncoder().encode(text).length）。
//
// hash 固定按本地 SHA-256 计算——这是插件端认的算法，且同步判重用的是
// **文本内容**而不是 hash，所以没有做成可配置项，界面上也不出现。
func newTextProfile(text, source string) Profile {
	p := Profile{
		Type:    "text",
		Hash:    hashText(text),
		Text:    text,
		HasData: false,
		Size:    len(text),
	}
	if source != "" {
		s := source
		p.Source = &s
	}
	return p
}

// ImageExt 是同步图片的固定扩展名：一律转成 PNG。
//
// PNG 无损、跨端通用，插件端的 EXT_CONTENT_TYPE 也认它。
// 统一扩展名还让 blob 名（<sha256>.png）成为稳定的内容地址。
const ImageExt = "png"

// newImageProfile 构造一个图片剪贴板 Profile。
//
// data 是 PNG 字节。data_name 用**内容寻址**（<sha256>.png），与插件端的
// blob 命名约定一致（插件用 BLOB_NAME_PATTERN 校验、并按扩展名给 Content-Type）：
// 同内容得到同名 ⇒ 重复上传幂等，也能直接拿名字当缓存键。
//
// size 取 PNG 字节长度，也就是真正写进远端的那个体积（不是剪贴板里未压缩的位图大小）。
// text 保持空串——图片没有文本；插件端靠 has_data 判定这是图片记录。
func newImageProfile(pngData []byte, source string) Profile {
	h := hashBytes(pngData)
	name := h + "." + ImageExt
	p := Profile{
		Type:     "image",
		Hash:     h,
		HasData:  true,
		DataName: &name,
		Size:     len(pngData),
	}
	if source != "" {
		s := source
		p.Source = &s
	}
	return p
}

// hashText 返回 text 的 SHA-256 十六进制摘要。
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// hashBytes 返回字节串的 SHA-256 十六进制摘要（图片附件用）。
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// encode 序列化为写入 WebDAV 的 JSON body。
func (p Profile) encode() ([]byte, error) {
	return json.Marshal(p)
}

// decodeProfile 解析远端文件内容。
//
// 返回的 ok=false 表示内容不可用（空文件）。
// plain=true 表示远端是旧版纯文本文件（非 JSON），已按纯文本兼容处理
// —— 与插件 pull() 的兼容分支一致。
//
// 判定「这是不是一个 Profile」看的是**有没有 Profile 的特征字段**，
// 而不是「有没有 text」：图片记录只有 has_data / data_name、没有 text 键
// （插件 JSON.stringify 会把值为 undefined 的 text 整个省掉）。
// 早先只认 text 键的写法会把图片 JSON 当成纯文本，把整段 JSON 当剪贴板文本写回本地。
func decodeProfile(raw []byte) (p Profile, plain bool, ok bool) {
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return Profile{}, false, false
	}
	// 先按 JSON Profile 解析
	var probe struct {
		Type     *string `json:"type"`
		Hash     *string `json:"hash"`
		Text     *string `json:"text"`
		HasData  *bool   `json:"has_data"`
		DataName *string `json:"data_name"`
		Size     *int    `json:"size"`
		Source   *string `json:"source"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil &&
		(probe.Text != nil || probe.HasData != nil || probe.Type != nil) {
		out := Profile{Type: "text", Text: ""}
		if probe.Text != nil {
			out.Text = *probe.Text
		}
		if probe.Type != nil {
			out.Type = *probe.Type
		}
		if probe.Hash != nil {
			out.Hash = *probe.Hash
		}
		if probe.HasData != nil {
			out.HasData = *probe.HasData
		}
		if probe.DataName != nil {
			out.DataName = probe.DataName
		}
		if probe.Size != nil {
			out.Size = *probe.Size
		} else {
			out.Size = len(out.Text)
		}
		if probe.Source != nil {
			out.Source = probe.Source
		}
		return out, false, true
	}
	// 兼容旧版纯文本文件
	return Profile{
		Type:    "text",
		Text:    text,
		HasData: false,
		Size:    len(text),
	}, true, true
}

// derefString 解引用可空字符串字段，nil 一律当空串。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
