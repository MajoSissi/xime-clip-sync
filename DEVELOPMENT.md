# Xime Clip Sync · 开发文档

面向改这个项目的人。用户向介绍见 [README.md](README.md)。

---

## 一、构建

需要 **Go 1.23+**，零第三方依赖、零 cgo。产物输出到 `build/`：

```bash
./build.sh                  # Windows 下用 build.bat
GO=/path/to/go ./build.sh   # go 不在 PATH 里（mise/asdf 的 shim 没生效）时这样指定
```

构建脚本的顺序是**先跑测试 → 再生成图标资源 → 最后编译**：

```bash
go run genico.go icon.go    # 生成 rsrc_windows_amd64.syso，跳过这步会丢图标和版本号
go build -ldflags "-s -w -H=windowsgui" -o build/xime-clip-sync.exe
```

⚠️ **别在项目根跑 `go build ./...`**——会吐一个不带 `-s -w` 的 exe。日常检查用 `go vet ./...` / `go test ./...`。

当前产物：`build/xime-clip-sync.exe`，v1.3.0，仅 Windows/amd64，8,561,152 字节。

**换图标只需替换 `img/logo.ico` 后重新构建**，不用改任何代码。

---

## 二、命令行参数

```
-config <path>   指定配置文件路径（默认：程序同目录的 config.json）
-open-ui         启动时自动打开配置界面（默认只在托盘后台运行）
-no-ui           不启动配置界面
-no-tray         不显示托盘图标
-test            测试 WebDAV 连接后退出
-once push|pull  执行一次同步后退出（适合放进计划任务）
-version         显示版本
-tray-test       开发用：投递一轮托盘菜单命令后退出
```

程序是**无控制台**构建，标准输出不会出现在任何地方，这几个参数只通过**退出码**（0 成功 / 1 失败）表达结果；
`-tray-test` 和正常运行一样把过程写进 `logs/`。

---

## 三、与输入法插件的协议对应

协议对齐插件的 `main.ts` 与 `libs/dav-url.ts`：

| 环节 | 插件（main.ts） | 本项目 |
|------|----------------|--------|
| 文件路径 | `{davUrl}/{remotePath}/clipboard/current.json` | `{davUrl}/{remotePath}`（`remotePath` 具体到文件） |
| 写入 | `PUT` + `Content-Type: application/json` | 同左 |
| 读取 | `GET` + `If-None-Match`，304 视为无变更 | 同左 |
| 目录 | PUT 返回 409/404 → 逐级 `MKCOL` → 重试一次 | 同左 |
| 认证 | `Authorization: Basic base64(user:pass)`，用户名为空则不带 | 同左 |
| 连接测试 | `PROPFIND` + `Depth: 0`（部分服务对 HEAD 返回 503） | 同左 |
| 限流 | 503 进入退避，避免延长封禁 | 拉取退避 10 分钟 / 推送退避 3 分钟 |
| 字段 | `type / hash / text / has_data / data_name / size / source`（snake_case） | 同左 |

> **⚠️ 两端 `remotePath` 语义不同（尚未统一）。** 插件把它当**目录**（文件名由插件固定追加，
> 留空 = 服务器根目录）；本项目把它当**完整文件路径**（留空 = `xime/clipboard/current.json`）。
> 两边都留空会指向不同文件。要同步同一文件：插件填 `Android/xime` → 本项目填 `Android/xime/clipboard/current.json`。

`size` 是文本的 UTF-8 字节长度，与插件的 `new TextEncoder().encode(text).length` 一致。

**关于 `hash`**：插件注释写「留空让宿主计算」，但 ximed 宿主闭源、算法无法确认。
本项目**固定按本地 SHA-256 计算并写入**——这是插件端认的算法。它不是配置项，界面上也不出现。
即使对端用了别的算法也**不会造成同步死循环**，因为判重靠文本内容而不是 hash（见第五节）；
读到远端 `hash` 与本地不一致时只记一条 warn，不影响同步。

---

## 四、密码怎么存（不落明文）

落盘的是 `passwordEnc` 字段：Windows 自带的 **DPAPI**（`crypt32.dll` 的 `CryptProtectData`）加密后再 base64。

- **绑定当前 Windows 用户**：主密钥由系统托管，`config.json` 拷到别的电脑或账号下**解不开**。
- **零依赖**：就是一次 `syscall`，没有引入任何加密库，体积也没变。
- **明文只在内存里**：Basic Auth 需要明文密码，解密结果只留在进程内存中用于发请求，绝不写回文件。
- **加密失败不吞掉**：DPAPI 失败时保留原文件不动，把原因写进日志并告警，不退化成写明文。
- **解不开就重填**：重装系统 / 换电脑 / 换账号后旧密文失效，日志会提示，打开界面重输一次即可。

老配置里若还留着明文 `password` 字段，**启动时会立刻加密并重写文件**，并记一条日志
`检测到配置里的明文密码，已改为加密保存`。

界面 `/api/config` 只回 `passwordSaved: true`，不回任何密码材料；
服务端也会**忽略浏览器提交上来的 `passwordEnc`**，不给网页往配置里塞任意内容的机会。

---

## 五、同步逻辑与回声抑制

核心是维护一个 `current`——「两端已达成一致的内容」：

```
每个周期：
  1. 本地剪贴板 != current  →  推送到远端，成功后 current = 本地内容
  2. 远端内容   != current  →  写入本地剪贴板，current = 远端内容
```

步骤 2 写入后 `current` 就等于远端内容，下一轮读到的本地剪贴板与之相等，**不会**再被推回远端
——这就是打断「电脑推给手机、手机又推回电脑」死循环的关键，即使两端 hash 算法不同也不会互相刷屏。

其他细节：

- **两个独立节奏**：本地检查默认每 5 秒一次（廉价、不联网），远端拉取默认每 30 秒一次。
- **启动策略**：以本地剪贴板建立基线，然后拉一次远端。远端有内容则以远端为准；远端还没有文件时用本地内容初始化远端。
- **本地变更检测**：Windows 下先用 `GetClipboardSequenceNumber` 廉价预判，序号没变就不读剪贴板内容。
- **ETag**：推送成功后缓存响应里的 ETag，下次拉取带 `If-None-Match`，命中 304 直接跳过。
- **启动不重复拉取**：同步循环的「下次拉取时间」排在间隔之后（`prime()` 已完整跑过一轮），热启动从 2 个请求降到 1 个。
- **两个长度上限**：`maxTextChars`（自动同步，默认 300）与 `manualMaxTextChars`（手动推送，默认 10000）。
  托盘「推送」与界面右上角「推送」共用 `PushNow()`，走后者；`prime`/`checkLocal` 两条自动路径只看前者。两者互不影响，0 = 不限。
- **超长内容不写进 `current`**：「本地内容」显示的是 `current`，所以超长内容不会显示在界面上；
  否则界面会显示一段其实没同步过去的内容，且下一轮远端轮询会把它当成「本地变了」而用远端内容冲掉。
  去重靠独立的 `oversizeText` 字段。

---

## 六、日志实现

日志按天分文件写在 **exe 同目录的 `logs/`**：

- **跨天自动轮转**：写入时发现日期变了就切新文件，不需要重启。
- **保留期**默认 7 天（含今天）：启动时清理一次，之后每 6 小时清理一次。
- **只删自己的文件**：只认 `xime-clip-sync-YYYY-MM-DD.log` 命名。
- **单文件上限**默认 2 MB（0 = 不限制）：达到上限后**当天不再写盘**（内存日志不受影响），
  并往文件末尾补一条 `本日日志已达 2 MB 上限，后续日志只保留在内存中`。
- `日志保留天数 = 0` 表示永久保留。

`logs/` 总占用大致是 **单文件上限 × 保留天数**（默认 2 MB × 7 天 ≈ 14 MB）。

**日志文案要短**：最高频的两行是 `⬆ 推送 （N 字符）` / `⬇ 拉取（N 字符，来自 <设备>）`，
由 `TestSyncLogLinesAreShort` 守着（含**反向哨兵**：出现旧长文案就报错；长度 ≤ 24 rune）。
手动推送/拉取成功**不再单独打一行**；**拉取没变化时不记**——那条路径每个轮询周期都走。
其余告警（hash 不一致、超上限）都有「只报一次」保护，所以高频行只有这两条。

---

## 七、图标与 DPI

图标只有**一个来源**：仓库里的 `img/logo.ico`，程序里**没有任何「画图标」的代码**。

| 用途 | 怎么来的 |
|------|---------|
| exe 图标（资源管理器 / 任务栏 / Alt-Tab） | 构建期由 `genico.go` 转成 `rsrc_windows_amd64.syso`，`go build` 自动链接 |
| 托盘图标 | 运行期用 `//go:embed` 内嵌同一份 `img/logo.ico`，按系统 DPI 挑一档，交给 `CreateIconFromResourceEx` |

`img/logo.ico` 有 8 档尺寸（256/128/96/64/48/32/24/16），全部原样保留：exe 里 8 帧 `RT_ICON`
与源文件逐字节一致；托盘按 `GetSystemMetrics(SM_CXSMICON)` 挑一档（100% 缩放是 16×16，150% 是 24×24），**1:1 绘制**。

**托盘图标为什么不糊**：进程默认「DPI 不感知」，系统会把界面按缩放比例做位图拉伸——150% 缩放下任务栏要 24×24，
而不感知的进程只能拿到虚拟化后的 16，外壳放大绘制就是「糊」。所以程序第一件事（早于创建任何窗口）就是声明 DPI 感知：

```
SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)   Windows 10 1703+
  ↓ 失败
SetProcessDpiAwareness(PROCESS_PER_MONITOR_DPI_AWARE) Windows 8.1+
  ↓ 失败
SetProcessDPIAware()                                  Vista+
```

生效后 `GetSystemMetrics` 返回真实尺寸。日志里能看到结果：

```
DPI 感知已启用：Per-Monitor V2
托盘图标已就绪：24×24（系统要求 24×24，来自 img/logo.ico）
```

挑档策略：正好相等 > 比目标大的里面最小的 > 最大的那一档（宁可缩小丢像素，也不放大插值发虚）。

`.syso` 装了 `RT_ICON`×8、`RT_GROUP_ICON`、`RT_VERSION` 三种资源，链接后资源管理器、任务栏、Alt-Tab
都会显示正确图标，右键「属性」也能看到版本号（从 `main.go` 解析，与 `-version` 一致）。

**为什么自己生成 `.syso` 而不用 `windres`？** 为了坚持零第三方依赖、零外部工具链。
`.syso` 就是一个简单的 COFF 对象文件（`.rsrc` 段 + 重定位 + 段符号）。两个坑：段特征位必须**恰好**是
`CNT_INITIALIZED_DATA | MEM_READ`（带 `MEM_DISCARDABLE` 会被链接器整个跳过，产出没有图标的 exe）；
资源目录里的偏移是 RVA，每处都要生成重定位。两条都有测试盯着。

---

## 八、文件结构

```
xime-clip-sync/
├── main.go                 入口、命令行参数、装配同步循环/配置界面/托盘
├── sync.go                 同步引擎（推/拉、回声抑制、限流退避、长度上限）
├── webdav.go               WebDAV 客户端（PUT/GET/PROPFIND/MKCOL）
├── davurl.go               远端 URL 组装（对齐插件 dav-url.ts）
├── profile.go              远端 JSON Profile 编解码 + 界面用的格式化清单
├── clipboard.go            剪贴板接口定义
├── clipboard_windows.go    Windows：直接调用 user32/kernel32
├── tray.go                 托盘接口与菜单动作定义
├── tray_windows.go         Windows 托盘（Shell_NotifyIconW + 隐藏消息窗口 + 气泡 + 鼠标手势）
├── dpi_windows.go          声明进程 DPI 感知（托盘图标 1:1 绘制的前提）
├── icon.go                 内嵌 img/logo.ico + ICO 解析（运行期托盘图标用）
├── icon_windows.go         从 ICO 帧生成 HICON（CreateIconFromResourceEx）
├── genico.go               构建期工具：把 img/logo.ico 转成 .syso（//go:build ignore，不进二进制）
├── secret_windows.go       密码加解密：DPAPI
├── autostart.go            自启的公共部分（程序路径、自启项名称）
├── autostart_windows.go    开机自启：读写注册表 Run 项
├── singleinstance_windows.go  单实例：独占文件句柄
├── config.go               配置读写与迁移（固定与程序同目录，绿色便携）
├── uiserver.go             配置界面监听器（端口绑定 / 运行时切换 / 地址文件）
├── ui.go                   配置界面路由与服务端（含密码脱敏）
├── logbuf.go               日志：内存环形缓冲 + 按天分文件 + 保留期清理
├── web/index.html          配置界面（内嵌进二进制）
├── img/logo.ico            图标唯一来源（换图标只动这个文件）
├── *_test.go               测试（见下一节）
├── build.bat / build.sh    一键构建脚本
└── rsrc_windows_amd64.syso （构建期生成）Windows 资源对象
```

两个目录需要说明：

- **`build/`** —— exe 输出目录，程序运行时的 `config.json`、`logs/`、`xime-clip-sync.ui` 也在这里。整个目录已 gitignore。
- **`rsrc_windows_amd64.syso`** —— 必须留在源码根，`go build` 才会自动链接进 exe，所以单独 gitignore。

---

## 九、测试

```bash
go test ./...          # 全部 93 个
go test ./... -short   # 跳过会改动真实剪贴板/注册表的测试
```

用 `httptest` 起了模拟 WebDAV 服务器，覆盖 URL 组装、wire 字段格式、纯文本兼容、目录自动创建、
Basic Auth、本地→远端、远端→本地、回声抑制、ETag 304、503 退避。

会真实改动本机状态的测试（结束即恢复，可用 `-short` 跳过）：

- `TestRealClipboardRoundTrip`：真的读写剪贴板，覆盖中文、emoji、`\t`/`\r\n`、1500 字长文本与空串，
  并校验剪贴板序号随写入递增。
- `TestAutostartRegistry`：真的写入并删除注册表 Run 项；检测到已有自启项会跳过，以免影响现网配置。
- `TestSingleInstanceLock`：第二个实例拿不到锁、释放后可重新获取。

几组关键约束由测试守着，改坏了会立刻变红：

| 关注点 | 测试 |
|--------|------|
| 长度上限按字符不是字节 | `TestOversizeNotPushed`（60 个汉字 = 180 字节，在 100 字符上限内**要**推送） |
| 手动推送能突破自动上限、但受「手动推送上限」约束（两个上限互不影响） | `TestManualPushBypassesAutoLimit` |
| 超长内容不显示在「本地内容」里、也不会被远端轮询冲掉剪贴板 | `TestOversizeNotShownAsCurrentText` |
| 「本地检查间隔」的单位是秒（只改文案忘了改倍数会静默变成高频轮询） | `TestLocalPollIntervalIsSeconds` |
| 旧配置升级不丢上限 | `TestConfigUpgradeFromBytesLimit` |
| 日志轮转 / 单文件上限 / 保留期 | `TestLogFile`、`TestLogFileSizeCap`、`TestLogFileSizeUnlimited`、`TestLogRetention` |
| 保留期清理不碰用户自己的文件 | `TestLogRetentionSkipsFilesWithoutDate` |
| 单文件上限的取值范围与换算 | `TestLogMaxFileMBBounds` |
| 旧配置不丢单文件上限 | `TestConfigUpgradeKeepsLogFileCap` |
| 落盘不留明文密码 | `TestSaveConfigEncryptsPassword`、`TestLoadConfigMigratesPlaintextPassword` |
| 界面不泄露密码 | `TestAPIConfigNeverReturnsPassword`、`TestAPISaveWithBlankPasswordKeepsStored` |
| 「远程路径」留空落到默认文件 | `TestRemotePathDefaultsToXimeWhenBlank`、`TestWebDAVClientUsesDefaultRemotePath` |
| 留空不被写回配置 | `TestRemotePathBlankIsNotWrittenBack` |
| 填了值不再自动补文件名 | `TestRemotePathUserValueIsKept` |
| 纯函数只做拼接 | `TestBuildFileURL` |
| 不做旧格式识别，远程路径原样保留 | `TestLoadConfigKeepsRemotePathAsIs` |
| 「远端原始内容」字段顺序 + 首尾截断 | `TestRemoteRawText`、`TestRemoteRawTextNilFields`、`TestHeadTail` |
| 内容预览 150 字符截断 | `TestStatusPreviewTruncatesAt150` |
| 配置固定在 exe 目录 | `TestConfigPathIsExeDir` |
| 启动不重复拉取 | `TestNoRedundantStartupFetch` |
| 托盘单击无响应、双击开界面 | `TestTrayClickAction`、`TestTrayWindowClassHasDblClks` |
| 托盘菜单排版与文案（顶部一行状态 + 两条分隔线、无同步开关项） | `TestTrayMenuLayoutOrder`、`TestTrayMenuStatusItemsOnTop`、`TestTrayMenuIDsAreDistinct`、`TestTrayMenuHasNoToggleItem`、`TestTrayMenuLabels` |
| 菜单状态行取自当前状态、不写死 | `TestTrayMenuStatusSource` |
| 状态行只做二选一（正常 / 异常） | `TestTrayMenuSyncLabel` |
| 状态行文案写死、够短（≤14 字）、无占位符字面量 | `TestTraySyncTextsAreShort` |
| 每个配置字段都在界面 FIELDS 里（漏了会静默丢设置） | `TestEveryConfigFieldIsInTheForm` |
| 「程序设置」页每个输入框 id 都是配置字段名 | `TestAppPageFieldsAreConfigFields` |
| 「恢复默认值」按钮在本页里、且本页没有排除项 | `TestRestoreButtonScope` |
| 默认值接口回**出厂默认**、不含密码材料、不再返回已删的 `hashMode` | `TestDefaultsEndpointReturnsFactoryDefaults` |
| 失败原因分类（端口冲突 vs 未运行） | `TestUIFailReasonOf`、`TestUIFailReasonOnRealBindFailure` |
| 「起过又没了」要能和「在跑」区分 | `TestUIServerFailedOnlyOnUnexpectedExit` |
| 打开配置界面失败会弹气泡 | `TestOpenUIReportsFailureOnBalloon`、`TestOpenUISuccessStaysSilent` |
| 菜单命令分发（含未知 ID 必须无反应） | `TestTrayHandleCommandDispatches` |
| 托盘状态词够短（≤4 字、无标点） | `TestTrayStatusLabelIsShort` |
| 图标挑档宁大勿小 | `TestPickICOPreference`、`TestLogoICOHasSizesNeededByDPI` |
| 端口：固定默认 `31213`，配置里永远存一个具体端口号 | `TestUIPortNormalize`、`TestLoadConfigUsesDefaultUIPortWhenMissing`、`TestUIPortUserChoiceIsKept`、`TestLoadConfigGeneratesFileOnFirstRun` |
| 端口被占用时**直接报错、不换端口** | `TestListenFailsWhenPortTaken`、`TestStartFailsWhenPortTaken`、`TestListenRejectsIllegalPort` |
| 端口运行时切换：失败要留在原端口 | `TestUIServerRebindSwitchesPort`、`TestUIServerRebindKeepsOldPortOnFailure`、`TestUIServerStartAndURLFile`、`TestSaveWithUnavailablePortWarnsAndKeepsUI` |
| 推送/拉取日志要短 | `TestSyncLogLinesAreShort` |
| 配置不见了要告警（而不是静默重置） | `TestLoadConfigWarnsWhenConfigDisappeared`、`TestLoadConfigNoWarnOnGenuineFirstRun` |
| 保存时附带动作失败要**全部**报出来 | `TestWarningsCollectAllMessages`、`TestSaveReportsEveryWarningNotJustFirst` |
| 换端口后引导页不能把提示吞掉 | `TestSwitchedPageKeepsWarning` |
| exe 内图标与源文件一致 | `TestSYSOIconMatchesLogoICO` |

---

## 十、改配置项时的三处平行清单

Go 结构体 / 前端 `FIELDS` 数组 / 页面 `<input id>` 三处必须同步。**漏了 `FIELDS` → 每次保存静默丢掉用户填的值**，
配 `TestEveryConfigFieldIsInTheForm` 守着。

改「单位」时（毫秒→秒这种）额外要改：字段名 · `defaultConfig()` · `normalize()` 的钳制 ·
**引擎里的换算倍数** · `<label>` · `min/max` · 前端 `FIELDS`/`NUMS` · 测试里手写的 JSON body · README。
**漏换算倍数最阴**：配置写 5 会变成 5 毫秒轮询，界面却显示「5 秒」，看不出任何异常。

删配置项时的清单比「删字段」长得多：字段 · 常量 · `FIELDS`/`NUMS`/`BOOLS` · `<select>` · `normalize()` 分支 ·
函数签名 · 调用点 · 测试 · 文档 · **界面与日志里「引导用户去改它」的文案**。
