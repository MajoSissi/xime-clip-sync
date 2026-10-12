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

// imageClipboard 是可选能力：读写剪贴板里的位图。
//
// 接口上统一用 **PNG** 作为交换格式（无损、跨端通用），平台差异都收在实现里：
// Windows 侧读的时候要把 CF_DIB / CF_DIBV5 解成像素再编码成 PNG，
// 写的时候反过来把 PNG 解成像素再铺成 32 位 DIB。
//
// 做成可选接口而不是加进 Clipboard：文本同步是核心能力，任何平台都得有；
// 图片是增强能力，没有就自动退化成只同步文本（同步引擎按类型断言探测）。
type imageClipboard interface {
	// HasImage 报告剪贴板里是否有位图。这个判断应当**廉价**（只查格式是否存在，
	// 不读数据、不做解码），因为每个本地检查周期都会先问一次。
	HasImage() bool
	// GetImage 读取剪贴板位图并编码为 PNG；剪贴板里没有可读的位图时返回 (nil, nil)。
	GetImage() ([]byte, error)
	// SetImage 把 PNG 字节写入剪贴板。传入的不是合法 PNG 时返回错误，
	// 并且**不改变**剪贴板现有内容。
	SetImage(png []byte) error
}
