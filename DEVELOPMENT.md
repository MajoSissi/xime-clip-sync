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

当前产物：`build/xime-clip-sync.exe`，v1.5.0，仅 Windows/amd64，9,228,288 字节（v1.4.0 起引入
`image/png` 等标准库包做图片编解码，比只同步文本的 v1.3.0 大约 +0.6 MB；v1.5.0 只是配置、
目录准备、错误信息、日志分通道与「运行状态」页的调整，体积基本没变）。

**换图标只需替换 `img/logo.ico` 后重新构建**，不用改任何代码。

### CI 与发版（GitHub Actions）

`.github/workflows/build.yml`，跑在 `windows-latest` 上，步骤与 `build.sh` 完全同序，只是测试加 `-short`：

```bash
go test ./... -short   # 跳过真的读写剪贴板 / 改注册表的用例：CI 是无交互会话，跑不了
go run genico.go icon.go
go build -trimpath -ldflags "-s -w -H=windowsgui" -o build/xime-clip-sync.exe .
```

**只手动触发**（Actions 页面 → build → Run workflow），不挂 `push` / `pull_request`——什么时候构建由人决定。
唯一的输入是 `tag`：

| `tag` 输入 | 行为 |
|-----------|------|
| 留空 | 测试 + 构建，exe 与 `SHA256SUMS.txt` 作为 artifact 上传 |
| 填 `v1.5.0` | 上面那些，再加一步发 Release（exe + `SHA256SUMS.txt`，changelog 自动生成） |

发版：先把 `main.go` 里的 `version` 改成 `1.5.0` 提交，然后 Actions 里跑一次、`tag` 填 `v1.5.0`。

⚠️ `tag` 与 `main.go` 里的 `version` **必须一致**。`genico.go` 是靠正则从 `main.go` 抠版本号写进 exe 的
`RT_VERSION` 资源，不一致的话 Release 页面写着 v1.5.0、exe 属性里却是旧版本号。workflow 里有一道
`if [ "$src" != "$want" ]` 的检查挡着，不一致直接失败。

#### 为什么 CI 固定用 1.23，以及「本机绿 ≠ CI 绿」

CI 跑**最低支持版本**（`go-version: "1.23"`），开发机上是更新的版本，两者在 `net/http` 上并不等价。
已经踩过一次：测试里 `ui.URL()+"/api/config/save"` 拼出了双斜杠 `//api/config/save`，
`ServeMux` 判定「路径不干净」会跳转到清洗后的路径，而**跳转状态码随 Go 版本变过**：

| Go 版本 | 双斜杠跳转 | `http.Client` 跟随时的行为 |
|---------|-----------|--------------------------|
| 1.23 ~ 1.25 | `301` | **POST 被降级成 GET** |
| 1.26 起 | `307` | 方法和 body 原样保留 |

接口只认 POST，于是同一个测试在 1.23 上收到 405、在 1.26+ 上是 200——CI 红、本机绿。
修法不是改版本，而是别拼出双斜杠：测试统一用 `apiURL(ui, path)` 这个 helper（`uiserver_test.go`），
它会把 `URL()` 结尾那个 `/` 去掉再拼。

教训：**接口地址的拼接不要手写**；另外改完测试至少跑一次 1.23 的用例：

```bash
GOTOOLCHAIN=go1.23.12 go test ./... -short   # 用 CI 那个版本本地验一遍
```

---

## 二、命令行参数

面向用户的参数表见 [README.md](README.md#命令行)。这里只记开发专用的一个：

```
-tray-test       开发用：自检托盘菜单命令分发后退出
```

`-tray-test` 和正常运行一样把过程写进 `logs/`，但**会真的执行推送/拉取**，别对着用户的正式配置跑。

程序是**无控制台**构建，标准输出不会出现在任何地方，所有参数只通过**退出码**（0 成功 / 1 失败）表达结果。

---

## 三、与输入法插件的协议对应

协议对齐插件的 `main.ts` 与 `libs/dav-url.ts`：

| 环节 | 插件（main.ts） | 本项目 |
|------|----------------|--------|
| 文件路径 | `{davUrl}/{remotePath}/clipboard/current.json` | `{davUrl}/{remotePath}`（`remotePath` 具体到文件） |
| 附件路径 | `{davUrl}/{remotePath}/clipboard/blobs/<data_name>` | 同左（`{剪贴板文件所在目录}/blobs/<data_name>`） |
| 写入 | `PUT` + `Content-Type: application/json` | 同左 |
| 附件写入 | 先 `PUT` blob、再 `PUT` JSON（顺序不能反） | 同左 |
| 读取 | `GET` + `If-None-Match`，304 视为无变更 | 同左 |
| 目录 | PUT 返回 409/404 → 逐级 `MKCOL` → 重试一次 | 同左，**多兜一个 403**；且在「测试连接」时就把目录建好（见下） |
| 认证 | `Authorization: Basic base64(user:pass)`，用户名为空则不带 | 同左 |
| 连接测试 | `PROPFIND` + `Depth: 0`（部分服务对 HEAD 返回 503） | 同左 |
| 限流 | 503 进入退避，避免延长封禁 | 拉取退避 10 分钟 / 推送退避 3 分钟 |
| 字段 | `type / hash / text / has_data / data_name / size / source`（snake_case） | 同左 |

> **⚠️ 两端 `remotePath` 语义不同（尚未统一）。** 插件把它当**目录**（文件名由插件固定追加，
> 留空 = 服务器根目录）；本项目把它当**完整文件路径**（留空 = `xime/clipboard/current.json`）。
> 两边都留空会指向不同文件。要同步同一文件：插件填 `Android/xime` → 本项目填 `Android/xime/clipboard/current.json`。

`size` 是文本的 UTF-8 字节长度，与插件的 `new TextEncoder().encode(text).length` 一致；
**图片记录**的 `size` 是 PNG 附件的字节数（也就是真正写进远端的体积）。

### 上传失败时要说清「哪儿失败了」

`webdavClient.doWithBody` 会把非 2xx 的响应体**读回一小段**（`errBodyMax` 400 字符，
`compactSnippet` 压成单行、尖括号变空格），拼进错误信息：

```
上传附件失败（HTTP 403）：服务器拒绝写入：可能是父目录不存在且未能自动创建、账号无写入权限，
或服务器策略限制；服务器返回：D:error D:quota-exceeded /D:error
```

早先这里直接 `io.Discard`，用户只能看到一个状态码，没法判断是程序问题还是远端问题。
`statusHint` 给 401/403/409/413/429/507 各补了一句人话，认不出的状态码返回空串（不硬编解释）。

403 被 `isMissingParentStatus` 一并当作「父目录可能不存在」处理：先逐级 `MKCOL` 再重试一次。
**这不会掩盖真问题**——目录建不起来时 `putWithDirRetry` 会把原始状态和 `MKCOL` 的失败
一起报出来，用户从错误里就能看出是「没建目录」还是「服务器不让写」。

### 目录在配置阶段建好，不靠每次同步去猜

上面那条 403 兜底是**安全网**，不是主路径。主路径是：**「测试连接」时一次性把远端目录建好**
（`TestConnection` → `webdavClient.ensureRemoteLayout`）。

```
{davUrl}/{远程路径所在目录}            ← 剪贴板文件的家
{davUrl}/{远程路径所在目录}/blobs      ← 图片附件（见 blobsDirURL）
```

这么安排的理由：目录创建是**被动触发**的（只在 PUT 失败后才补建），因此必须先猜中
「服务器用哪个状态码表示父目录不存在」——409 是标准，404 见 Alist/Nextcloud，
403 见部分坚果云，猜漏一个就卡住。把建目录挪到配置阶段，就彻底不依赖这个猜测了：

- 这是用户配置连接时唯一会主动触发的动作，做一次就够，不占同步节奏的请求额度；
- 幂等：目录已存在时 `MKCOL` 返回 405，被当作成功，反复点「测试连接」无副作用；
- `ensureDirURL` 按**目录 URL** 逐级建（`relativePathParts`），`ensureDirectories` 则先
  `buildDirURL` 剥掉文件名再交给它——两者共用同一段循环，不会各说各话。

> 注意 `ensureRemoteLayout` 会对 `xime`、`xime/clipboard` 各 MKCOL 两次（建剪贴板目录时
> 走一遍、建 blobs 时又路过一遍）。这是**故意**的冗余：两行代码各自表达了完整意图，
> 将来布局改了也不会因为「以为父目录已经建过」而漏建。代价是配置时多两个请求，可以接受。

### 图片（附件）协议

- 图片统一转成 **PNG** 再传（`data_name = <sha256>.png`，内容寻址 ⇒ 重复上传幂等）。
- 插件端的 `BLOB_NAME_PATTERN` 只认 `[A-Za-z0-9][A-Za-z0-9._-]*`，
  本项目 `davurl.go` 的 `blobNamePattern` 用同一套规则**先校验再拼路径**——
  附件名来自远端 JSON，直接拼接就是路径穿越的入口。
- 图片记录**没有 text 值**（本项目写成空串，插件则整个省略该键）。
  因此 `decodeProfile` 判定「是不是 Profile」看的是 `text` / `has_data` / `type` **任一**字段存在，
  只认 `text` 键的老写法会把图片 JSON 当纯文本、把整段 JSON 写回剪贴板（见 `TestDecodeImageProfile`）。

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

### 图片（与文本并列的一套状态）

文本用 `current` 判重，图片不能——图片记录的 `text` 恒为空串，拿它比的话任何一次本地图片变更
都会被误判成「和远端一致」而漏推。所以另有一套状态：

| 字段 | 含义 |
|------|------|
| `currentKind` | 当前一致的是哪一种：`""` / `text` / `image` |
| `currentImageHash` | 一致的那张图的**内容哈希**（对下载/上传的 PNG 字节算 SHA-256） |
| `currentImageSize` | 那张图的字节数（界面显示体积用） |
| `currentImagePNG` | 那张图的 PNG 字节，**只为了界面能把图画出来**（见 `/api/image`） |
| `remoteKind` / `remoteImageSize` | 远端那边是什么（界面展示 + 判断要不要下附件） |
| `remoteImagePNG` / `remoteImageHash` | 远端那张图的字节与它**本地算的**哈希；只有真拿到字节后才有值 |

`Status` 里对应暴露 `currentImageHash` / `remoteImageHash`（界面取原图的内容地址）与
`currentTextChars` / `remoteTextChars`（**未截断**的字符数）。字符数不能由 `currentText` 反推——
那是 150 字符的摘要，1000 字的文本会显示成 151，数字看着合理但是错的。

几条刻意的决定：

- **内容优先级：文本优先。** 剪贴板同时有文本和位图时（Excel / Word 复制单元格、网页复制富文本
  都会这样）按文本同步——用户复制的是一段内容，把它当图片推过去只会得到一张看不清的截图。
  只有**完全没有文本**时才走图片分支（`checkLocal` → `handleLocalImage`）。
- **不推送空文本。** 没有文本、也没有图片时直接返回。旧实现会把「复制一张图片」变成
  「推送一条空文本」，把远端记录冲掉（图片同步关掉时尤其明显）。
- **判重按下载到的字节算，不信远端 `hash` 字段。** 对端可能用别的算法，
  直接比它会让同一张图被反复写进剪贴板。写 `currentImageHash` 用的也是本地算出来的值。
- **图片上限也分自动 / 手动两个**（`maxImageMB` / `manualMaxImageMB`，单位 **MB**，0 = 不限，
  最大 50）。用 MB 而不是 KB 是因为这个数字是给用户看的，而截图常见几百 KB 到几 MB。
  上限按**推送出去的 PNG 体积**算，不是剪贴板里未压缩的位图大小——后者大得多，拿它当判据会误伤。
  超限的图不写进 `currentImage`，去重靠 `oversizeImageHash`（与文本的 `oversizeText` 同思路）。
  旧配置里的 `maxImageKB` / `manualMaxImageKB` 在 `loadConfig` 里自动换算成 MB（`kbToMB`，向上取整）。
- **拉取不设图片上限**（只受 `webdav.go` 的 `maxBlobResponseBytes` 兜底）：
  与文本一致——上限管的是「推送」，不该因为本机自动上限小就收不下对端发来的大图。
- **`markLocalSeq`**：自己往剪贴板写完内容（`SetText` / `SetImage`）后必须调一次。
  写入会推进剪贴板序号，不同步的话下个本地检查周期会把这次写入当成「用户复制了新内容」再推回远端。

### 剪贴板只占「取字节」那一小会儿（`clipboard_windows.go`）

**不变式：解码与编码一律在剪贴板锁之外做。**

这不是洁癖，是一个真实故障的根因：开着本程序时，截图工具「确认」后会卡一下，
而且刚截的图进不了 Ditto 的历史。原因就是取图时**全程占着剪贴板**——
4K 截图的 `CF_DIB` 有 30 多 MB，`dibToImage` 要再分配一份 30 MB 的位图，
`encodePNG` 还要归一化一次并 deflate，整条链路几百毫秒。这段时间里：

- 截图工具写剪贴板时卡在 `OpenClipboard` 重试上（重试耗尽就**放弃**，图根本没进剪贴板）；
- Ditto 收到 `WM_CLIPBOARDUPDATE` 后要打开剪贴板才能取数据，打不开就跳过这一条。

所以取图被拆成三步：

| 步骤 | 是否锁剪贴板 | 做什么 |
| --- | --- | --- |
| `clipboardHas(format)` | **否** | `IsClipboardFormatAvailable` 不需要打开剪贴板，用它先挑格式 |
| `clipboardRawOf(format)` | 是，只做一次内存拷贝 | 拿到原始字节立刻释放 |
| `clipboardImageToPNG(raw, fromPNG)` | **否** | DIB 解码 + PNG 编码，耗时全在这里 |

顺带的好处：**纯文本剪贴板一次都不锁**（`GetText` 也先用 `clipboardHas(cfUnicodeText)` 预判），
而复制图片时每次都会走一遍这条路。

**为什么必须有 `clipboardStats` 计数器**：Windows 的 `OpenClipboard` 对**同进程是可重入的**
（自己占着时再开一次照样成功，实测如此），所以从进程内部**问不出**「我是不是还占着剪贴板」。
`clipboardStats()` 暴露（占用层数，累计打开次数），`TestGetImageReleasesClipboardBeforeConverting`
才能在转换函数里断言「层数必须是 0」。把旧写法改回去，这条测试立刻变红（实测）。

`openClipboard` / `closeClipboard` 必须严格配对（一律 `defer closeClipboard()`）：
漏一次释放就是把剪贴板永久锁死，比慢一点严重得多。

### 界面怎么把图片画出来（`/api/image`）

「运行状态」页的「本地内容 / 远端内容」两块直接把图画出来，右上角靠右给出规模
（文本按**字符**、图片按**体积**）。图片走 `GET /api/image?side=local|remote&h=<sha256>`：

- **按内容寻址**。URL 里带的是引擎自己算的 SHA-256。同一张图 URL 不变，所以可以
  `immutable` 长缓存；图换了 URL 就换，界面不会画出一张已经过期的图。
  前端 `setShot` 只在 `h` 变化时才重设 `img.src`——status 每 2 秒推一次，
  无脑重设会让浏览器反复请求同一张图并闪一下。
- **取不到就 404**，界面退回「只显示体积 +（未取到原图）」。
  引擎只在**确实已经拿到字节**时给得出图（推送过、拉取过、或启动时读过剪贴板）。
  刻意**不**为了画一张图去现读剪贴板（会重新引入上面那个抢剪贴板的 bug），
  也**不**去现下附件（一个界面请求不该触发网络 IO）。
- 远端从图片变回文本、或只拿到元数据时，手里留的那份字节会被丢掉
  （`TestRemoteImageBytesDroppedWhenRemoteBecomesText`）——否则界面会把上一张图
  继续画在「远端内容」里，而远端此刻根本不是那张图。
- 卡片只有 300 多像素宽（`.page.status` 左列 `minmax(340px, 460px)`），缩略图看不清，
  所以图片可点：点一下开原尺寸 PNG（`/api/image` 返回的就是原图）。
- CSS 上两个细节容易踩：`max-width: 100%` 与 `max-height: 240px` **要同时给**，
  只给一个另一个方向会撑破卡片；`.shot` 的 `display: block` 特异性高于 UA 的
  `[hidden]`，必须显式补一条 `.shot[hidden] { display: none; }`，否则「没有图」时会留一块空白。

---

## 六、出网：代理与传输层重试

### 代理

`proxyMode` 四档（`system` / `http` / `socks5` / `none`），`proxyFuncForConfig` 把它翻译成
`http.Transport.Proxy` 回调；返回 nil 就是直连。默认 `system`。

| 模式 | 怎么选路 |
|------|---------|
| `system` | 读 `HKCU\...\Internet Settings` 的 `ProxyEnable` / `ProxyServer` / `ProxyOverride`（`proxy_windows.go`） |
| `http` / `socks5` | 用 `proxyUrl` 拼出 `http://host:port` / `socks5://host:port`；scheme 以**模式**为准 |
| `none` | 返回 nil，直连 |

三个刻意的决定：

- **系统代理只读注册表，不叠加 `HTTP_PROXY` 环境变量。** `http.ProxyFromEnvironment` 只认环境变量，
  而从资源管理器双击启动的 GUI 进程没有 shell 的环境变量——用它的话「系统代理」会看起来有、
  实际不生效。混用两套来源还会让同一个设置在不同启动方式下行为不同，比不生效更难查。
- **选了 `http` / `socks5` 却没填地址（或地址解析不出来）时，Proxy 回调返回错误**，
  不静默退回直连。用户选代理往往就是为了让流量走代理，悄悄直连比同步报错更糟。
- **`ProxyOverride` 必须支持「通配符在中间」**（`127.*`、`192.168.*`）。这是系统默认列表的实际写法，
  只做后缀匹配的话局域网自建 WebDAV 会被硬塞进代理直接连不上。见 `matchWildcard`。

`ProxyServer` 有两种写法都要认：裸 `host:port`（所有协议共用），
以及 `http=...;https=...`（按协议分开；没列出当前协议就直连）。

⚠️ **测试里一律 `ProxyMode = none`**（见 `testConfig`）。照抄默认值的话，这套测试会去读
**开发机自己的注册表**——本机配了系统代理时，指向 `127.0.0.1` 的 `httptest` 请求会被塞进真代理，
测试随机器而红或绿。

### 传输层重试

`webdavClient.request` 对传输层错误**立刻重试一次**，间隔 `transferRetryDelay`（500ms），
并记一条 warn（`TestTransferErrorRetriesOnce` 盯着「只重试一次」且「必须留痕」）。

**超时类错误不重试**（`transportErrIsRetriable`）。理由不是省事：`Client.Timeout` 是 30 秒，
重试会再等一个 30 秒，而这段时间 `syncMu` 被占着，连本地剪贴板的推送都跟着停摆。
这条规则同时保证了「重试」只给最坏情况增加 500 毫秒，而不是 30 秒。

> Go 自己在 `net/http` 里也会重试，但条件很窄：**只有复用连接（keep-alive）被对端关掉**时才会
> （`transport.go` 的 `shouldRetryRequest`：`if !pc.isReused() { return false }`）。
> 全新连接上的 DNS 失败、连接被拒、超时一律不重试——这正是这里要补的那一段。

---

## 七、日志实现

日志按天分文件写在 **exe 同目录的 `logs/`**：

- **跨天自动轮转**：写入时发现日期变了就切新文件，不需要重启。
- **保留期**默认 7 天（含今天）：启动时清理一次，之后每 6 小时清理一次。
- **只删自己的文件**：只认 `xime-clip-sync-YYYY-MM-DD.<kind>.log` 命名
  （`<kind>` ∈ `sync` / `app`），以及 v1.5 之前没有通道段的旧命名
  `xime-clip-sync-YYYY-MM-DD.log`——升级上来时那些文件仍会被清理。
  名字前缀像、但通道段不认识的文件（`xime-clip-sync-…-bogus.log`）一律不碰。
- **单文件上限**默认 2 MB（0 = 不限制）：达到上限后**当天不再写盘**（内存日志不受影响），
  并往文件末尾补一条 `本日日志已达 2 MB 上限，后续日志只保留在内存中`。
- `日志保留天数 = 0` 表示永久保留。

### 两条通道：同步 / 程序

`LogLine.Kind` 把日志分成两条通道，**内存与磁盘上都是分开的**：

| 通道 | `kind` | 记什么 | 文件 |
| --- | --- | --- | --- |
| 同步日志 | `sync` | 剪贴板的推/拉、连接测试、限流退避、上传下载失败 | `…-YYYY-MM-DD.sync.log` |
| 程序日志 | `app` | 配置保存、界面端口、托盘图标、开机自启 | `…-YYYY-MM-DD.app.log` |

- 写入口：`Infof/Warnf/Errorf` 走**程序**通道（默认），`SyncInfof/SyncWarnf/SyncErrorf` 走同步通道。
  默认给程序通道，是为了让 `main.go` / `ui.go` / `uiserver.go` / `autostart*` 这些调用点不必改动；
  看到 `SyncInfof` 就知道这条一定与剪贴板同步有关。
- 内存里**每条通道各一个定长环**（`LogBuffer.lines[kind]`）。不共用一个环，是因为
  共用时程序日志刷屏会把同步日志顶掉，而用户打开「运行状态」页十有八九是在查同步问题。
- 落盘时**每条通道各一个文件句柄**（`logSink`），各自按天轮转、各自按单文件上限封顶。
- 订阅（SSE）拿到的是两条通道混合的流，每行自带 `kind`。
- `/api/logs` 返回 `{"sync":[…], "app":[…]}`，两条通道都给。
- **界面上只显示同步日志。** 「运行状态」页只有一块日志面板（`#syncLogs`），
  前端 `addLog` 按 `LOG_KIND = "sync"` 过滤掉程序日志——那一页是查「剪贴板为什么没同步过去」的地方，
  程序日志混进来只会冲淡它。程序日志照常写进 `logs/` 下的 `.app.log`，需要时直接看文件。
  由 `TestStatusPageShowsOnlySyncLogs` 守着（它会拒绝 `程序日志` 面板和 `appLogs` 这两个 id 再回来）。

`logs/` 总占用大致是 **单文件上限 × 保留天数 × 2**（默认 2 MB × 7 天 × 2 ≈ 28 MB）。

**日志文案要短**：最高频的两行是 `⬆ 推送 （N 字符）` / `⬇ 拉取（N 字符，来自 <设备>）`，
由 `TestSyncLogLinesAreShort` 守着（含**反向哨兵**：出现旧长文案就报错；长度 ≤ 24 rune）。
手动推送/拉取成功**不再单独打一行**；**拉取没变化时不记**——那条路径每个轮询周期都走。
其余告警（hash 不一致、超上限）都有「只报一次」保护，所以高频行只有这两条。

---

## 八、图标与 DPI

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

生效后 `GetSystemMetrics` 返回真实尺寸，托盘图标按系统要的边长 1:1 绘制（失败才记一条 warn）。

挑档策略：正好相等 > 比目标大的里面最小的 > 最大的那一档（宁可缩小丢像素，也不放大插值发虚）。

`.syso` 装了 `RT_ICON`×8、`RT_GROUP_ICON`、`RT_VERSION` 三种资源，链接后资源管理器、任务栏、Alt-Tab
都会显示正确图标，右键「属性」也能看到版本号（从 `main.go` 解析，与 `-version` 一致）。

**为什么自己生成 `.syso` 而不用 `windres`？** 为了坚持零第三方依赖、零外部工具链。
`.syso` 就是一个简单的 COFF 对象文件（`.rsrc` 段 + 重定位 + 段符号）。两个坑：段特征位必须**恰好**是
`CNT_INITIALIZED_DATA | MEM_READ`（带 `MEM_DISCARDABLE` 会被链接器整个跳过，产出没有图标的 exe）；
资源目录里的偏移是 RVA，每处都要生成重定位。两条都有测试盯着。

### 托盘图标为什么会自己消失（以及怎么补回来）

**托盘图标不是进程画出来的，是外壳（explorer.exe）那里的一份登记。** 外壳只要重建了任务栏——
重启 explorer、从睡眠/休眠里醒过来、分辨率或缩放变化后重建——就会把所有第三方图标一起丢掉。

关键在于**进程这边收不到任何错误**：`Shell_NotifyIcon` 不报错，消息循环也没有异常，程序里没有任何
东西能「察觉图标没了」。表现出来就是**图标消失了、但程序明明还在跑**（日志在写、同步照做、双击托盘
的位置没有反应），用户只能重启程序才找得回图标。

唯一能做的是**接住外壳的通知，然后重新登记一次**。两个信号都要接（`trayWindowAction`）：

| 信号 | 什么时候来 | 只接另一个会漏的情况 |
|------|-----------|--------------------|
| `TaskbarCreated`（`RegisterWindowMessageW` 换出的消息号） | 外壳重建完任务栏后广播 | 唤醒那一瞬间外壳正在重建，这条**很可能在我们被挂起时就广播过了** |
| `WM_POWERBROADCAST` + `PBT_APMRESUMEAUTOMATIC` / `PBT_APMRESUMESUSPEND` | 系统从睡眠/休眠里醒过来 | 外壳没重建任务栏、但把图标弄丢了的场合 |

补的方式是 `NIM_DELETE` → `NIM_ADD`：外壳按 `(hWnd, uID)` 认这份登记，图标还在时直接 `NIM_ADD`
**不保证**会替换（文档没有承诺），先删一次才能保证「加」真的生效。删一个不存在的图标是无害的。

几个刻意的决定：

- **消息号注册失败（`0`）时一律不认**。`0` 就是 `WM_NULL`，消息循环里到处都是；把 `0` 当
  `TaskbarCreated` 会变成「每收到一条空消息就把图标删了又加」，任务栏里图标一直闪。见
  `TestTrayWindowAction` 里那条反向哨兵。
- **补图标必须留日志**（`已重新添加托盘图标` / 失败时带原因）。补成功这件事用户看不见——
  图标回来了就等于「什么都没发生过」，事后只能靠日志判断「图标偶尔消失」到底是外壳丢的、
  还是压根没加上。日志回调走 `trayCallbacks.Log` / `Warn`（托盘在后台，得让它自己记得了）。
- **注册失败只记一条 warn，不让托盘起不来**：只是一项自愈能力没了，不该连图标都没有。
- **隐藏窗口必须是顶层窗口**（`CreateWindowExW` 的父窗口传 `0`）。消息专用窗口（`HWND_MESSAGE`）
  **收不到广播**，那样上面这些全白搭。`TestTrayReceivesTaskbarCreatedBroadcast` 真的广播一次
  `TaskbarCreated` 来钉住这条前提——它依赖真窗口，所以 `-short` 会跳过。

---

## 九、文件结构

```
xime-clip-sync/
├── main.go                 入口、命令行参数、装配同步循环/配置界面/托盘
├── sync.go                 同步引擎（推/拉、回声抑制、限流退避、长度上限）
├── webdav.go               WebDAV 客户端（PUT/GET/PROPFIND/MKCOL、目录准备、传输层重试）
├── proxy.go                代理模式与 Proxy 回调（模式校验、代理 URL 组装）
├── proxy_windows.go        Windows：读注册表里的系统代理 + ProxyOverride 绕过匹配
├── davurl.go               远端 URL 组装（对齐插件 dav-url.ts）
├── profile.go              远端 JSON Profile 编解码 + 界面用的格式化清单
├── dib.go                  Windows 剪贴板位图（DIB）↔ 图像 ↔ PNG 的编解码（纯计算，无 build tag）
├── clipboard.go            剪贴板接口定义（文本 + 可选的序号 / 图片能力）
├── clipboard_windows.go    Windows：直接调用 user32/kernel32（文本、位图、序号）
├── tray.go                 托盘接口与菜单动作定义
├── tray_windows.go         Windows 托盘（Shell_NotifyIconW + 隐藏消息窗口 + 气泡 + 鼠标手势 + 外壳重建后补图标）
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
├── logbuf.go               日志：两条通道（同步/程序）的内存环 + 分文件落盘 + 保留期清理
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

## 十、测试

```bash
go test ./...          # 全部 158 个
go test ./... -short   # 跳过会动真实剪贴板 / 注册表 / 真窗口的测试
```

用 `httptest` 起了模拟 WebDAV 服务器，覆盖 URL 组装、wire 字段格式、纯文本兼容、目录自动创建、
Basic Auth、本地→远端、远端→本地、回声抑制、ETag 304、503 退避。

图片这一块分两层测：

- **`dib.go` 是纯计算**（`dib_test.go`，无 build tag）：造各种 DIB（24/32 位、BI_RGB / BI_BITFIELDS、
  自上而下、8 位调色板、OS/2 头）验证解码，往返验证编码，并专门钉住
  「32 位 BI_RGB 的 alpha 未定义 ⇒ 必须当不透明」——搞错这条会让截图整张变透明。
  这些用例在 CI 上照跑（CI 读不到真实剪贴板，但纯函数不需要）。
- **同步链路**用假剪贴板（实现了 `imageClipboard`）端到端跑：推图 → 远端出现 blob + JSON、
  拉图 → 写进剪贴板、同图不重复写（回声抑制）、超限不推、开关关闭时不动远端。

会真实改动本机状态的测试（结束即恢复，可用 `-short` 跳过）：

- `TestRealClipboardRoundTrip`：真的读写剪贴板，覆盖中文、emoji、`\t`/`\r\n`、1500 字长文本与空串，
  并校验剪贴板序号随写入递增。
- `TestRealClipboardImageRoundTrip`：真的把一张图写进剪贴板再读回来，逐像素比对，
  并验证「同一张图读两次字节一致」（回声抑制比的是哈希，字节不稳就会来回推）。
- `TestSetImageRejectsGarbage` / `TestSetImageRejectsEmpty`：非法/空 PNG 必须报错且**不破坏**剪贴板现有内容。
- `TestGetImageReleasesClipboardBeforeConverting`：**取图时不许占着剪贴板做解码/编码**。
  用 `clipboardStats()` 的占用层数判定——Windows 的 `OpenClipboard` 同进程可重入，
  从进程内部问不出「剪贴板是不是被自己占着」。把旧写法改回去这条测试立刻变红。
- `TestGetImageOnTextClipboardNeverLocks`：纯文本剪贴板上取图一次都不锁剪贴板（靠累计打开次数判定）。
- `TestAutostartRegistry`：真的写入并删除注册表 Run 项；检测到已有自启项会跳过，以免影响现网配置。
- `TestSingleInstanceLock`：第二个实例拿不到锁、释放后可重新获取。

几组关键约束由测试守着，改坏了会立刻变红：

| 关注点 | 测试 |
|--------|------|
| 长度上限按字符不是字节 | `TestOversizeNotPushed`（60 个汉字 = 180 字节，在 100 字符上限内**要**推送） |
| 手动推送能突破自动上限、但受「手动推送上限」约束（两个上限互不影响） | `TestManualPushBypassesAutoLimit`、`TestManualImagePushBypassesAutoLimit` |
| 图片上限按 MB 换算、0 = 不限制、负数归零、过大收敛到硬上限 | `TestImageLimitUnitIsMB`、`TestImageConfigDefaultsAndBounds` |
| 旧配置的 `maxImageKB` 自动换算成 MB 并落盘（不悄悄回默认） | `TestLegacyImageKBConfigMigratesToMB`、`TestKBToMBBoundaries` |
| 父目录不存在时服务器给 403（而非 409/404）也能自动建目录重试成功 | `TestBlobUploadRecoversFrom403MissingDir` |
| 上传失败的错误信息带上服务端响应体、且 XML 尖括号被压平 | `TestUploadErrorCarriesServerMessage` |
| 「测试连接」把远端目录建好（剪贴板目录 + blobs），且不写任何文件 | `TestTestConnectionPreparesRemoteDirs` |
| 「测试连接」幂等（目录已存在不算错）、目录建不起来时如实报错 | `TestTestConnectionIsIdempotent`、`TestTestConnectionReportsDirFailure` |
| 附件目录 URL 与附件 URL 前缀一致（「建目录」和「写附件」不能各说各话） | `TestBlobsDirURL` |
| 按目录 URL 逐级拆分（`relativePathParts`）与按文件 URL 拆分（`relativeDirParts`）差一层 | `TestRelativePathParts` |
| 图片硬上限必须小于附件读取上限（否则推得上去、拉不回来） | `TestImageConfigDefaultsAndBounds` |
| 超长内容不显示在「本地内容」里、也不会被远端轮询冲掉剪贴板 | `TestOversizeNotShownAsCurrentText` |
| 图片记录（无 text 键）不能被当成纯文本写回本地 | `TestDecodeImageProfile` |
| 图片 wire 格式：内容寻址 `<sha256>.png`、has_data、size = PNG 字节数 | `TestImageProfileWireFormat` |
| 附件 URL 组装 + 路径穿越/非法名被拒 | `TestBuildBlobURL`、`TestContentTypeOf` |
| 图片推送**先 blob 后 JSON**（顺序反了会留下悬空引用） | `TestPushImageToRemote` |
| 远端图片能落进剪贴板；同一张图不重复写（回声抑制） | `TestPullImageToClipboard` |
| 坏附件不写剪贴板、且留下错误 | `TestPullCorruptBlobIsRejected` |
| 超限图片不推送并计数；图片同步关闭时不动远端、也不写剪贴板 | `TestImageOversizeNotPushed`、`TestImageSyncDisabledLeavesRemoteAlone`、`TestManualPushImageWhenDisabled` |
| 32 位 BI_RGB 的 alpha 未定义 ⇒ 必须当不透明（否则截图整张透明） | `TestDibToImageRGB32AlphaIgnored` |
| DIB 解码覆盖 24/32 位、位域、自上而下、8 位调色板、OS/2 头；坏数据一律报错 | `TestDibToImageRGB24`、`TestDibToImageBitfieldsKeepsAlpha`、`TestDibToImageTopDown`、`TestDibToImagePalette8`、`TestDibToImageRejectsBadInput` |
| PNG 编码确定性（同像素 → 同字节，回声抑制与内容寻址都依赖它） | `TestEncodePNGIsDeterministic`、`TestDibRoundTrip` |
| 图片日志也要短（≤ 24 字） | `TestImageLogLinesAreShort` |
| 「本地检查间隔」的单位是秒（只改文案忘了改倍数会静默变成高频轮询） | `TestLocalPollIntervalIsSeconds` |
| 旧配置升级不丢上限 | `TestConfigUpgradeFromBytesLimit` |
| 日志分文件落盘 / 单文件上限 / 保留期 | `TestLogFile`、`TestLogFileSizeCap`、`TestLogFileSizeUnlimited`、`TestLogRetention` |
| 保留期清理不碰用户自己的文件 | `TestLogRetentionSkipsFilesWithoutDate` |
| 旧命名（无通道段）的日志也会被清理 | `TestLogRetentionCleansLegacyNames` |
| 同步日志与程序日志分属两条通道 | `TestSyncLogsStayOnSyncChannel` |
| 两条通道的内存环互不挤占 | `TestLogRingsAreIndependent` |
| 单文件上限是每条通道各自的 | `TestLogFileCapIsPerChannel` |
| 日志文件名的通道段解析（含旧命名兼容） | `TestParseLogFileName` |
| 「运行状态」页只有同步日志，且前端按 kind 过滤掉程序日志 | `TestStatusPageShowsOnlySyncLogs` |
| 单文件上限的取值范围与换算 | `TestLogMaxFileMBBounds` |
| 旧配置不丢单文件上限 | `TestConfigUpgradeKeepsLogFileCap` |
| 落盘不留明文密码 | `TestSaveConfigEncryptsPassword`、`TestLoadConfigMigratesPlaintextPassword` |
| 界面不泄露密码 | `TestAPIConfigNeverReturnsPassword`、`TestAPISaveWithBlankPasswordKeepsStored` |
| 「远程路径」留空落到默认文件 | `TestRemotePathDefaultsToXimeWhenBlank`、`TestWebDAVClientUsesDefaultRemotePath` |
| 留空不被写回配置 | `TestRemotePathBlankIsNotWrittenBack` |
| 填了值不再自动补文件名 | `TestRemotePathUserValueIsKept` |
| 纯函数只做拼接 | `TestBuildFileURL` |
| 不做旧格式识别，远程路径原样保留 | `TestLoadConfigKeepsRemotePathAsIs` |
| 内容预览 150 字符截断 | `TestStatusPreviewTruncatesAt150` |
| 「本地/远端内容」右上角的字符数是**完整**长度（不是 150 字符摘要的长度） | `TestStatusReportsFullTextChars` |
| 只有手里真有字节才给得出图（不现读剪贴板、不现下附件） | `TestImagePNGOnlyWhenBytesAreHeld` |
| 远端从图片变回文本时丢掉旧字节（否则界面画出过期的图） | `TestRemoteImageBytesDroppedWhenRemoteBecomesText` |
| `/api/image`：有图 200 + `image/png`，没图与未知 side 一律 404 | `TestImageEndpoint` |
| 「运行状态」页的「内容」两块：规模靠右、图片双向约束、按哈希去重、可点开原图 | `TestStatusPageContentPanels` |
| 「程序设置」三张卡片按内容撑高（不许再用跨行 + 拉伸去凑两列） | `TestAppPageCardsSizeToContent` |
| 配置固定在 exe 目录 | `TestConfigPathIsExeDir` |
| 启动不重复拉取 | `TestNoRedundantStartupFetch` |
| 托盘单击无响应、双击开界面 | `TestTrayClickAction`、`TestTrayWindowClassHasDblClks` |
| 外壳重建任务栏 / 睡眠唤醒后补回图标（两类信号都认，消息号注册失败时不认 `0` 号消息） | `TestTrayWindowAction`、`TestTrayRestoresIconOnShellRebuild`、`TestTrayRestoreFailureIsLoggedAsWarning`、`TestTrayRestoreWorksWithoutCallbacks` |
| 隐藏窗口**真的**收得到外壳广播（必须是顶层窗口，不能是消息专用窗口） | `TestTrayReceivesTaskbarCreatedBroadcast`（`-short` 跳过，要真窗口） |
| 托盘菜单排版与文案（顶部一行状态 + 两条分隔线、无同步开关项） | `TestTrayMenuLayoutOrder`、`TestTrayMenuStatusItemsOnTop`、`TestTrayMenuIDsAreDistinct`、`TestTrayMenuHasNoToggleItem`、`TestTrayMenuLabels` |
| 菜单状态行取自当前状态、不写死 | `TestTrayMenuStatusSource` |
| 状态行只做二选一（正常 / 异常） | `TestTrayMenuSyncLabel` |
| 状态行文案写死、够短（≤14 字）、无占位符字面量 | `TestTraySyncTextsAreShort` |
| 每个配置字段都在界面 FIELDS 里（漏了会静默丢设置） | `TestEveryConfigFieldIsInTheForm` |
| 代理：模式校验、代理 URL 组装、选了代理没填地址要报错（不静默直连） | `TestProxyModeDefault`、`TestProxyModeNormalize`、`TestProxyURLFromConfig`、`TestProxyWithoutAddressFailsLoudly`、`TestProxyWithUnparsableAddressFails`、`TestProxyModeNoneIsDirect`、`TestManualProxyReachesTransport`、`TestSystemProxyFuncIsWired` |
| 请求**真的**经过代理 / 「不代理」时真的不过代理 | `TestHTTPProxyIsActuallyUsed`、`TestProxyModeNoneBypassesProxy` |
| 系统代理：`ProxyServer` 两种写法、`ProxyOverride` 通配符绕过 | `TestSystemProxyURL`、`TestBypassProxy`、`TestBypassProxyRealWorldList`、`TestReadSystemProxyDoesNotPanic` |
| 传输层错误重试一次；超时类不重试（否则 30 秒阻塞翻倍） | `TestTransportErrIsRetriable`、`TestTransferErrorRetriesOnce`、`TestTimeoutIsNotRetried` |
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

## 十一、改配置项时的三处平行清单

Go 结构体 / 前端 `FIELDS` 数组 / 页面 `<input id>` 三处必须同步。**漏了 `FIELDS` → 每次保存静默丢掉用户填的值**，
配 `TestEveryConfigFieldIsInTheForm` 守着。

改「单位」时（毫秒→秒这种）额外要改：字段名 · `defaultConfig()` · `normalize()` 的钳制 ·
**引擎里的换算倍数** · `<label>` · `min/max` · 前端 `FIELDS`/`NUMS` · 测试里手写的 JSON body · README。
**漏换算倍数最阴**：配置写 5 会变成 5 毫秒轮询，界面却显示「5 秒」，看不出任何异常。

删配置项时的清单比「删字段」长得多：字段 · 常量 · `FIELDS`/`NUMS`/`BOOLS` · `<select>` · `normalize()` 分支 ·
函数签名 · 调用点 · 测试 · 文档 · **界面与日志里「引导用户去改它」的文案**。
