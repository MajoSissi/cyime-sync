# AGENTS.md — CyIME Sync (Windows)

> 本文件是唯一的工程约束来源。代码、示例、口头说明与本文件冲突时，以本文件为准。改约束必须与实现同一提交内改本文件。

## 0. 交付范围

一个 Windows 独立小程序（不是输入法组件、不是驱动、不是 Windows 服务）：

- Go 编写，`CGO_ENABLED=0`，产出单文件 `exe`，无运行时依赖，无需管理员权限，无安装器。
- 通知区域常驻托盘图标；**双击托盘**用默认浏览器打开内置 Web 设置页。
- 内置静态 Web 设置页（Material 3 风格），由进程内 `net/http` 提供；不引入 WebView、Electron、前端构建链、npm。
- 实现 CyIME 剪贴板同步协议的**电脑端（服务端）**，双向同步纯文本：扫码配对 + TLS + 逐设备令牌（Android 原生"手机与电脑互联"）。
- **只实现当前一代协议**。上一代明文 + Basic + 手填配对码的做法已彻底移除，不保留兼容分支。

参考实现（只读对照，不复制进本仓库）：`https://github.com/Cyletix/CyIME/tree/main/app/src/main/java/com/kingzcheung/xime/clipboard/sync`（`PairingQrDecoder.kt`、`WindowsPairingProtocol.kt`、`WindowsDevices.kt`、`WindowsSecureTransport.kt`、`DeviceHost.kt`）。手机端行为是既有事实，服务端必须适配它，不允许反过来要求手机端改协议。

## 1. 技术选型硬约束

| 项 | 约束 | 理由 |
|---|---|---|
| 语言 | Go（当前工具链 1.27.x，`go.mod` 声明实际使用的最低版本） | 单文件交付、交叉编译稳定 |
| CGO | 始终 `CGO_ENABLED=0` | 无 gcc 环境、无 MSVC 依赖 |
| 第三方依赖 | 仅 `golang.org/x/sys/windows`（`golang.org/x/sys` 一个模块）、`github.com/skip2/go-qrcode`（纯 Go、零传递依赖，生成配对二维码） | 其余 Win32 调用用 `syscall.NewLazyDLL` + `NewProc` 自行绑定；二维码不手写编码器 |
| 托盘 / 窗口 | 自绘：注册窗口类 + `Shell_NotifyIconW` + `TrackPopupMenu` | 避免引入 systray 类库带来的 CGO/单文件/DPI 问题 |
| HTTP | 标准库 `net/http` | 协议简单，够用 |
| 前端 | 纯静态 HTML/CSS/JS，`go:embed` 打包 | 单文件、离线可用 |
| 数据存储 | 单个 JSON 配置文件，位于 **exe 同级 `data/`** | 绿色部署，可读、可手改、无数据库 |
| 日志 | 自写按大小轮转的文件写入器（`internal/logging`） | 不引日志框架；见 §7 |

禁止：引入 CGO 依赖、引入需要 Node/Rust/Python 的构建步骤、引入 WebView2/Electron/Tauri、引入 ORM 或数据库、引入日志框架以外的重型依赖。

## 2. 目录布局（新建代码按此放置）

```
main.go              // 入口：数据目录、日志、单实例锁、配置加载、托盘、HTTP 服务、同步引擎启动
internal/appdir/     // data/ 定位（exe 同级，不可写时退化）、可写探测、旧版数据一次性迁移
internal/logging/    // 按大小轮转的日志写入器 + 超期清理 + 后台 janitor
internal/config/     // 配置结构、读写、原子落盘、迁移
internal/clipboard/  // Win32 剪贴板读写（序列号、重试、UTF-8/UTF-16 转换）
internal/protocol/   // profile 编解码、校验、ETag、错误码映射
internal/pairing/    // 配对：自签证书、限时窗口、逐设备令牌、紧凑二维码编码
internal/syncer/     // 双向同步引擎：变化检测、去重、冲突、事件流
internal/server/     // /api/clipboard、/api/settings、/api/status、SSE
internal/tray/       // 窗口类、消息循环、托盘图标、菜单
internal/winutil/    // 懒加载 Win32 绑定、DPI、自启动、本机地址
web/                 // 静态页面（go:embed），index.html + md3.css + app.js + icon-64/180.png
img/                 // 品牌图标母版 logo-512x512.png 与 logo.ico（唯一图标来源）
rsrc_windows_amd64.syso // 由图标生成，供 Go 链接器写入 exe 的 .rsrc（文件图标）
```

单文件超过 500 行必须拆分；`internal/*` 之间禁止循环依赖；`main.go` 只做装配。

## 3. 协议契约（扫码配对 + TLS + 逐设备令牌）

一个监听端口同时服务两种连接：本机浏览器打开设置页走明文 HTTP，手机走 HTTPS。服务端读连接首字节，`0x16`（TLS 握手）走 TLS，其余按明文 HTTP 处理。

### 3.1 配对

协议权威来源是手机端 Kotlin 实现。字段名、字节布局、状态码必须逐项一致，否则手机端提示"不是 CyIME 配对码"或连不上。

- 证书：ECDSA P-256 自签，持久化在 `data/tls/cert.pem|key.pem`；**重启不得更换**（指纹变化等于换了一台电脑，用户必须重新扫码）。指纹 = 叶证书 DER 的 SHA-256 小写十六进制，手机端按此固定信任并忽略主机名。
- 二维码接口：`GET /api/qr.png?token=<本机令牌>`。二维码内含一次性配对密钥，等价于"授权扫码者读写剪贴板"，**禁止匿名获取**，也必须校验来源为本机。
- 二维码载荷：`cyime://pair?v=2&c=` + base64url(无填充)。解码后字节布局固定为

  ```
  [0]      版本 = 2
  [1]      地址个数 n（1..8）
  n × 6B   4 字节 IPv4 + 2 字节端口（大端）
  16B      server_id（UUID 原始字节）
  32B      证书 DER 的 SHA-256
  16B      pairing_id（UUID 原始字节）
  32B      配对密钥
  8B       过期时间（unix 秒，大端）
  ```

  总长必须是 `106 + 6n`。地址必须是 `https://<IPv4>:<端口>`，私有/回环地址，不得出现域名或 `0.0.0.0`。
- 配对窗口 2 分钟，**未过期时复用同一窗口**：自动刷新二维码（页面倒计时归零、改监听地址、配对成功后取下一张）不得让正在扫描的码失效。
- 设置页的「换一张」是用户主动动作，走 `POST /api/pairing?rotate=1`：立刻作废旧 `pairing_id`/`secret` 并开新窗口，旧码扫码得到 `410`。不带 `rotate=1` 的 `POST` 仍复用窗口，不得轮换。
- `POST /api/pair`（仅 TLS，明文请求一律 `400`）请求体 `{"v":2,"pairing_id","secret","client_id","client_name","client_kind"}`，成功 `201`：

  ```json
  {"v":2,"server_id":"<uuid>","server_name":"<电脑名>","device_id":"<uuid>","access_token":"<43 字符 base64url>"}
  ```

  | 状态 | 触发条件 |
  |---|---|
  | `201` | 配对成功，窗口随即消费 |
  | `400` | 非 JSON / `v`≠2 / `pairing_id` 不符 / `client_id` 非 UUID / 非 TLS |
  | `401` | `secret` 不匹配（连续 10 次关闭窗口） |
  | `409` | 已配对设备达到 8 台上限 |
  | `410` | 没有打开或已过期的配对窗口 |
  | `413` | 请求体 > 4096 字节 |

  同一 `client_id` 重新配对沿用原 `device_id` 并**轮换令牌**，旧令牌立即失效。
- 认证：`Authorization: Bearer <access_token>`（43 字符）。服务端只保存令牌的 SHA-256，比较用 `subtle.ConstantTimeCompare`；撤销设备即失效。**不存在其它认证方式**（不发 `WWW-Authenticate`，不接受 Basic）。
- 本仓库只实现"扫码"入口。附近发现（mDNS `_cyime-clip._tcp.`）与连接码确认（`/api/pair/nearby`）不在范围内，见 §9。

### 3.2 剪贴板端点

| 方法 | 路径 | 语义 |
|---|---|---|
| GET | `/api/clipboard` | 读取电脑当前剪贴板文本 |
| PUT | `/api/clipboard` | 手机推送文本，写入电脑剪贴板 |

Profile JSON（请求体与响应体同构）：

```json
{"type":"text","text":"内容","size":123,"hash":"<sha256-hex>","source":"cyime-android","has_data":false}
```

- `type` 恒为 `text`；`has_data` 恒为 `false`；**不得**输出 `data_name`（出现即被手机端判为非法）。
- `size` = `text` 的 UTF-8 字节数；`hash` = 该 UTF-8 字节的 SHA-256 小写十六进制。两者必须由服务端重新计算，不得信任客户端传入值。
- `text` 非空、非纯空白、不含 NUL、UTF-8 长度 ≤ 65536 字节；否则 `400`/`413`。
- 请求体 ≤ 262144 字节。
- `source` 可选，≤ 128 字符；服务端发出的 `source` 固定标识电脑端。

响应与状态码（与手机端 `responseError` 一一对应，不得改语义）：

| 状态 | 触发条件 |
|---|---|
| `200` | 成功；PUT 的 200 响应体必须回显与请求**相同**的 `hash`，否则手机端视为未确认 |
| `204` | 剪贴板为空、非文本（图片、文件），或文本超过推送字符上限；手机端据此清空本地 ETag 且不清空手机剪贴板 |
| `304` | GET 携带 `If-None-Match` 且内容未变；不得带响应体 |
| `400` | 请求体非法（非 JSON、字段缺失、hash/size 不符） |
| `401` | 设备令牌缺失/无效/已撤销 |
| `409` | 写入时剪贴板被其他进程改变（CAS 失败），本次未写入 |
| `413` | 文本超过 64 KiB |
| `503` | 剪贴板被占用（OpenClipboard 重试后仍失败），或电脑端已停用 |

ETag：对文本内容计算（内容哈希的引号包裹形式，如 `"<sha256-hex>"`）；文本变化即变化；空剪贴板不产生 ETag。`If-None-Match` 与当前 ETag 相等时返回 `304`。

协议层的其它硬要求：

- 手机端每约 2 秒 GET 一次，`GET` 必须廉价且幂等：不得因 GET 修改剪贴板、不得刷新 ETag、不得记录正文。
- 手机端会重复 PUT 同一条内容（退避重试）：内容与当前剪贴板相同必须视为成功并返回 200，不重复写剪贴板。
- 不做跨设备时间顺序判断，不尝试"哪边更新"合并；不传历史记录、图片、文件、HTML。
- `GET /api/status`（带 Bearer）→ `200 {"v":2,"connected":true,"send_enabled":true,"receive_enabled":true,"device_id","server_name"}`；令牌无效 `401`。方向开关已取消，`send_enabled`/`receive_enabled` 恒为 `true`，字段保留只为与手机端既有解析逻辑兼容。不带 Bearer 时返回本机设置页用的运行状态，两者都不含秘密。

## 4. 同步引擎行为约束

- 变化检测用 `GetClipboardSequenceNumber`，不做轮询读全文；序列号变化才 `OpenClipboard` 读内容。
- 读写剪贴板必须带重试（`OpenClipboard` 失败按 ~10–30ms 间隔重试，总预算 ≤ 500ms），重试耗尽返回 `503`。
- 写入用 `GlobalAlloc(GMOVEABLE)` + `CF_UNICODETEXT`；写入前再次校验序列号（CAS 语义），不符返回 `409`。
- 自写抑制：本进程写入产生的序列号变化不得回送成"电脑新内容"事件，也不得被当成新条目再次推送。
- 单条文本上限 64 KiB；超限内容在事件流与日志中只记录字节数，不记录正文。
- 敏感内容分两类，必须区别对待：
  - **来源声明**（密码管理器/系统写入的 `ExcludeClipboardContentFromMonitorProcessing`、`CanIncludeInClipboardHistory=0`、`CanUploadToCloudClipboard=0`）——来源的明确意图，**始终跳过**，不提供任何放行开关。
  - **本程序猜测**（4–8 位纯数字 ≈ 短信验证码）——只是形态猜测，受 `skip_suspected_otp` 开关控制（默认开）。关闭后这类内容正常进缓存并推送，事件记为 `synced`，不再记 `sensitive`。
  - 判定来源由 `clipboard.Snapshot.SensitiveOrigin` 区分，判定策略集中在 `syncer.Engine.shouldSkip`，不得在 HTTP 层另写一套。
- 两个推送上限，都按 Unicode 字符计数（ASCII 与中文都算 1 个字符）：
  - `max_push_chars`（默认 1000，可配 1–100000）：自动推送的上限。超限文本**不进缓存**（GET 返回 204），事件记为 `oversize`。
  - `force_push_chars`（默认 10000，可配 1–100000）：托盘「推送」使用的手动上限，忽略常规上限。超限同样不进缓存并记 `oversize`。
  - 手动推送只放宽长度，**不放宽隐私策略**：`shouldSkip` 命中的内容仍然跳过。
  - 手动推送的放行**绑定内容本身**（`Engine.forceHash`）：成功后同一段内容再被观测到（系统或其它程序把剪贴板原地重设，序列号变、内容不变），或配置变更触发 `Refresh`，都**不得**再按常规上限把它从缓存撤下——否则手机下一次 GET 拿到 204，用户看到的是"强推了却没同步"。剪贴板换成别的内容时放行立即失效；程序重启不保留。
  - **协议硬上限 64 KiB 不可放宽**：`len(text) > protocol.MaxTextBytes` 时任何路径都记 `oversize` 并说明是协议上限，手动推送也不例外（手机端按同一数值拒绝，报"已推送"是假成功）。
  - 事件去重：序列号变化但**内容 hash 与判定结论都没变**时不得重复记事件；内容离开过剪贴板（空/非文本/敏感跳过一次）后再出现相同文字，算新的一次推送。
  - v6 迁移只在用户从未改过常规上限（仍是旧默认 5000）时才跟随新默认 1000，自定义值必须保留。
- 事件流（供设置页实时展示）只保留元数据：时间、方向、来源、字节数、hash 前缀、状态；正文预览恒开但**不落盘**，压成单行、超长时保留头 120 + 尾 40 字符，中间省略。
- 事件方向文案：`out` → 「推送」，`in` → 「拉取」。
- 环形缓冲，最多 200 条事件；进程重启后不恢复历史。

## 5. 托盘与进程生命周期

- 单实例：命名互斥量 `Global\CyIMESync.SingleInstance`；第二个实例启动时向已有实例发消息打开设置页后自行退出。
- 托盘图标：双击 = 打开设置页；右键菜单只含 设置、推送、退出。
  - 「推送」调用 `syncer.Engine.ForcePush`，按 `force_push_chars` 判长；同步停用时记一条 `invalid` 事件并放弃。
  - 菜单里**不放**启用/停用同步：同步开关只在设置页顶栏，避免两处状态互相覆盖。
- 关闭设置页浏览器标签不影响进程；只有"退出"结束进程。不弹关闭确认框。
- 消息循环必须在主 goroutine 绑定的 OS 线程上跑（`runtime.LockOSThread`），HTTP/同步引擎在其它 goroutine。
- 托盘图标资源用 `go:embed` 内嵌 ICO（`internal/tray/icon.ico`），运行期从内存生成 HICON，不依赖外部文件。
- 图标只有一份母版 `img/logo-512x512.png`：托盘 ICO、页面 `icon-64/180.png`、exe 文件图标（`rsrc_windows_amd64.syso`）都从它派生；换图标必须四者同步重生成，禁止另画一套品牌图形。
- 支持开机自启动（写 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`），默认**关闭**，由用户在设置页开启。
- 进程以普通用户权限运行；任何需要管理员权限的行为（防火墙规则、服务安装）都不做，只提示用户。

## 6. Web 设置页约束

- 单页，无路由框架，无外部 CDN、无外部字体、无外链；断网可用。
- Material 3：使用 M3 设计令牌（颜色角色、state layer、elevation、shape scale、typography scale），跟随系统明暗（`prefers-color-scheme`），主色可用 `#6750A4` 系；组件形态用 filled/outlined button、switch、card、snackbar。不手搓仿 iOS 风格。
- **布局固定为顶栏 + 双栏**：左栏依次是 连接（二维码 + 已连接设备）、同步、服务；右栏是整列高的**事件日志**（`position: sticky`，自身滚动）。窄屏（≤1000px）堆叠为单列，事件在最后。
- 顶栏**不做成横贯的色块**：透明背景、无下边框、不吸顶，只靠留白与下方卡片分层；页面滚动时顶栏跟随页面滚走。
- **页面不设页脚，顶栏也不显示版本号与副标题**：品牌区只有图标 + `CyIME`。不在任何位置罗列运行时长、版本、说明文字。
- 页面必须提供：服务总开关与端口、扫码二维码、已连接设备（可断开、可改备注）、允许的局域网绑定地址、自动推送字符上限、手动推送字符上限、跳过疑似验证码开关、**实时同步事件列表**（SSE 推送）、清空事件、开机自启动开关。同步方向恒为双向、事件预览恒开，因此页面上**不放**方向开关与预览开关。两个上限的界面名称固定为「自动推送字符上限」（后台监听触发）与「手动推送字符上限」（托盘菜单触发）。
- 两个字符上限都是 `type="number"`：统一宽度、隐藏调节箭头，并**禁用滚轮改值**（聚焦时 `wheel` 事件 `preventDefault` 后失焦），避免滚动页面时误改。
- 事件行**固定两行**、高度统一（不允许出现只有一行的矮条目）：第一行是 **方向胶囊（推送/拉取）在最左** + 时间（等宽小字）+ 状态说明（仅在**有正文**时出现）+ 右侧字节数；第二行是正文预览或跳过原因，**横跨整块从最左边缘起**（`grid-column: 1 / -1`，与方向胶囊左对齐，让正文拿满整块宽度）。整条事件是一个 `grid`，第二行不得用写死的 `padding-left` 模拟缩进。**条目之间不用分割线**：每条是独立圆角块（浅色用最亮的容器色、深色用高一级容器色），靠 6px 间距 + 块面底色分层，悬停时块面加深一级；不要退回「描边卡片」或「斑马纹」（斑马纹会随新事件插入而错位）。没有正文的事件（敏感 / 空内容 / 占用等）把 `note` 放第二行并按状态色显示，否则会矮一行、与其它条目参差不齐。状态列只放一段文字：有 `note` 就显示 `note`（它本身是完整描述），没有才退回短分类，**不得**把分类和说明并排堆在一起。预览必须按**真实行宽**（插入 DOM 后的 `scrollWidth`/`clientWidth`）判断是否放不下，只有放不下时才截成「头…尾」；放得下却切掉中间会让预览读起来像另一段文字。不展示设备加入日期。成功事件的第一行只留时间/方向/字节数。
- 事件**只标注例外状态**（`duplicate`/`sensitive`/`oversize`/`conflict`/`busy`/`invalid`/`empty`）：`synced` 是常态，不渲染状态文字；事件区头部也不放「实时」之类的连接指示，SSE 状态不外露。
- 设备列表是**一整块面板**：行首机型图标、行间细分隔线，名称（有备注时显示备注，型号降为次要小字）、连接状态与右侧「备注 / 断开」同处一行内。设备超过 4 台时在面板内滚动（细滚动条 + 底部渐隐提示还有更多），头部右侧显示 `n / 8` 数量；不得让列表溢出卡片或把每行做成独立小卡片。备注在重新扫码配对后必须保留。
- **每台设备必须标注连接状态**（行内「在线 / 离线」，绿点/灰点）。判定依据只有一条：该设备最近一次通过 Bearer 令牌认证的时间（`pairing.Manager.markSeen`/`LastSeen`，只存内存，进程重启后一律从离线开始）。空闲 ≥ `pairing.OnlineWindow`（15 秒，与 `app.js` 的 `ONLINE_WINDOW` 必须一致）即视为离线。服务端只在设备视图里下发 `idle_seconds`（`-1` = 本次启动后没连过），页面按秒本地推算；另用 10 秒低频兜底刷新 `/api/pairing`（仅页面可见时）发现手机重新上线，切回标签页立即刷新一次。不得为了这个状态去写盘、记录请求正文或改成 1 秒轮询。
- 文案极简：设置项旁边不写解释性小字，必要的说明放 `title` 悬停提示。
- 事件列表实时更新走 `text/event-stream`（`/api/events`），断线自动重连；不得用 1 秒轮询兜底。
- 所有写操作走 JSON API 并即时生效，无需重启；设置变更必须原子落盘。
- 页面只监听 `127.0.0.1` 的设置接口可免认证；但若 `/api/settings` 与 `/api/clipboard` 共用监听端口（默认如此），则设置写接口必须要求同源校验（`Origin`/`Referer` 为本机地址）+ 一次性本地令牌，防止局域网内其它主机改配置。只读状态接口可放宽。
- 组件用了 `display:flex/grid` 时 `[hidden]` 会失效，CSS 必须有全局兜底。

## 7. 安全、隐私与数据文件

- 默认监听 `0.0.0.0`（手机需要局域网可达），端口默认 `18740`；允许用户改为指定网卡地址。
- 全链路 TLS（自签 + 手机端指纹固定）与逐设备令牌；README 不再有"明文配对码"这类说明，因为明文认证已移除。
- 二维码含一次性配对密钥：只允许本机设置页凭令牌获取，不得匿名公开、不得写日志、不得落盘。
- 访问令牌只在配对响应里出现一次，服务端仅保存 SHA-256；撤销设备后立即失效。
- **数据文件全部位于 exe 同级 `data/`**：`config.json`、`tls/`、`logs/`。不得写注册表（自启动除外）、不得写 `%APPDATA%`（仅旧数据迁移时读取）。
- 日志策略：单文件上限 1 MiB，最多 4 个文件（当前 + 3 份历史），超过 7 天的历史文件在启动时与每 6 小时清理。**任何日志都不得包含剪贴板正文、令牌、配对密钥。**
- 响应加 `Cache-Control: no-store`；不返回 `Server` 版本细节；不做 CORS 通配。
- 不自动配置防火墙、不做 UDP 广播发现、不读取系统凭据。

## 8. 构建与验证

```
go build -trimpath -ldflags "-s -w -H windowsgui" -o build/cyime-sync.exe .
```

- 构建产物统一放 `build/`，运行期数据随之落在 `build/data/`；`build/`、`data/`、`*.exe` 都在 `.gitignore` 里。
- `rsrc_windows_amd64.syso` 已入库，`go build` 会自动把它链进 exe（文件图标）；换图标后重新生成：
  `go run github.com/akavel/rsrc@latest -ico internal/tray/icon.ico -arch amd64 -o rsrc_windows_amd64.syso`。
  `.syso` 缺失也能构建，但 exe 会退化成默认图标——交付前必须确认 exe 的 `.rsrc` 段非空且与托盘图标同源。
- 交付前必须实跑：`go vet ./...`、`go test ./...`、`go build` 通过，并实际启动 exe 验证托盘双击打开设置页、扫码配对 → 读写往返、事件流刷新。
- 推送上限必须双份验证：同一段超过 `max_push_chars` 的文本，常规轮询应记 `oversize`，托盘「推送」应记 `synced`（`已手动推送`）；再把长度拉到超过 `force_push_chars`，两条路径都应 `oversize`。
- 设置页的数字输入框要实测滚轮：聚焦后滚动不得改变数值（`WheelEvent` 应被 `preventDefault`）。
- 协议验证用本地回环：起进程后用手工 `curl` 覆盖 200/204/304/400/401/409/413/503 与中文/表情/生僻字/换行文本。
- 必须额外验证：同一端口 HTTPS 握手成功且证书指纹与二维码内一致；`/api/qr.png` 无令牌 `403`；用二维码里的 `pairing_id`/`secret` 走完 `201` → Bearer 读 `/api/status`、读写 `/api/clipboard`；重复配对 `410`；旧 Basic 请求 `401`。
- 必须验证数据目录：首次启动生成 `build/data/config.json`、`build/data/tls/`、`build/data/logs/`；日志写满上限后产生 `.1.log`；超期历史文件被清理。
- 不在 `go test` 里依赖真实系统剪贴板；`internal/clipboard` 通过接口注入假实现，Windows 实现只在 `_windows.go` 文件中，非 Windows 构建必须能通过 `go vet`（用 build tag 隔离）。
- 任何"已验证"结论必须给出实际命令与输出；未跑真机/局域网验收要写明未验收。

## 9. 明确不做（非目标）

- 旧协议兼容（明文 HTTP Basic + 手填配对码）：已删除，不再加回。
- 附近发现（mDNS `_cyime-clip._tcp.`）与连接码确认（`/api/pair/nearby`）：手机端"附近电脑"列表不会出现本机，只能扫码连接。
- 剪贴板历史批量同步、图片/文件/HTML/RTF 同步、多设备广播（只服务单台/多台手机对同一电脑）。
- 输入法功能、候选词、Rime 引擎、云端存储、账号体系、自动更新。
- 安装器、服务注册、防火墙自动化、Linux/macOS 移植。
- 修改 Android 端协议语义。

### 已知限制（2026-11-01 确认暂不处理）

- **手机 → 电脑方向不做敏感判定**：手机推来的内容一律写入剪贴板并进入事件预览（预览仅内存、单行、≤160 字符）。协议里没有"本条内容敏感"的标记，电脑端无从判断；要处理需先与 Android 端约定字段。若将来要做，优先方案是"疑似验证码仍写入，但不进历史、事件不打预览"。
- **写入剪贴板时不设置历史/云同步标记**：三个隐私格式（`ExcludeClipboardContentFromMonitorProcessing`、`CanIncludeInClipboardHistory`、`CanUploadToCloudClipboard`）**只在读取时判定，写入时从不设置**，因此同步来的内容会被 Windows 剪贴板历史（Win+V）留存。用户开启 `HKCU\Software\Microsoft\Clipboard\EnableClipboardHistory=1` 时需自行评估。

## 10. 变更流程

- 改协议字段、状态码、ETag 语义 → 先在本文件 §3 更新，再改代码。
- 新增依赖 → 先在本文件 §1 表格登记理由；未登记即视为违规。
- 与手机端实测不符时，以手机端实际请求/响应为准修正服务端，并在本文件记录该实测结论。
