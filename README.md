# Xime Clip Sync

Xime Clip Sync 是一个基于 WebDAV 的 Windows 剪贴板同步工具。它把本机剪贴板的**文本与图片**
同步到远端 JSON 文件（图片另存到旁边的 `blobs/` 目录），并定期拉取远端内容更新本机剪贴板。
配合 [Xime 曦码 输入法](https://github.com/ximeiorg/Xime) 的
`webdav-clipboard-sync` 插件使用，两端指向同一个远端文件，可以在 Windows 电脑和手机之间同步剪贴板。

## 工作方式

- 运行后最小化到托盘常驻
- 运行期间本地变化按「本地检查间隔」推送到远端，远端变化按「远端轮询间隔」拉取。
- 用文本内容 / 图片哈希和 WebDAV `ETag` 避免重复同步与循环更新。
- 托盘图标被系统丢弃时（重启资源管理器、睡眠或休眠唤醒后重建任务栏）会**自动补回来**，不用重启程序。

## 同步什么

| 内容 | 说明 |
| --- | --- |
| 文本 | 默认同步，受「文本长度上限」约束（自动 / 手动各一个上限） |
| 图片 | 默认**开启**，位图统一转成 PNG 原图存到远端 `blobs/` 目录，受「单张图片上限」约束（自动 / 手动各一个上限，单位 MB，最高 50） |

- 复制的内容**同时含文本和图片**时（Excel / Word 复制单元格、网页复制富文本）按**文本**同步——
  用户复制的是一段内容，把它当图片推过去只会得到一张看不清的截图。
- 图片上限按**推送出去的 PNG 体积**算，而不是剪贴板里未压缩的位图大小。
- 关掉「同步图片」后，图片既不上传也不下载（复制图片不会把远端记录冲成空文本）。
- 老版本（v1.4.0）配置里的 `maxImageKB` / `manualMaxImageKB` 会自动换算成 MB，不用手改。

## 结构

```
build/
├── xime-clip-sync.exe
├── config.json              ← 配置（密码是 DPAPI 密文）
├── xime-clip-sync.ui        ← 当前配置界面地址
└── logs/
    ├── xime-clip-sync-2026-09-27.sync.log   ← 同步日志（剪贴板的推与拉）
    └── xime-clip-sync-2026-09-27.app.log    ← 程序日志（配置、界面、托盘、自启）
```

远端（WebDAV 服务器上）的布局：

```
{davUrl}/{远程路径}               ← 剪贴板 JSON（文本 / 图片元数据），默认 xime/clipboard/current.json
{davUrl}/{所在目录}/blobs/<sha256>.png   ← 图片原图，内容寻址（同图同名，重复上传幂等）
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
  "syncImages": true,
  "maxImageMB": 1,
  "manualMaxImageMB": 10,
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

### 首次配置：填完点一次「测试连接」

「测试连接」除了验证账号能不能访问，还会**在远端把所需目录建好**：

```
{服务器地址}/{远程路径所在目录}           ← 剪贴板文件的家
{服务器地址}/{远程路径所在目录}/blobs     ← 图片附件的家
```

已存在的目录会跳过，所以反复点没有副作用，也不会写任何文件。

建议配好连接就先点一次。理由：建目录本来是「PUT 失败后才补建」的被动逻辑，得先猜中
服务器用哪个状态码表示「父目录不存在」——409 是标准，但有的服务器（部分坚果云、
反向代理后的 WebDAV）回的是 **403**，猜漏就会卡在「上传附件失败（HTTP 403）」。
配置阶段建一次，之后同步就不用再碰这件事了。

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

图片附件放在剪贴板文件旁边的 `blobs/` 目录里，两端布局一致，不用单独配置。

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
| `-test` | 测试 WebDAV 连接（并建好远端目录）后退出 |
| `-once push\|pull` | 执行一次同步后退出，适合放进计划任务 |
| `-version` | 显示版本号 |

程序是**无控制台**构建，标准输出不会出现在任何地方，这些参数只通过**退出码**（0 成功 / 1 失败）表达结果。
日常用不上，双击 exe 就行。

程序没有控制台，**`logs/` 是唯一的排查线索**。日志分两条通道，各自单独成文件：

| 通道 | 记什么 | 文件 |
| --- | --- | --- |
| 同步日志 | 剪贴板的推送与拉取、连接测试、限流退避、上传下载失败 | `xime-clip-sync-<日期>.sync.log` |
| 程序日志 | 配置保存、界面端口、托盘图标、开机自启 | `xime-clip-sync-<日期>.app.log` |

两条通道在内存与磁盘上都互不挤占：程序日志刷屏不会把同步日志顶掉，反之亦然。
配置界面的「运行状态」页只展示**同步日志**——那一页是用来查同步问题的；
程序日志直接看 `logs/` 下的 `.app.log`。

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
- 只同步**剪贴板的文本与位图（图片）**，不同步文件（复制文件只同步其文件名/路径文本，视来源而定）。
- 图片统一转成 **PNG** 传输（无损），透明通道保留；超过上限的图片不推送。
- 不做增量或历史记录，远端只保留最新一条（图片 blob 按内容寻址，旧图不会自动删除）。
- 没有剪贴板历史与快捷键面板，只做「当前剪贴板」的双向同步。
- 靠轮询工作，不是即时推送：本地变化最坏要等一个「本地检查间隔」（默认 5 秒）才推送，
  远端变化取决于「远端轮询间隔」。
