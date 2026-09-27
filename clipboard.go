package main

// Clipboard 是跨平台的剪贴板「纯文本」读写接口。
type Clipboard interface {
	// GetText 读取当前剪贴板文本；剪贴板中没有文本时返回空串。
	GetText() (string, error)
	// SetText 写入剪贴板文本。
	SetText(text string) error
}

// seqClipboard 是可选能力：提供廉价的变更序号，
// 让同步循环不必每次都去锁定剪贴板读取内容。
type seqClipboard interface {
	Seq() (uint32, bool)
}
