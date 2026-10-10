# Xime Clip Sync

Xime Clip Sync 是一个基于 WebDAV 的 Windows 剪贴板同步工具。它把本机剪贴板文本同步到远端 JSON 文件，
并定期拉取远端内容更新本机剪贴板。配合 [Xime 曦码 输入法](https://github.com/ximeiorg/Xime) 的
`webdav-clipboard-sync` 插件使用，两端指向同一个远端文件，可以在 Windows 电脑和手机之间同步纯文本。

## 工作方式

- 运行后最小化到托盘常驻
- 运行期间本地变化按「本地检查间隔」推送到远端，远端变化按「远端轮询间隔」拉取。
- 用文本内容和 WebDAV `ETag` 避免重复同步与循环更新。
- 托盘图标被系统丢弃时（重启资源管理器、睡眠或休眠唤醒后重建任务栏）会**自动补回来**，不用重启程序。

## 结构

```
build/
├── xime-clip-sync.exe
├── config.json              ← 配置（密码是 DPAPI 密文）
├── xime-clip-sync.ui        ← 当前配置界面地址
└── logs/
    └── xime-clip-sync-2026-09-27.log
```

## 配置

首次运行会自动生成 `config.json`，推荐直接在界面里改（双击托盘图标）。

```json
{
  "davUrl": "https://dav.jianguoyun.com/dav/",
  "remotePath": "xime/clipboard/current.json",
  "username": "you@example.com",
  "passwordEnc": "<DPAPI 密文>",
  "deviceName": "my-pc",
  "enabled": true,
  "pollSeconds": 30,
  "localPollSeconds": 5,
  "maxTextChars": 300,
  "manualMaxTextChars": 10000,
  "insecureSkipVerify": false,
  "proxyMode": "system",
  "proxyUrl": "",
  "autostart": false,
  "logToFile": true,
  "logRetainDays": 7,
  "logMaxFileMB": 2,
  "uiPort": 31213
}
```

> 密码不以明文写入：界面填的密码经 Windows DPAPI 加密后存进 `passwordEnc`，换台电脑或换个 Windows
> 账号都解不开。`config.json` 含密文，不要提交到 Git 或分享给他人。

### 代理

`proxyMode` 有四档，界面上在「程序设置 → 网络代理」里：

| `proxyMode` | 含义 |
| --- | --- |
| `system` | 跟随 Windows 系统代理（「Internet 选项 → 连接 → 局域网设置」），**默认** |
| `http` | 用 `proxyUrl` 指定的 HTTP 代理 |
| `socks5` | 用 `proxyUrl` 指定的 SOCKS5 代理 |
| `none` | 直连，不走任何代理 |

`proxyUrl` 只填 `host:port`（如 `127.0.0.1:7890`），不用写 `http://`。只在 `http` / `socks5` 两档下生效。

系统代理里的「跳过代理」列表（`ProxyOverride`）会照常生效，所以 `127.*`、`192.168.*` 这些局域网地址不会被塞进代理。
系统代理取的是注册表里的设置，**不读 `HTTP_PROXY` 环境变量**——从资源管理器双击启动的程序拿不到那些变量。

### 与手机端插件对齐

手机端插件把「远程路径」当**目录**（文件名由插件固定追加），本程序把它当**完整文件路径**。
要指向同一个文件，填「插件里那个远程目录 + `/clipboard/current.json`」：

> 插件里填 `Android/xime` → 这里填 `Android/xime/clipboard/current.json`

两边都留空会指向**不同**的文件，那样就同步不上了。

## 命令行

```
xime-clip-sync.exe [options]
```

| 参数 | 说明 |
| --- | --- |
| `-config <path>` | 指定配置文件，默认 exe 同目录的 `config.json` |
| `-open-ui` | 启动时自动打开配置界面 |
| `-no-ui` | 不启动配置界面 |
| `-no-tray` | 不显示托盘图标 |
| `-test` | 测试 WebDAV 连接后退出 |
| `-once push\|pull` | 执行一次同步后退出，适合放进计划任务 |
| `-version` | 显示版本号 |

程序是**无控制台**构建，标准输出不会出现在任何地方，这些参数只通过**退出码**（0 成功 / 1 失败）表达结果。
日常用不上，双击 exe 就行。

程序没有控制台，**`logs/` 是唯一的排查线索**。

## 构建

需要 Go 1.23+，产物输出到 `build/`：

```bash
./build.sh                  # Windows 下用 build.bat
GO=/path/to/go ./build.sh   # go 不在 PATH 里时这样指定
```

**换图标只需替换 `img/logo.ico` 后重新构建**，不用改代码。

不想自己编译的话，去 [Releases](https://github.com/MajoSissi/xime-clip-sync/releases) 下现成的
`xime-clip-sync.exe`（附带 `SHA256SUMS.txt`）。构建是**手动触发**的：仓库 Actions 页面 →
build → Run workflow，见 [DEVELOPMENT.md](DEVELOPMENT.md#ci-与发版github-actions)。
构建细节、协议、实现原理与测试见 [DEVELOPMENT.md](DEVELOPMENT.md)。

## 已知限制

- 只支持 **Windows / 64 位**。
- 只同步**文本**，不同步图片/文件（与插件能力一致）。
- 不做增量或历史记录，远端只保留最新一条。
- 没有剪贴板历史与快捷键面板，只做「当前剪贴板」的双向同步。
- 靠轮询工作，不是即时推送：本地变化最坏要等一个「本地检查间隔」（默认 5 秒）才推送，
  远端变化取决于「远端轮询间隔」。
