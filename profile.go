package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
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

// hashText 返回 text 的 SHA-256 十六进制摘要。
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
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
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Text != nil {
		out := Profile{Type: "text", Text: *probe.Text}
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

// rawTextHeadTail 是「远端原始内容」里 text 字段保留的首尾字符数。
const rawTextHeadTail = 5

// remoteRawText 把远端 Profile 渲染成固定字段顺序的清单，供界面「远端原始内容」展示。
//
// 刻意**不**直接用 json.MarshalIndent 输出原文：
//   - 字段顺序要固定（跟对端实现无关），否则同一个东西每次看起来都不一样；
//   - text 必须截断——完整内容上面「远端内容」那块已经展示过了，这里再铺一遍
//     既重复又把卡片撑长；只留首尾各 5 个字符反而更好用：
//     头尾最容易暴露「被截断」「编码不对」「多了引号」这类问题。
func remoteRawText(p Profile) string {
	var b strings.Builder
	line := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	line("type", p.Type)
	line("size", strconv.Itoa(p.Size))
	line("hash", p.Hash)
	line("has_data", strconv.FormatBool(p.HasData))
	line("data_name", derefString(p.DataName))
	line("source", derefString(p.Source))
	line("text", headTail(p.Text, rawTextHeadTail))
	return strings.TrimRight(b.String(), "\n")
}

// derefString 解引用可空字符串字段，nil 一律当空串。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// headTail 只保留首尾各 n 个字符，中间用省略号连起来；不足 2n 个字符时原样返回。
//
// 按字符（rune）切，不按字节——否则中文和 emoji 会被劈成乱码。
func headTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= 2*n {
		return s
	}
	return string(r[:n]) + "…" + string(r[len(r)-n:])
}
