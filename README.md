# Xime Clip Sync

**让 Windows 电脑和手机的剪贴板文本双向同步**—— WebDAV 服务器（如坚果云）。

手机端复制一段文字，电脑上直接粘贴；电脑上复制，手机端也能拿到。
配合 [Xime 输入法](https://github.com/ximeiorg/Xime) 的 `webdav-clipboard-sync` 插件使用，

- 常驻系统托盘，**双击直接进托盘，不弹任何窗口**
- 只同步**文本**（与插件能力一致），不同步图片和文件
- 只支持 **Windows / 64 位**

---

## 获取与运行

需要 Go 1.23+，产物输出到 `build/`：

```bash
./build.sh                  # Windows 下用 build.bat
GO=/path/to/go ./build.sh   # go 不在 PATH 里时这样指定
```

```
build/
├── xime-clip-sync.exe
├── config.json              ← 配置（密码是 DPAPI 密文）
├── xime-clip-sync.ui        ← 当前配置界面地址
└── logs/
    └── xime-clip-sync-2026-09-27.log
```

exe 目录不可写时，程序在日志里告警，**不会**把配置挪到 `%AppData%`。

> 开发相关（构建细节、协议、实现原理、测试）见 [DEVELOPMENT.md](DEVELOPMENT.md)。

---

## 首次配置

**双击**托盘图标打开配置界面，填好「同步设置」页的连接信息，点右上角「保存并应用」。
「测试连接」按钮可以就地验证能不能连上。

| 配置项 | 说明 | 示例 |
|--------|------|------|
| WebDAV 服务器地址 | 服务器根地址，含协议与端口 | `https://dav.jianguoyun.com/dav/` |
| 远程路径 | 剪贴板文件路径，**要具体到文件**；留空即 `xime/clipboard/current.json` | `rime/clip/current.json` |
| 用户名 / 密码 | WebDAV 账号（坚果云用「应用密码」） | — |
| 本机设备名 | 写入远端 `source` 字段，便于对端区分来源 | `my-pc` |
| 跳过 TLS 证书校验 | 自签证书的局域网服务器勾选 | 否 |

### 要和手机端插件指向同一个文件

手机端插件把「远程路径」当**目录**，文件名由插件固定追加；本程序把它当**完整文件路径**。
所以填「插件里那个远程目录 + `/clipboard/current.json`」：

> 插件里填 `Android/xime` → 这里就填 `Android/xime/clipboard/current.json`

两边都留空会指向**不同**的文件，那样就同步不上了。

---
