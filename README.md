# Xime Clip Sync · 桌面端 WebDAV 剪贴板同步

[Xime 输入法](https://github.com/ximeiorg/Xime) 的 `webdav-clipboard-sync` 插件只能在安卓端使用。
本项目是它的**桌面端对应实现**：用同一套 WebDAV 文件协议，让 Windows 电脑和手机双向同步剪贴板**文本**。

协议逐字对齐插件的 `main.ts` 与 `libs/dav-url.ts`，两端指向同一个远端文件，可以和手机同时使用。

程序常驻系统托盘：**双击后直接进托盘，不弹任何窗口**；之后双击托盘图标打开配置界面，右键弹出菜单。
只支持 **Windows / amd64**，零第三方依赖、零 cgo，编出来就是一个能拷进 U 盘的 `.exe`（约 8.5 MB）。

---

## 一、快速开始

### 构建

需要 Go 1.23+。产物输出到 `build/`，源码目录保持干净：

| 文件 | 用途 |
|------|------|
| `build/xime-clip-sync.exe` | 不弹控制台，常驻托盘，双击就能用 |

```bash
./build.sh                  # Windows 下用 build.bat；先跑测试，再生成图标资源，最后编译
GO=/path/to/go ./build.sh   # go 不在 PATH 里（mise/asdf 的 shim 没生效）时这样指定
```

**要换图标就换 `img/logo.ico`**，重新构建即可，不用改任何代码。

### 运行

双击 `build/xime-clip-sync.exe`。程序不弹窗口，直接驻留托盘，悬停图标可看到当前状态。

`config.json`、`logs/`、`xime-clip-sync.ui` 都在 **exe 同目录**下，整个文件夹拷走就能带走：

```
build/
├── xime-clip-sync.exe
├── config.json              ← 配置（密码是 DPAPI 密文，不是明文）
├── xime-clip-sync.ui        ← 当前配置界面地址
└── logs/
    └── xime-clip-sync-2026-09-27.log
```

exe 所在目录不可写时，程序会在日志里明确告警，**不会**悄悄把配置挪到 `%AppData%`——那样你会找不到自己的配置。

还没配置过服务器时，托盘会弹气泡提示你去配置。**双击**托盘图标打开配置界面。
配置界面固定监听 `31213`，地址就是 `http://127.0.0.1:31213/`；
想换端口在界面「程序设置 · 配置界面」里改，保存后立即生效。当前地址也记在 `xime-clip-sync.ui` 里。

### 配置项

界面分两行：**第一行是标题 + 同步状态 + 右上角三个动作**（推送 / 拉取 / 保存并应用，`Ctrl+S` 也能保存），
**第二行是页签**，底色比顶栏深一档、上下各一条分隔线，和顶栏明确隔开。

页签分三页：**同步设置**（连接配置 + 同步行为）、**运行状态**（运行状态 + 运行日志）、
**程序设置**（启动与日志 + 配置界面，页底一个「恢复默认值」）。
选中的页签写在地址栏的 `#` 后面，刷新后还停在原来那一页。

「测试连接」的响应**就地显示在按钮同一行的右侧**（顶栏三个按钮的响应则浮在按钮下方），
所以不用满屏找提示。这条提示是绝对定位的，出现和消失**不会改变卡片高度**——
否则点一下按钮整张卡片就跳一下。文案过长会省略号截断，鼠标悬停可看全。

「运行状态」页里的「本地已同步内容」和「远端内容」最多显示 **150 字符**，超出就截断加省略号
（这里只是让你确认「同步的是不是这条」，完整内容看剪贴板就行）。

**连接配置**

| 配置项 | 说明 | 示例 |
|--------|------|------|
| WebDAV 服务器地址 | 服务器根地址，含协议与端口 | `https://dav.jianguoyun.com/dav/` |
| 远程路径 | 剪贴板文件路径，**要具体到文件**；留空即 `xime/clipboard/current.json` | `rime/clip/1.json` |
| 用户名 / 密码 | WebDAV 账号（坚果云用「应用密码」） | — |
| 本机设备名 | 写入远端 `source` 字段，便于对端区分来源 | `my-pc` |
| 跳过 TLS 证书校验 | 自签证书的局域网服务器勾选 | 否 |

「远程路径」留空就等价于填 `xime/clipboard/current.json`。填了值就按填的来，
**程序不会再自动追加任何文件名**——所以 `xime` 指的是「一个叫 `xime` 的文件」，不是目录。

要和手机端插件指向同一个文件，就填「插件里那个远程目录 + `/clipboard/current.json`」：
插件里填 `Android/xime`，这里就填 `Android/xime/clipboard/current.json`。

> 早期版本的「远程路径」填的是**目录**（程序固定追加 `clipboard/current.json`）。
> 现在**不做任何旧格式识别或自动补全**：填什么就是什么。
> 如果你手上还有那种老配置，按上面的规则手动补上文件名即可。

**同步行为**

| 配置项 | 默认 | 说明 |
|--------|------|------|
| 启用同步 | 是 | 关掉后程序仍在托盘，只是不推不拉 |
| 文本长度上限（字符） | `300` | 本地复制的文本超过这个字数就不推送，0 表示不限制 |
| 远端轮询间隔（秒） | `30` | 多久去远端看一次有没有新内容 |
| 本地检查间隔（毫秒） | `1000` | 多久看一次本机剪贴板（**不产生任何网络请求**） |
| hash 字段模式 | `sha256` | `sha256` 本地计算 / `empty` 交给对端 |

> 「文本长度上限」按**字符**算，不是字节。200 个汉字算 200 字符（在默认上限内，会同步），
> 但写进远端文件时 `size` 字段是 600（UTF-8 字节数）——协议要求，两者不是一回事。
> 超长内容不会被推送，「超长跳过」计数 +1，日志记一条 warn。

**启动与日志**

| 配置项 | 默认 | 说明 |
|--------|------|------|
| 开机自动启动 | 否 | 写 `HKCU\...\CurrentVersion\Run`，不需要管理员权限 |
| 日志写入文件 | 是 | 写到 `logs/`，按天分文件 |
| 日志保留天数 | `7` | 过期日志自动删除，0 表示永久保留 |
| 单个日志文件上限（MB） | `2` | 单个日志文件达到上限后当天不再写盘（内存日志不受影响），0 表示不限制 |

**配置界面**

| 配置项 | 默认 | 说明 |
|--------|------|------|
| 监听端口 | `31213` | 改完**立即生效**；端口被别的程序占着时，会提示你改配置文件再重启 |

> 端口是**程序自己的**设置，和日志无关，所以单独一张卡片放在「程序设置」页里；
> 它下面那行「当前地址」可以点开或复制。

> 密码**不会明文保存**（见第四节），界面也不回显。密码框**留空 = 不修改**，只有真的输入了新密码才会覆盖。

### 命令行参数

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

程序是**无控制台**构建，标准输出不会出现在任何地方，所以这几个参数只通过
**退出码**（0 成功 / 1 失败）表达结果；`-tray-test` 和正常运行一样把过程写进 `logs/`。
日常用不上这些参数，双击 exe 就行。

### 托盘

| 操作 | 行为 |
|------|------|
| 左键单击 | **无响应**（图标缩在任务栏角落，误触概率高，一碰就弹浏览器很烦人） |
| 左键双击 | 用默认浏览器打开配置界面 |
| 右键 | 弹出菜单（顶部一行是当前状态） |
| 悬停 | 显示同步状态（同步中 / 已停用 / 未配置 / 限流退避 / 同步出错） |

右键菜单从上到下是：

```
同步中 🔄                ← 状态：同步在不在正常跑
──────────
推送
拉取
──────────
设置
退出
```

三段：**状态 / 高频的手动同步 / 低频的「程序级」入口**。顶部那行是**灰色不可点**的，
每次弹菜单时现取，所以永远是最新的。

它只做二选一：`同步中 🔄` / `未同步 🛑`。想知道具体是限流退避还是同步出错看悬停提示，
想知道为什么看界面「运行状态」页。（`🛑` 也包括「你自己在界面上关了同步」——
菜单只报事实，不替用户解释原因。）

文案写死在程序里，界面上没有对应的设置项。

配置界面**起不来**（端口被占）时，右键「设置」会弹气泡说明原因，
并告诉你改 `config.json` 里的 `uiPort` 再重启。

**恢复默认值**

「程序设置」页最底下有一个「恢复默认值」按钮：把**这一页**各项恢复成出厂默认
（启动与日志 + 配置界面），**只填表、不保存**——点完还要点右上角「保存并应用」才生效。
「同步设置」页不受影响，WebDAV 地址和密码更不会被清掉。

监听端口**也参与恢复**（回到 `31213`）。注意它同时也是本页所在的地址：
保存后页面会跳到新端口，界面上会提示你新地址——想留在原端口就把它填回去再保存。

出厂默认值只在程序里定义一份（`defaultConfig()`），界面通过 `GET /api/defaults` 取，
前端**不另抄一份**——抄一份就多一个会悄悄漂移的地方。

> 端口被占用时界面**会起不来**（见常见问题），启动日志和托盘气泡都会告诉你改哪里。

**点了「设置」没反应？** 会弹气泡说明原因（例如「配置界面不可用（端口冲突），详见日志」），
最常见的原因就是 `31213` 被别的程序占了。
无控制台的构建里，气泡是这时候唯一能主动够到你的手段——光写日志是没人会去翻的。

悬停提示里的状态词一律压到 4 个字以内，具体原因（哪次请求失败、退避到几点）在界面「运行状态」页里看。

菜单里**没有「暂停同步」**：同步开关只在界面「同步设置 · 同步行为」里改。托盘图标离鼠标太近，
右键菜单又长得很像，误点一下就把同步关了、还看不出是谁关的，代价远大于省一次点击。
「设置」双击托盘图标也能进。

### 单实例保护

启动时独占一个 `xime-clip-sync.lock`（与配置同目录）。检测到已有实例就**直接退出并打开已有实例的配置界面**，
而不是再起一条同步循环——否则两个副本会同时读写同一个剪贴板、互相覆盖来回推送。

运行中的实例会把配置界面地址写进 `xime-clip-sync.ui`；第二个实例优先读它，
读不到才退回配置里指定的端口。地址文件在正常退出时删除；
被强杀留下的残留文件探测不通时，只报告「已在运行」而不会去开一个死链。

---

## 二、远端读写费不费流量？

**很省。** 稳态下每次轮询只发 1 个条件 GET，服务器回 304（没有新内容，不回正文），一来一回约 **330 字节**。
本地每秒检查剪贴板那部分**完全不碰网络**。

下面是实测数据（用一个带字节计数的模拟 WebDAV 服务器精确统计收发的每个字节，再让真实 exe 去跑）：

| 轮询间隔 | 每天请求 | 每天流量 | 占坚果云免费配额 |
|---------|---------|---------|----------------|
| **30 秒（默认）** | 2,880 | **0.9 MB** | **10%** |
| 15 秒 | 5,760 | 1.8 MB | 20% |
| 5 秒 | 17,280 | 5.4 MB | 60% |
| 3 秒 | 28,800 | 9.1 MB | **100%（会触顶）** |

坚果云免费版限制 **600 次请求 / 30 分钟**。瓶颈是**请求次数**而不是流量：每天不到 1 MB 的流量可以忽略，
但 3 秒轮询会把配额吃满。手机端也在轮询同一个文件时，两边间隔建议 **≥ 600 / (30 × 设备数)** 秒
——两台设备各 30 秒是安全的。

启动时的一次性开销：远端已有文件 1 个请求（~0.5 KB）；远端还没有文件 5 个请求（~2 KB，
GET 404 → PUT 409 → MKCOL ×2 → PUT 201），一辈子只发生一次。之后只有剪贴板内容**真的变了**才 PUT。

---

## 三、与输入法插件的协议对应

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

> **⚠️ 两端 `remotePath` 的语义不同（尚未统一）。**
> 插件把它当**目录**，文件名 `clipboard/current.json` 由插件固定追加，留空 = 服务器根目录；
> 本项目把它当**完整文件路径**，留空 = 默认文件 `xime/clipboard/current.json`。
> 两边都留空会指向不同的文件。要让两台设备同步同一个文件，按插件端的写法填：
> 插件填 `Android/xime` → 本项目填 `Android/xime/clipboard/current.json`。

`size` 是文本的 UTF-8 字节长度，与插件的 `new TextEncoder().encode(text).length` 一致。

**关于 `hash`**：插件注释写「留空让宿主计算」，但 ximed 宿主闭源、算法无法确认，所以本项目提供两种模式
（`sha256` / `empty`）。**两种都不会造成同步死循环**，因为判重靠文本内容而不是 hash（见第五节）。
程序每次读到远端内容都会拿它的 `hash` 与本地 SHA-256 比对，不一致就在界面上提示并记一条 warn
——不影响同步，只是让你一眼确认对端到底用没用同一种算法。

---

## 四、密码怎么存（不落明文）

落盘的是 `passwordEnc` 字段：Windows 自带的 **DPAPI**（`crypt32.dll` 的 `CryptProtectData`）加密后再 base64。

- **绑定到当前 Windows 用户**：主密钥由系统托管，所以 `config.json` 拷到别的电脑或别的账号下**解不开**。
  这是有意的——配置可以随手拷走，密码不会跟着泄露。
- **零依赖**：就是一次 `syscall`，没有引入任何加密库，体积也没变。
- **明文只在内存里**：Basic Auth 需要明文密码，解密结果只留在进程内存中用于发请求，绝不写回文件。
- **加密失败不吞掉**：DPAPI 调用失败时保留原文件不动，把原因写进日志并告警，而不是退化成「把明文写下去」。
- **解不开就重填**：重装系统、换电脑、换 Windows 账号后旧密文失效，日志会给出提示，
  打开配置界面重新输一次即可，其余配置都还在。

老配置里若还留着明文 `password` 字段，**启动时会立刻加密并重写文件**，同时记一条
`检测到配置里的明文密码，已改为加密保存`。

界面 `/api/config` 只回一个 `passwordSaved: true` 布尔值，不回任何密码材料；
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

- **两个独立节奏**：本地检查每秒一次（廉价、不联网），远端拉取默认每 30 秒一次。
  这样「复制完立刻同步到手机」的延迟基本由本地检查决定（1 秒内），
  而「手机推过来的内容」最坏等一个远端轮询间隔。
- **启动策略**：以本地剪贴板建立基线，然后拉一次远端。远端有内容则以远端为准；
  远端还没有文件时，用本地内容初始化远端。
- **本地变更检测**：Windows 下先用 `GetClipboardSequenceNumber` 做廉价预判，序号没变就不去读剪贴板内容。
- **ETag**：推送成功后缓存响应里的 ETag，下次拉取带 `If-None-Match`，命中 304 直接跳过。
- **启动不重复拉取**：同步循环的「下次拉取时间」排在间隔之后（启动时的 `prime()` 已经完整跑过一轮），
  热启动从 2 个请求降到 1 个。

---

## 六、日志与保留期

日志按天分文件写在 **exe 同目录的 `logs/`** 下：

```
logs/
├── xime-clip-sync-2026-09-26.log
└── xime-clip-sync-2026-09-27.log   ← 今天
```

- **跨天自动轮转**：写入时发现日期变了就切到新文件，不需要重启。
- **保留期**默认 7 天（含今天）：启动时清理一次，之后每 6 小时清理一次。
- **只删自己的文件**：只认 `xime-clip-sync-YYYY-MM-DD.log` 这个命名，同目录下别的东西不会被动。
- **单文件上限**默认 2 MB，可在界面上改（0 = 不限制）。达到上限后**当天不再写盘**
  （内存日志不受影响），避免某天异常刷屏把磁盘写满；同时往文件末尾补一条
  `本日日志已达 2 MB 上限，后续日志只保留在内存中`，免得事后以为是程序不写日志了。
- `日志保留天数 = 0` 表示永久保留、不自动清理。

两个限制是配合使用的，`logs/` 的总占用大致就是 **单文件上限 × 保留天数**（默认 2 MB × 7 天 ≈ 14 MB）。
启动时日志里会打印当前策略，一眼能看到实际生效的值：

```
日志已落盘：…\logs（保留 7 天，单文件上限 2 MB）
```

程序没有控制台，`logs/` 是唯一的排查线索。

---

## 七、图标与 DPI

图标只有**一个来源**：仓库里的 `img/logo.ico`，程序里**没有任何「画图标」的代码**。

| 用途 | 怎么来的 |
|------|---------|
| exe 图标（资源管理器 / 任务栏 / Alt-Tab） | 构建期由 `genico.go` 转成 `rsrc_windows_amd64.syso`，`go build` 自动链接进 exe |
| 托盘图标 | 运行期用 `//go:embed` 内嵌同一份 `img/logo.ico`，按系统 DPI 挑一档尺寸，交给 `CreateIconFromResourceEx` |

`img/logo.ico` 里有 8 档尺寸（256/128/96/64/48/32/24/16），全部原样保留：exe 里 8 帧 `RT_ICON`
与源文件逐字节一致；托盘则按 `GetSystemMetrics(SM_CXSMICON)` 挑一档
（100% 缩放是 16×16，150% 是 24×24），**1:1 绘制，不做缩放**。

### 托盘图标为什么不糊

这里有个容易踩的坑：**进程默认是「DPI 不感知」的**。不声明的话，系统会把整个界面按缩放比例做位图拉伸
——于是 150% 缩放的屏幕上，任务栏要 24×24 的图标，而 DPI 不感知的进程调 `GetSystemMetrics(SM_CXSMICON)`
只能拿到虚拟化后的 **16**。交出 16×16 由外壳放大到 24×24 绘制，看上去就是「糊」。

所以程序**第一件事**（早于创建任何窗口）就是声明 DPI 感知，逐级回退以兼容老系统：

```
SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)   Windows 10 1703+
  ↓ 失败
SetProcessDpiAwareness(PROCESS_PER_MONITOR_DPI_AWARE) Windows 8.1+
  ↓ 失败
SetProcessDPIAware()                                  Vista+
```

生效之后 `GetSystemMetrics` 返回的就是真实尺寸，挑到同一档、1:1 绘制。日志里能看到实际结果：

```
DPI 感知已启用：Per-Monitor V2
托盘图标已就绪：24×24（系统要求 24×24，来自 img/logo.ico）
```

另一处细节是**挑档策略**：正好相等 > 比目标大的里面最小的 > 最大的那一档。
宁可拿大图缩小（丢像素），也不拿小图放大（凭空插值、边缘发虚）。

`.syso` 里装了 `RT_ICON`×8、`RT_GROUP_ICON`、`RT_VERSION` 三种资源，链接后资源管理器、任务栏、
Alt-Tab 都会显示正确图标，右键「属性」里也能看到版本号（版本号从 `main.go` 解析，不会和 `-version` 对不上）。

**为什么自己生成 `.syso` 而不用 `windres`？** 为了坚持零第三方依赖、零外部工具链。
`.syso` 就是一个很简单的 COFF 对象文件（一个 `.rsrc` 段 + 重定位 + 一个段符号）。两个坑：
段特征位必须**恰好**是 `CNT_INITIALIZED_DATA | MEM_READ`（带了 `MEM_DISCARDABLE` 会被链接器整个跳过，
产出没有图标的 exe）；资源目录里的偏移是 RVA，每处都要生成一条重定位。这两条都有测试盯着。

---

## 八、常见问题

**坚果云提示 503 / 同步暂停？**
免费版限制每 30 分钟 600 次请求，手机和电脑同时轮询很容易触顶。把「远端轮询间隔」调到 30 秒以上
（见第二节的配额表）；触发限流后程序会自动退避，等几分钟即可恢复。

**局域网自建 WebDAV（如 Alist、Nextcloud）连不上？**
自签证书请勾选「跳过 TLS 证书校验」；同时确认服务器地址带端口和 `/dav/` 之类的路径前缀。

**手机端能看到电脑推的内容，但反过来不行？**
先用界面右上角的「拉取」验证；再看日志里是 `304`（无变更）还是 `404`（远端还没有文件）。
若远端文件存在但内容没变，说明手机端还没把新内容写上去。

**想确认手机写过来的数据格式对不对？**
界面「运行状态」页里有「远端原始内容」，它把 `current.json` 解析后按**固定字段顺序**列出来：

```
type: text
size: 12
hash: 9f86d081884c7d65…
has_data: false
data_name: 
source: my-phone
text: 你好世界吗…后五个字
```

`text` 只留首尾各 5 个字符——完整内容上面「远端内容」那块已经展示过了，
这里只关心「头尾有没有被截断、编码有没有错」。字段缺了或结构不对，就对照第三节检查。

**复制了一大段文字，结果手机上没有？**
默认只推送 **300 字符以内**的文本。看「超长跳过」计数有没有涨、日志里有没有
`剪贴板内容 N 字符，超过上限 300 字符，未推送到远端`。需要同步更长的内容就把上限调大（0 = 不限制）。

**界面提示「对端 hash 与本地 SHA-256 不一致」怎么办？**
说明手机端用的不是 SHA-256。**不用管它**——同步判重靠的是文本内容，两端算法不同也能正常同步。
只有当你想让远端文件的 hash 与本地严格一致时，才需要把「hash 字段模式」改成「留空」并保存。

**配置界面的端口是怎么定的？**
固定 `31213`。当前地址显示在界面「程序设置 · 配置界面」里，也记在 exe 同目录的 `xime-clip-sync.ui`。
想换就把「监听端口」填成你要的端口号，**保存后立即生效**：页面会提示新地址，旧端口随即关闭。

**端口被占用了怎么办？** 程序**不会**自己换端口——地址是你要存成书签的东西，
悄悄换掉比打不开更让人困惑。端口绑不上时配置界面就起不来，启动日志会写清
「改 `config.json` 里的 `uiPort`，换一个没被占用的端口再重启」，托盘也会弹气泡说同一件事。
注意 Windows 会把整段 TCP 端口保留给 Hyper-V / WSL / Docker
（`netsh int ipv4 show excludedportrange protocol=tcp` 能看），落到保留段里的端口同样绑不上。

**保存后有些设置没生效？**
保存配置时程序还会顺带做三件事：写开机自启项、切日志文件、重绑配置界面端口。
任何一件失败都**不会**让保存整体失败（其余设置照常生效），但会拼成一条提示浮在按钮下方，
比如「配置已保存，但日志落盘设置失败：…；端口 50750 用不了，界面仍留在 http://127.0.0.1:31213/」。

多条一起报，不是只报第一条：一次保存完全可能同时踩中两件事，
早先只留第一条的写法会让另一件事**静默失效**——设置看起来存上了、实际没生效，比直接报错更难查。

**托盘图标看起来有点糊？**
先看日志里的那两行（见第七节）。如果显示 `16×16（系统要求 16×16）`，说明你的显示缩放是 100%，
本来就不该糊；要是仍然糊，把这两行日志发出来。

**配置文件在哪？为什么不做 `%AppData%` 回退？**
固定是 `xime-clip-sync.exe` 同目录的 `config.json`，日志在其同目录的 `logs/` 下——整个文件夹拷到 U 盘就能带走。
不做回退是因为那会让「配置到底在哪」变成一个需要猜的问题，所以这里选择**明确失败**：
exe 目录不可写时程序照常运行，但会在日志里告警，而不是悄悄换一个地方存。想放到别处用 `-config <path>`。

首次运行会自动生成 `config.json`，所以「文件存在」等价于「跑过一次」。
反过来说，如果**配置文件不见了、但 `logs/` 里还有日志**，那就不是首次运行——程序会在日志里明确告警
「配置文件不存在，已按默认值重新生成一份」，提示你原来的配置是被删除或移走了，而不是设置自己丢了。

**密码安全吗？**
`config.json` 里只有 DPAPI 密文，没有明文密码。但它**只挡得住「文件被拷走」**：同一个 Windows 账号下
运行的任何程序都能调用 DPAPI 解开它。防的是配置被随手发出去、被同步网盘备份走这类场景，不是防本机木马。

---

## 九、文件结构

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

- **`build/`** —— exe 的输出目录。程序运行时产生的 `config.json`、`logs/`、`xime-clip-sync.ui`
  也都在 exe 旁边，也就是这个目录里。整个目录已 gitignore。
- **`rsrc_windows_amd64.syso`** —— 必须留在源码根，`go build` 才会自动链接进 exe，所以单独 gitignore。

## 十、测试

```bash
go test ./...          # 全部 91 个
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
| 旧配置升级不丢上限 | `TestConfigUpgradeFromBytesLimit` |
| 日志轮转 / 单文件上限 / 保留期 | `TestLogFile`、`TestLogFileSizeCap`、`TestLogFileSizeUnlimited`、`TestLogRetention` |
| 保留期清理不碰用户自己的文件 | `TestLogRetentionSkipsFilesWithoutDate`（前缀像日志但日期段不是日期的一律留着） |
| 单文件上限的取值范围与换算 | `TestLogMaxFileMBBounds`（默认 2 MB；0 = 不限制；负数退回默认；过大收敛） |
| 旧配置不丢单文件上限 | `TestConfigUpgradeKeepsLogFileCap`（缺 `logMaxFileMB` 时落到 2 而不是 0） |
| 落盘不留明文密码 | `TestSaveConfigEncryptsPassword`、`TestLoadConfigMigratesPlaintextPassword` |
| 界面不泄露密码 | `TestAPIConfigNeverReturnsPassword`、`TestAPISaveWithBlankPasswordKeepsStored` |
| 「远程路径」留空落到默认文件 | `TestRemotePathDefaultsToXimeWhenBlank`、`TestWebDAVClientUsesDefaultRemotePath` |
| 留空不被写回配置 | `TestRemotePathBlankIsNotWrittenBack`（配置里保持用户原样） |
| 填了值不再自动补文件名 | `TestRemotePathUserValueIsKept`（`xime` 就是「叫 xime 的文件」） |
| 纯函数只做拼接 | `TestBuildFileURL`（留空 = 服务器根；**别**再往里塞默认值） |
| 不做旧格式识别，远程路径原样保留 | `TestLoadConfigKeepsRemotePathAsIs`（像目录的值也不再补全） |
| 「远端原始内容」字段顺序 + 首尾截断 | `TestRemoteRawText`、`TestRemoteRawTextNilFields`、`TestHeadTail` |
| 内容预览 150 字符截断 | `TestStatusPreviewTruncatesAt150` |
| 配置固定在 exe 目录 | `TestConfigPathIsExeDir`（防止哪天又被改回 `%AppData%` 回退） |
| 启动不重复拉取 | `TestNoRedundantStartupFetch` |
| 托盘单击无响应、双击开界面 | `TestTrayClickAction`、`TestTrayWindowClassHasDblClks` |
| 托盘菜单排版与文案（顶部一行状态 + 两条分隔线、无同步开关项） | `TestTrayMenuLayoutOrder`、`TestTrayMenuStatusItemsOnTop`、`TestTrayMenuIDsAreDistinct`、`TestTrayMenuHasNoToggleItem`、`TestTrayMenuLabels` |
| 菜单状态行取自当前状态、不写死 | `TestTrayMenuStatusSource` |
| 状态行只做二选一（正常 / 异常） | `TestTrayMenuSyncLabel` |
| 状态行文案写死、够短（≤14 字）、且没有占位符字面量 | `TestTraySyncTextsAreShort` |
| 每个配置字段都在界面 FIELDS 里（漏了会静默丢设置） | `TestEveryConfigFieldIsInTheForm` |
| 「程序设置」页每个输入框的 id 都是配置字段名（写错会让「恢复默认值」静默跳过它） | `TestAppPageFieldsAreConfigFields` |
| 「恢复默认值」按钮在本页里、且本页没有任何被排除在恢复之外的项 | `TestRestoreButtonScope` |
| 默认值接口回的是**出厂默认**（不是当前配置）、且不含密码材料 | `TestDefaultsEndpointReturnsFactoryDefaults` |
| 失败原因分类（端口冲突 vs 未运行） | `TestUIFailReasonOf`、`TestUIFailReasonOnRealBindFailure`（真去绑一个被系统保留的端口） |
| 「起过又没了」要能和「在跑」区分 | `TestUIServerFailedOnlyOnUnexpectedExit`（换端口关掉旧监听器不算失败） |
| 打开配置界面失败会弹气泡 | `TestOpenUIReportsFailureOnBalloon`、`TestOpenUISuccessStaysSilent` |
| 菜单命令分发（含未知 ID 必须无反应） | `TestTrayHandleCommandDispatches` |
| 托盘状态词够短（≤4 字、无标点） | `TestTrayStatusLabelIsShort` |
| 图标挑档宁大勿小 | `TestPickICOPreference`、`TestLogoICOHasSizesNeededByDPI` |
| 端口：固定默认 `31213`，配置里永远存一个具体端口号 | `TestUIPortNormalize`、`TestLoadConfigUsesDefaultUIPortWhenMissing`、`TestUIPortUserChoiceIsKept`、`TestLoadConfigGeneratesFileOnFirstRun` |
| 端口被占用时**直接报错、不换端口**（日志与气泡告诉你改 `uiPort`） | `TestListenFailsWhenPortTaken`、`TestStartFailsWhenPortTaken`、`TestListenRejectsIllegalPort` |
| 端口运行时切换：失败要留在原端口，不能把用户关在门外 | `TestUIServerRebindSwitchesPort`、`TestUIServerRebindKeepsOldPortOnFailure`、`TestUIServerStartAndURLFile`、`TestSaveWithUnavailablePortWarnsAndKeepsUI` |
| 推送/拉取日志要短（全程序最高频的一行，直接决定日志文件增速） | `TestSyncLogLinesAreShort` |
| 配置不见了要告警（而不是静默重置） | `TestLoadConfigWarnsWhenConfigDisappeared`、`TestLoadConfigNoWarnOnGenuineFirstRun` |
| 保存时附带动作失败要**全部**报出来（不是只报第一条） | `TestWarningsCollectAllMessages`、`TestSaveReportsEveryWarningNotJustFirst` |
| 换端口后引导页不能把提示吞掉 | `TestSwitchedPageKeepsWarning` |
| exe 内图标与源文件一致 | `TestSYSOIconMatchesLogoICO`（逐字节比对，守住「换了 `img/logo.ico` 却忘了重新生成 `.syso`」） |

## 十一、已知限制

- **只支持 Windows**。剪贴板、托盘、自启三处都是 Windows 原生实现。
- 只同步**文本**，不同步图片/文件（与插件能力一致）。
- 不做增量或历史记录，远端只保留最新一条。
- 没有剪贴板历史与快捷键面板，只做「当前剪贴板」的双向同步。
- 程序靠轮询工作，不是即时推送：本地变化约 1 秒内推送，远端变化取决于「远端轮询间隔」。
- 单实例锁是**按配置目录**的。用不同配置目录启动两个副本不会被拦住，
  但也不该这么用——两个副本同步同一个剪贴板只会互相覆盖。
