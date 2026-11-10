# Lumen 技术框架 v1

> 对应契约：`docs/Lumen-API契约-v1.md` v1.7（29 path / 33 operation）
> 对应设计稿：32 屏，fileId 745649420954674
> 2026-11-11

---

## 0. 四个硬约束

所有选型都由这几条倒推出来，不是先选技术栈再找场景：

| # | 约束 | 直接后果 |
|---|---|---|
| **C1** | 媒体源是**磁盘上的原盘级文件**（4K HEVC 10bit HDR、TrueHD、21GB 单文件），目录结构由用户自带、命名不规范 | 必须 Direct Play 优先；不能做"上传后转码"的 Netflix 式架构 |
| **C2** | **主机形态不预设**：可能是 NAS（弱核显或无独显），也可能是日常在用的桌面 PC / Mac（有独显或强核显）。**同一份二进制要在三种上都跑对** | 「单核 0.4× 实时」这类结论**不能写进设计**。转码能力必须**启动时探测**，配额与画质档位由探测结果决定 |
| **C3** | 访问形态是**局域网为主 + 偶尔外网**，不开公网端口 | 用 Tailscale 组网；鉴权不能依赖"内网可信" |
| **C4** | 主机可能是**用户正在使用的那台电脑**，不是专用设备；也可能是插着移动硬盘的笔记本 | 后台任务必须"有礼貌"：可暂停、低优先级、电池供电时不跑、拔盘要能扛住 |

C2 是本版最重要的变化。上一版把"NAS + 无独显"当成事实，于是推出了"转码硬限 1 路""必须预转代理版"这些结论——但媒体目录完全可能在一台带 RTX 显卡的台式机上，那里实时转 4K 绰绰有余，硬限 1 路反而是自废武功。

**处理办法不是"分两套设计"，是把能力变成运行时变量**：服务端启动时自测（§1.1），策略由 profile 推导。这样 NAS 和游戏主机跑的是同一份代码、同一套接口，只是参数不同。

C3 仍是转折点。如果只有局域网，转码是可选优化项；一旦有外网，转码变成**必备组件**——外网链路扛不住 4K 原盘码率。

---

## 1. 系统拓扑

```
┌───────────── 主机：NAS / 桌面 PC / Mac / 小服务器（同一份二进制） ─────────────┐
│                                                                              │
│  lumen-server  (Go, 单二进制；systemd / launchd / Windows 服务 或前台运行)      │
│   ├─ HTTP/WS  :8686   局域网 + tailscale0                                    │
│   ├─ SQLite    <dataDir>/lumen.db  (WAL)  ← **必须在本地盘**，见 §1.2          │
│   ├─ HostProbe 启动时自测：编码器 / 磁盘 / 核数 → host_profile                 │
│   ├─ Worker   goroutine 池：扫描 / 刮削 / 缩略图 / 指纹                         │
│   ├─ Transcoder  按 profile 决定并发与编码器（软编 / QSV / NVENC / VAAPI / VT） │
│   └─ FFmpeg / ffprobe / libvips  外部进程（随包分发或系统安装）                 │
│                                                                              │
│  tailscaled   ← 唯一的外网入口，不监听公网                                     │
└──────────────────────────────────────────────────────────────────────────────┘
        │                        │                              │
   媒体卷（可多个、可移动）    LAN 192.168.1.0/24          Tailscale 100.64.0.0/10
   C:\Users\me\Videos              │                              │
   D:\Media  /Volumes/Media        ▼                              ▼
   \\nas\media（SMB）      ┌───────────────┐            ┌───────────────┐
                           │ 桌面端 Tauri 2 │            │ 移动端 RN      │
                           │ React + libmpv │            │ AVPlayer /     │
                           │ (macOS/Win)    │            │ ExoPlayer      │
                           └───────────────┘            └───────────────┘
```

**媒体卷与数据目录必须分开配置，这是硬要求。** 媒体可以在任何地方（本地 NTFS/APFS、外接 USB、SMB/NFS 网络盘）；`lumen.db` 只能在本地盘上——SQLite 的 WAL 依赖 POSIX 文件锁与 `mmap` 一致性，在 SMB/NFS 上会**静默损坏**（不是报错，是数据坏掉）。把它放进媒体目录旁边是最容易被"顺手"做错的一件事。

**一个刻意的设计：服务端不内置播放能力。** mpv 只在客户端。服务端关于播放的全部职责是——给 URL、记账、在失败后给诊断。这条边界让服务端保持无状态（除了 SQLite），重启不影响正在播的直连会话。

---

## 1.1 主机能力探测（Host Profile）——通用化的核心

因为主机可能是 NAS 也可能是台式机，**任何写死的转码能力都是错的**。做法：首次启动跑一次探测，结果落库，之后只读库。

### 探测什么、怎么探

| 项 | 方法 | 为什么不能靠"猜" |
|---|---|---|
| 可用硬件编码器 | `ffmpeg -hide_banner -encoders` 列表 **+ 真编 24 帧** 验证 | 驱动装了但卡不在（虚拟机、远程桌面会话）、NVENC 授权不足都会让"列表里有"变成"实际用不了"。**必须真编一小段**再判定可用 |
| 全链吞吐 | 真实片源 `4K → 1080p → 编码`，**8 秒窗口 × 两个取样点**，取较小值 | 同一句 `-preset veryfast` 在 i9 和 J4125 上差 10 倍；同一台机器换个场景还能再差 13% |
| 仅解码吞吐 | 同片源只解码不编码 | 用来区分"解码慢"还是"编码慢"，排查时才有方向 |
| DV tonemap 吞吐 | DV P5 片源 + `libplacebo`，软解 + 编码 | 与分辨率几乎无关（逐像素成本），单独测才知道能不能实时 |
| 磁盘类型 | 数据目录写 32MB 并 `fsync` | 决定扫描并发与缩略图生成节奏。**只写数据目录**：往用户的媒体库里丢临时文件不可接受 |
| 逻辑核数 / 内存 | 系统 API | 决定 worker 池大小 |
| 供电形态 | 笔记本：`GetSystemPowerStatus` / `/sys/class/power_supply` | C4：电池上不该跑转码 |
| 媒体卷的读写特性 | 每个卷单独测一次首字节延迟（**尚未实现**，`volumes.first_byte_ms` 已留列） | SMB 网络盘上随机读小文件极慢，`fsnotify` 也不可靠（见 §1.2） |

**探测耗时**：全量约 **28 秒**（四个测量项 × 两个取样点），`--probe-fast` 约 8 秒。原定"10 秒内"是在只有"编 1 秒 1080p"一项时定的，加入 4K 与 DV 两项后不再成立。**这个代价是一次性的**：结果落库，之后启动只读库；探测本身在后台跑，不阻塞服务启动。

### 探测方法里三个反直觉的点（都是实测撞出来的）

**① 必须扣掉"打开文件 + seek"的固定开销。** 打开一个 33 GB 的 mkv 并跳到 120 秒处要 **约 0.85 秒**，这段时间一帧都没处理。用"总帧数 ÷ 总墙钟"算，这段会被摊进帧率里：8 秒窗口下硬编 480 fps 会被算成 **259 fps**，4 秒窗口下更糟到 **190 fps**。做法是**同一命令再跑一次 1 秒的基线运行**，把它的墙钟时间扣掉。8 秒与 60 秒窗口交叉验证：480 vs 494 fps，差 3%。

**② 不能信 ffmpeg 自报的 `fps=`。** 实测短片段里进度行与收尾行全是 `fps=0.0`（进程结束太快，只有收尾那一行统计）。这个数算的还是"距上次上报的增量"，是瞬时值。帧数从 `-progress pipe:1` 取（与日志级别无关），帧率自己算。

**③ 一个取样点不够。** 同一台机器、同一条命令，120 秒处取 8 秒测出 81 fps，同一位置取 30 秒测出 72 fps——差 13%，纯粹因为两个片段内容不同（动作戏比对话戏难压）。所以取**两个相隔半部片子的取样点，用较小值**。取最小值是保守方向：多估一点等待时间，比承诺了却卡住好。

### 分档与策略

| 档 | 判据（全链吞吐，fps） | 转码并发 | 默认最高档位 | 扫描并发 |
|---|---|---|---|---|
| **S** 强 | 有可用硬编 **且** 全链 ≥ 48 fps（2× @24fps） | **2–3 路** | 2160（外网仍按 `externalMaxHeight`） | 4 |
| **M** 中 | 有可用硬编，或全链 ≥ 24 fps（1× @24fps） | **2 路** | 1080 | 3 |
| **L** 弱 | 全链 7.2–24 fps（0.3–1× @24fps） | **1 路** | 1080（4K 需预转，见 §9 R1） | 2 |
| **X** 无 | 全链 < 7.2 fps 或探测失败 | **1 路，且只允许 remux** | 720 | 1 |

**判据用绝对 fps，不用倍率。** 倍率是 `fps ÷ 片源帧率`，同一台机器在 24fps 片源上是 20×、在 60fps 的 HFR 片源上只剩 8×。把倍率当机器属性存下来，一部 60fps 的电影就会被判成"轻松可转"（R1 ①）。上表的括号里写"@24fps"只是给读者一个直观折算，库里存的是 fps。

**原表 0.6–1.0× 之间是空的**（M 到 L 之间有断档）。本版把这一段并入 L——保守，且不会出现"落不进任何档"的空洞。

**并发由吞吐推导**：`floor(全链fps ÷ (24 × 1.35))`，再按档位上限截断。1.35 是每路转码预留的余量（音轨转码、mux、磁盘抖动、别的东西抢 CPU）。实测 480 fps 的机器算出 14 路，截断到 S 档的 3 路；软编 76 fps 算出 2 路。

**接口表面完全一致，只是参数不同。** 契约里 `renditions` 必须按本机 profile 过滤——L 档机器不能把 `{height:2160, requiresTranscode:true}` 放进响应，否则用户点了就是卡顿或 `TRANSCODER_BUSY`。这条已写进契约 §4.1。

### 一个反直觉的推论

**越是强机器，越要限制并发。** 台式机同时转 3 路会把风扇拉满、CPU 打满，用户正在这台机器上干别的——C4 说的就是这个。所以并发上限不只看算力，还要看"这台机器是不是有人在用"：

- 有人在前台用（检测到输入活动 / 全屏应用）→ 降到 1 路，且 soft nice
- 电池供电 → 暂停转码，正在播的会话不打断（继续用已生成的分片），新请求排队
- 可配置"只在 22:00–08:00 转码"（29 屏定时任务已有这个模型）

## 1.2 媒体存储无关化

媒体目录从"NAS 上固定的 `/volume1/media`"变成"用户随便指一个文件夹"，带来五个必须处理的问题。**每一条都会在 Windows 上先炸。**

### ① 盘符会变，所以不存绝对路径

Windows 上 `D:\Media` 拔了移动硬盘再插，可能变成 `E:\Media`；NAS 挂载点、Mac 的 `/Volumes/xxx` 同理。

**规则：库里只存 `volume_id + rel_path`，绝对路径每次用时由 `volume_id` 解析。**

`volume_id` 取卷的稳定标识，不能用盘符：

| 平台 | 取法 |
|---|---|
| Windows | `GetVolumeInformationW` 的 volume serial number + 卷标；或用 `\\?\Volume{GUID}\` |
| macOS | `diskutil info -plist` 的 `VolumeUUID`（网络卷无 UUID → 退回挂载 URL 哈希） |
| Linux | `blkid` 的 `UUID`；网络挂载用 `fstab` 里的挂载点 + `statfs` 的 `f_fsid` |

盘符变化时服务端**不该报错**，而是把该卷标成 `offline`，30 屏之外不影响其它卷的播放。

### ② 大小写敏感性不一致

Windows / macOS（默认 APFS）不区分大小写，Linux 区分。同一个文件在两平台上算不算"同一个"，取决于比较方式。

**规则：所有用于去重与查找的路径键，先 `foldCase()`（Unicode 简单大小写折叠）再比较；显示时用原始大小写。** 索引列存折叠后的值，另存一份原始值给 UI。不做这一步，同一批文件在 Windows 上扫两次会得到一套结果，换到 Linux 上又是一套。

### ③ Unicode 规范化差异（最容易漏的一条）

macOS 的文件名返回 **NFD**（「旅」拆成基字符 + 组合符号），Windows/Linux 多为 **NFC**。中文字符里带声调的字、韩文、带重音的拉丁字母都会中招。

后果很具体：同一个文件，在 Mac 上扫出来的路径字符串与 Windows 上不相等 → 换平台后**整库重新匹配一遍**，或者去重失效出现两条记录。

**规则：入库前一律 `NFC` 规范化；比较时也用 NFC。** Go 里是 `golang.org/x/text/unicode/norm`，一条 `norm.NFC.String()`，但漏了就是跨平台数据不一致。

### ④ 文件监控不可靠，必须能降级

`fsnotify` 在本地盘可用（Windows 用 `ReadDirectoryChangesW`，macOS 用 FSEvents，Linux 用 inotify），但：

- **SMB / NFS 网络盘上没有事件**（或只有部分事件）
- inotify 有 `max_user_watches` 上限，大库会撞
- Windows 上网络盘直接不支持

**规则：监控是优化，扫描是基线。** 每个卷记一个 `watch_mode`：`native`（事件）/ `poll`（定时全量/增量比对 `mtime+size`）/ `off`。网络卷自动落 `poll`，默认间隔 30 分钟。**任何依赖"收到事件"的逻辑都是错的。**

### ⑤ 路径长度与权限

- Windows 传统 API 有 260 字符限制，深目录 + 长文件名很容易超。**全平台统一在路径前加 `\\?\` 前缀**（Windows）或用 Go 的 `os` 包自动处理，并在解析阶段就拒绝超长路径而不是扫描到一半失败。
- 主机是 PC 时服务端以**普通用户身份**运行，没有 NAS 上的 `media` 组。权限错误要归为"卷不可读"而不是"文件损坏"——否则 28 屏会把权限问题显示成"这个文件有损坏"。
- 用户可能把整个 `C:\Users\me` 加进来（含系统目录）。**加库时禁掉系统路径**：`C:\Windows`、`Program Files`、`/System`、`~/.ssh` 等，否则会扫到几十万个无关文件。

---

## 2. 服务端分层

```
cmd/lumen/main.go              装配、配置、信号处理
internal/
  api/            HTTP/WS 传输层：路由、中间件、DTO 编解码
  domain/         纯业务：匹配、播放决策、诊断、权限判定
  store/          SQLite 访问（唯一的 SQL 出现地）
  pipeline/       任务编排：队列、租约、checkpoint、进度上报
  media/          ffprobe、ffmpeg、libvips 的进程封装
  hub/            WS 事件总线（seq、环形缓冲、订阅）
  lib/            ULID、签名、时间、日志
```

**分层铁律**（这三条比目录结构重要）：

1. `domain` **不 import `api`、不 import `store`**。它只接受值类型和接口。这条保证播放决策可以被单测覆盖——本产品最需要被测的就是"这个文件该直连还是转码"。
2. **SQL 只出现在 `store`**。`api` 层不许出现 `db.Query`，否则"counts 与 items 同事务"这类约束会被随手破坏。
3. `pipeline` 不直接写库，通过 `store` 的窄接口（`UpdateProgress`、`FinishTask`）——这是 `task.progress` 不落库那条规则的落点。

### 2.1 写连接的串行化

SQLite 在 WAL 下允许并发读、单写。Go 的 `database/sql` 连接池会自然地并发写，然后撞 `database is locked`。做法是**两个句柄**：

```go
// 读池：多连接，只读
ro, _ := sql.Open("sqlite", "file:lumen.db?mode=ro&_pragma=query_only(1)")
ro.SetMaxOpenConns(8)

// 写句柄：单连接，所有写操作串行
rw, _ := sql.Open("sqlite", "file:lumen.db?_pragma=busy_timeout(5000)")
rw.SetMaxOpenConns(1)
```

`SetMaxOpenConns(1)` 让写请求在 Go 侧排队，而不是在 SQLite 侧争锁超时。代价是写吞吐上限 = 单次事务耗时，对本产品足够（最大写压力是扫描器每 200 文件一次批量插入）。

### 2.2 SQLite Pragma

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;   -- 不是 FULL：崩溃最多丢最后一个事务，可接受
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA mmap_size    = 268435456; -- 256MB，覆盖整个库
PRAGMA temp_store   = MEMORY;
```

`synchronous=NORMAL` 是算过的：WAL 下它只在 checkpoint 时才 fsync，正常掉电不丢数据，只有内核崩才可能丢最后一个事务。这台机器上有价值的是扫描速度，不是"最后一个进度点不丢"。

---

## 3. 数据层

### 3.1 表（一期）

**`volumes`** — 媒体卷（新增，替代"绝对路径"）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | TEXT PK | `vol_` + ULID |
| `volume_key` | TEXT UNIQUE | 卷稳定标识：Windows volume serial / macOS VolumeUUID / Linux `blkid` UUID；网络卷用挂载 URL 哈希 |
| `kind` | TEXT | `local` / `removable` / `network` |
| `mount_hint` | TEXT | 最近一次见到的挂载点或盘符，**仅用于提示，不作为寻址依据** |
| `watch_mode` | TEXT | `native` / `poll` / `off`，见 §1.2 ④ |
| `status` | TEXT | `online` / `offline`（拔盘、网络断） |
| `read_mbps` | REAL | 探测的吞吐，决定扫描并发 |
| `first_byte_ms` | REAL | 首字节延迟，网络卷通常很大 |

**`libraries`** — 媒体库根

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | TEXT PK | `lib_tv` / `lib_movie` / `lib_photo` / `lib_music` |
| `volume_id` | TEXT FK | 指向 `volumes` |
| `root_rel` | TEXT | **相对卷根**的路径，如 `media/tv`、`Users/me/Videos`，统一 `/` 分隔 |
| `kind` | TEXT | `movie` / `tv` / `photo` / `music` |
| `name` | TEXT | 展示名 |
| `enabled` | INT | |
| `last_scan_at` | INT | unix ms，驱动 28 屏「上次扫描 8 分钟前」 |

> 唯一键是 `(volume_id, root_rel)`，不是 `root_rel` 本身——两个卷上都可以有 `media/tv`。

**`unmatched`** — 待整理（契约组 A 的数据源）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | TEXT PK | `um_` |
| `volume_id` | TEXT FK | |
| `rel_path` | TEXT | 相对**库根**，统一 `/` 分隔；给右栏「路径」行 |
| `path_key` | TEXT UNIQUE | `volume_id + foldCase(NFC(卷相对路径))` 的哈希，**去重的唯一依据**，见 §1.2 ②③。注意括号里是**卷相对路径**（`root_rel + rel_path`），不是上一列 |
| `library_id` | TEXT FK | |
| `size_bytes` | INT | |
| `mtime` | INT | 严重依赖：增量扫描靠它 |
| `media_type` | TEXT | `unknown` / `ambiguous` / `movie` / `episode` / … |
| `status` | TEXT | `pending` / `matched` / `ignored` |
| `confidence` | REAL | 0 表示未识别 |
| `reason_code` | TEXT | 契约枚举 |
| `reason_params` | TEXT | JSON |
| `parsed` | TEXT | JSON，文件名解析结果 |
| `probe` | TEXT | JSON，ffprobe 原始结果 |
| `thumb_path` | TEXT | 本地缓存路径（相对 dataDir），非 URL |
| `ignored_until` | INT NULL | TTL 预留，一期恒 NULL |
| `missing_since` | INT NULL | 文件消失的时间；非空即「软删」，见 §4.1 |
| `created_at` / `updated_at` | INT | |

> **`rel_path` 与 `path_key` 用的不是同一个路径（v3.6 澄清）**
>
> `rel_path` 是**库相对**的（`a.mkv`），因为契约要求返回给客户端的是这个，
> 客户端拼 `{libraryName}/{relPath}` 显示（契约 241 行）。
> 但 `path_key` 必须用**卷相对**路径（`media/movie/a.mkv`）算——
> 同一块盘上 `media/movie` 和 `media/tv` 两个库都可以有 `a.mkv`，
> 用库相对路径算键会撞，第二个文件会被唯一约束挡掉。
>
> 实现见 `domain.VolumePathKey(volumeID, rootRel, libRel)`。

索引：
```sql
CREATE INDEX idx_unmatched_status ON unmatched(status, created_at DESC);
CREATE INDEX idx_unmatched_lib    ON unmatched(library_id, status);
CREATE INDEX idx_unmatched_mtime  ON unmatched(mtime);   -- 增量扫描比对
CREATE INDEX idx_unmatched_missing ON unmatched(missing_since) WHERE missing_since IS NOT NULL;
```

**`media`** — 已入库

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | TEXT PK | `md_` |
| `kind` | TEXT | `movie` / `episode` / `track` / `photo` |
| `tmdb_id` | INT NULL | |
| `title` / `original_title` | TEXT | |
| `year` | INT NULL | |
| `season` / `episode` | INT NULL | |
| `library_id` | TEXT FK | |
| `volume_id` + `rel_path` | TEXT | **定位用**，`/` 分隔，**不存绝对路径**（§1.2 ①） |
| `path_key` | TEXT UNIQUE | `sha256(volume_id ‖ 0x00 ‖ Fold(NFC(卷相对路径)))`，**唯一键**，算法同 `unmatched` |
| `file_path` | TEXT NULL | 最近一次解析出的绝对路径，**仅用于展示与排障**，不参与唯一性 |
| `mtime` | INT | **增量扫描必需**：只认 `mtime + size` 判变化（v3.6 补，原文漏了） |
| `missing_since` | INT NULL | 文件消失的时间；非空即「软删」，见 §4.1 |
| `duration_ms` / `size_bytes` / `bitrate_kbps` | INT | **播放决策与 ETA 全靠这三列** |
| `width` / `height` | INT | |
| `video_codec` / `audio_codec` / `container` | TEXT | 直连判定的输入 |
| `hdr` | TEXT NULL | `hdr10` / `dv` / `hlg` / NULL |
| `added_at` / `updated_at` | INT | |

```sql
CREATE UNIQUE INDEX idx_media_path ON media(path_key);
CREATE INDEX idx_media_tmdb  ON media(tmdb_id) WHERE tmdb_id IS NOT NULL;
CREATE INDEX idx_media_ep    ON media(tmdb_id, season, episode);
```

> **为什么唯一键是 `path_key` 而不是 `file_path`（v3.5 更正）**
>
> 早先这里写的是 `UNIQUE(file_path)`，与 §1.2 ①「库里只存 `volume_id + rel_path`」直接冲突，
> 也与本文件 §4.1 扫描流程里「与 `unmatched.path_key` + `media.path_key` 比对」对不上——
> 那句话已经要求 `media` 有 `path_key` 列，而表定义里没有。
>
> 冲突的后果很具体：移动硬盘从 `D:\` 拔下再插成 `E:\` 时，`volume_id`（卷序列号）与 `rel_path` 都没变，
> 本该命中同一行；但 `D:\…` 与 `E:\…` 是两个字符串，`UNIQUE(file_path)` 约束放行，
> 于是库里出现两行指向同一部物理片子。两份 `media_id` 就带来两份 `playback_progress`
> ——首页多一部重复电影、昨天的进度消失，**而日志里一片干净**。
>
> 按 `path_key` 去重后，盘符变化时唯一约束直接命中，什么都不会发生。

FTS5 全文索引走独立虚表 + 触发器同步，不要 `content=media` 直接绑——媒体表结构会变。

**`tasks`** — 任务（契约组 B）

| 列 | 说明 |
|---|---|
| `id` `kind` `state` `title` `priority` | |
| `target` | JSON（`libraryId` / `path` / `fileCount` / `sizeBytes`） |
| `progress_done` / `progress_total` / `unit` / `percent` | **落库，但只在 checkpoint 时写** |
| `rate_per_sec` / `eta_ms` | 不落库，重启后由进度差重算 |
| `can_pause` / `can_cancel` / `can_retry` | 由 `kind` + `state` 派生，不存 |
| `pause_requested_at` | 协作式暂停的唯一凭据 |
| `result_code` / `result_params` | 契约 `TaskResult` |
| `needs_attention` | JSON，29 屏「需处理 ›」 |
| `error_code` / `error_message` / `engine_log_tail` | |

```sql
CREATE INDEX idx_tasks_queue ON tasks(state, priority DESC, created_at);
```

**`playback_sessions`** — 播放会话（内存为主，此表为崩溃恢复与 30 屏推断）

| 列 | 说明 |
|---|---|
| `id` | `ps_` |
| `media_id` / `device_id` | |
| `mode` | `direct` / `remux` / `transcode` |
| `resume_position_ms` | 起播点 |
| `progress_token` | 防并发乱序回写 |
| `transcode_job_id` | 指向 tasks |
| `start_segment` | HLS 起点 |
| `created_at` / `last_seen_at` | GC 依据 |

**`playback_progress`** — 续播点，独立于会话（会话会 GC，进度必须留）

```sql
PRIMARY KEY (media_id, device_id)
-- position_ms, duration_ms, playing, updated_at
```

这张表同时是 30 屏「正在使用 · 直连播放」的**推断来源**：服务端没有"客户端在上报播放"以外的信息源，`playing=1 AND now-updated_at<90s` 就是"正在播"。

**`devices`** / **`diagnoses`** / **`settings`** — 略，字段直接照契约 `Device` / `Diagnosis` 建。

**`host_profile`** — 主机自测结果（新增，见 §1.1）

单行表（`id = 1`）。探测很贵（要真编一小段），所以结果落库，启动读库、只在"硬件变了"或手动重测时更新。

| 列 | 类型 | 说明 |
|---|---|---|
| `tier` | TEXT | `S` / `M` / `L` / `X` |
| `max_height` | INT | 该档允许对外提供的最高档位（2160/1080/720），`renditions` 按它过滤 |
| `encoders` | TEXT | JSON：`[{name, listed, usable, verified, hardware, why}]`；`verified` 表示真编过 |
| `hw_encoder` / `soft_encoder` | TEXT | 实际选中的硬编与软编；`hw_encoder` 为空表示只有软编 |
| `decode_fps` | REAL | 仅解码吞吐 |
| `chain_fps_1080` | REAL | **决策主依据**：4K→1080p 全链吞吐 |
| `chain_fps_2160` | REAL | 4K→2160p 全链吞吐（源不是 4K 时为 NULL） |
| `tonemap_fps` | REAL | DV P5→SDR 全链吞吐；NULL 表示没测或没有 libplacebo |
| `tonemap_slots` | INT | 恒为 0 或 1。DV tonemap 与分辨率无关，**每台机器只允许 1 路** |
| `cores` / `mem_mb` | INT | |
| `transcode_concurrency` | INT | 由吞吐推导，可被用户设置下调，**不可上调过档位上限** |
| `sample_kind` / `sample_path` / `sample_height` / `sample_hdr` | TEXT/INT | 测量样本与它的关键属性（`dv5` 这类），用于解释"这个数是哪来的" |
| `confidence` | TEXT | `measured`（真实片源）/ `estimated`（合成源折算） |
| `disk_class` / `disk_write_mbps` | TEXT/REAL | **数据目录**所在盘；媒体卷的特性另列在 `volumes` |
| `on_battery` | INT | 上次探测时是否在用电池（只影响"现在跑不跑"，不改档位） |
| `probed_at` / `probe_ms` | INT | 探测时刻与耗时 |
| `probe_error` | TEXT NULL | 探测失败原因；非空时按 `X` 档保守运行 |
| `reasons` | TEXT | JSON：决策依据的完整列表，32 屏本机诊断直接展示 |

**为什么删掉了原来的 `soft_x264_ratio` / `soft_x264_ratio_4k`**：它们是 `f(fps, 片源帧率)` 的结果，不是独立事实。同一台机器在 24fps 片源上是 20×、60fps 上是 8×，存一个"倍率"必然在某个时刻被误用成机器属性（R1 ①）。现在库里只存 fps，倍率在读取时按片源帧率现算。

**为什么要存而不是每次启动算**：全量探测约 28 秒，在弱机器上本身就是 CPU 满载。每次启动都做，用户会觉得"这个程序开机就卡一下"。而且结果必须能解释——`reasons` 那一列就是给 32 屏回答"你凭什么说这台机器行"。

### 3.2 一张表都不建的（一期）

`photos` / `photo_faces` / `albums` / `tracks` / `playlists` / `collections`。设计稿里有 12 屏是照片和音乐，但一期范围只到"扫描 + 刮削 + 电影剧集播放"。**提前建空表没有收益**，反而让迁移脚本多 8 个空壳。预留只在两点：`media.kind` 枚举先留出 `track`/`photo`；`libraries.kind` 留出 `photo`/`music`。

---

## 4. 媒体管道

### 4.1 扫描

```
walk(volume.mount, library.root_rel)
          ──▶ 过滤（扩展名 / . 开头 / @eaDir / #recycle / $RECYCLE.BIN / .DS_Store / 系统目录）
          ──▶ 路径归一：NFC + 拆成 rel_path，算 path_key
          ──▶ 与 unmatched.path_key + media.path_key 比对
                ├─ 已存在且 mtime+size 未变 → 跳过（绝大多数）
                ├─ 已存在且变了 → 重新 probe + 重解析
                └─ 新文件 → 插 unmatched 待解析
```

**只认 `mtime + size` 两列**，不认 `inode`（SMB/NFS 后不可靠，Windows 上也不同），不算哈希（21GB 文件算一次 30 分钟，扫描体验会崩）。

**比对用 `path_key`，不是路径字符串**——它已经过 `volume_id + foldCase + NFC`，见 §1.2 ②③。绕过这一步直接用 `rel_path` 比较，会在"Mac 上建的库搬到 Windows"时全量重扫，或者在大小写不敏感的卷上重复入库。

**跳过目录要按平台给全**，Windows 的 `$RECYCLE.BIN` / `System Volume Information`、macOS 的 `.Spotlight-V100` / `.fseventsd`、群晖的 `@eaDir` / `#recycle`，漏一个就会扫进几千个垃圾文件。

每 200 文件一个事务批量提交 + 一次 checkpoint 进度上报。这是"暂停"的最小粒度——30 屏显示「已处理 8,486 / 12,480」时，中断最多丢 200 个文件的工作量。

**卷状态先判**：`status=offline` 时直接跳过并推 `task.finished{needsAttention:true}`，不要去 walk 一个不存在的挂载点——Windows 上访问已拔出的盘符会阻塞数秒。同理，遍历中途遇到 `ERROR_NOT_READY` 要整体中止该卷，而不是当作"文件被删"把库里的记录清空。**"读不到"和"不存在"必须区分**，混为一谈的结果是拔一次移动硬盘，整卷观影记录被删。

**消失只打标记，不删记录**（v3.6）：遍历完整结束后，库里记得、盘上没见到的记录写 `missing_since`，物理行保留。理由是 `playback_progress` 的 `media_id` 外键是 `ON DELETE CASCADE`——删 media 会连带删掉续播点，而这正是用户最在意的那部分数据。真正的清理由用户在 29 屏「需处理」里显式确认。

判定消失有一个必要条件：**只有遍历完整结束（`Partial=false`）才做**。任何权限错误、被独占的目录、取消，都会让「没见到」和「不存在」不可区分，此时宁可不标。

### 4.2 文件名解析 → 候选 → 匹配

**先纠正一个前提：文件名不是唯一、也不是最好的输入。**

实测 F:\movies 里那个 DV 片源，`ffprobe -show_entries format_tags` 直接给出：

```
TAG:IMDB=tt43515101
TAG:TMDB=movie/1633056
TAG:Group=HHWEB
```

**容器里就带着准确身份。** 这不是个例——现代压制组的 mkv 几乎都会写 `TMDB` / `IMDB` 标签，因为播放器（Infuse、Kodi、Emby）都在读它。而 R6 实测「脏文件名命中率」时用的全是文件名，所以才有那么多 0 命中。

匹配优先级因此定为三级，成本与准确率都是递减的：

| 优先级 | 输入 | 成本 | 准确率 |
|---|---|---|---|
| 1 | 同目录 `.nfo`（Kodi 格式，含 `<tmdbid>` / `<imdbid>`） | 一次小文件读 | 精确 |
| 2 | 容器 tag（`TMDB` / `IMDB`） | 一次 ffprobe（本来就要跑） | 精确 |
| 3 | 文件名解析 → TMDB search | 一次网络请求 + 打分 | R6 实测 hit@1 = 92% |

**第 1、2 级命中时不需要调 search，只需要一次 detail 请求拿元数据**，请求数从「3 次/片」降到「1 次/片」。这对 5 万文件的首次刮削是 15 万 → 5 万的差别。

**代价**：`probe` 阶段必须把 `format_tags` 一起收下来落库（不是只收流信息），否则匹配阶段要重跑 ffprobe。`probe` 结果本来就要缓存，加这几个字段不额外花钱。

**不做的事**：不解析 `Group`（压制组）、不从文件名里猜 `TMDB-1633056` 这种写法。前者无用，后者已被前两级覆盖。

解析器是纯函数（`domain/parse`），输入文件名，输出 `ParsedName` + `reason_code`。它的输出**直接决定 28 屏每行副标那句话**，所以要按契约的 `UnmatchedReasonCode` 枚举来组织，而不是按"解析成功/失败"两分：

- `EPISODE_WITHOUT_SERIES` — 有 `S02E04` 但没剧名
- `YEAR_OR_TITLE_AMBIGUOUS` — `2012.mkv`
- `TITLE_YEAR_OVERLAP` — 片名部分重合
- `YEAR_IS_RELEASE_TAG` — `[2019]` 是压制年
- …

每个 code 产出确定的 `reasonParams`，客户端查本地文案表拼句子。**服务端不拼中文**，这条贯穿全栈。

TMDB 查询要有本地缓存表（`tmdb_cache(tmdb_id, payload, fetched_at)`）。实测一条片子约 3 次请求（search + detail + credits），5 万文件约 15 万次；同一部剧的多集、同一部片的多个版本会反复命中同一个 `tmdb_id`，缓存直接把请求数压到与**作品数**同阶，而不是与**文件数**同阶。

### 4.3 缩略图

libvips 而不是 FFmpeg 截图：vips 对 JPEG/PNG 快一个量级，只有视频取帧才用 ffmpeg。策略是**取 20% 位置的一帧**（片头黑场和 logo 太多），失败回落 5%。

缩略图不落库，落盘 `<dataDir>/thumbs/<md_id前2位>/<md_id>.webp`（`dataDir` 默认在系统用户数据目录，可配置；**必须在本地盘**，原因同 DB），DB 只存相对路径。`thumbUrl` 是 API 层现签的（TTL 24h），不是持久 URL。

### 4.4 扫描与播放的冲突

扫描会持续读盘，播放也在读盘。**争抢的严重程度取决于卷的类型，而不是"是不是 NAS"**：

| 卷 | 症状 | 策略 |
|---|---|---|
| 机械盘（含 NAS 阵列） | 随机读寻道互相打断，播放会卡顿数秒 | 播放活跃时扫描降到 1 并发，IO 优先级最低 |
| SSD / NVMe | 基本无感 | 正常并发（按 profile 4/3/2/1） |
| SMB / NFS 网络盘 | 扫描会把链路打满，播放缓冲被挤 | 播放活跃时**暂停扫描**，不是降速——降速在网络盘上仍会占满带宽 |

"降优先级"要跨平台实现，没有统一的 `ionice`：

| 平台 | 做法 |
|---|---|
| Linux | `ionice -c3` + `nice 19`（子进程启动时设） |
| macOS | `taskpolicy -b` 或 `setiopolicy_np(IOPOL_TYPE_DISK, …, IOPOL_THROTTLE)`；`nice 19` |
| Windows | `SetPriorityClass(PROCESS_MODE_BACKGROUND_BEGIN)` —— 同时降 CPU 与 IO，是唯一有效的做法 |

**这是 30 屏「正在使用」状态的一个真实用途**，不只是展示：它决定了扫描要不要让路、转码要不要排队。

---

## 5. 播放子系统（最关键的一块）

### 5.1 直连 / remux / 转码 三分支

契约的 `PlaybackMode` 有三个值，但只有 `direct` 和 `transcode` 的边界是清楚的。中间的 `remux` 是决定移动端能不能用的那一条，而它恰恰最容易被实现成"省略掉"。

```
GET /play/{mediaId}
  │
  ├─ 会话已存在且未过期 → 复用
  │
  ├─ 客户端显式要求转码？ ──────────── yes ──▶ transcode
  │
  ├─ 网络 = 外网 且 height > externalMaxHeight(1080)？ ── yes ──▶ transcode
  │
  ├─ 容器/轨与客户端能力冲突？
  │    ├─ 编码不支持（HEVC 在无硬解的 Android）──▶ transcode
  │    └─ 仅容器不支持（mkv → AVPlayer）────────▶ remux  ← 关键分支
  │
  └─ 其余 ─────────────────────────────────────▶ direct
```

**`remux` 不是优化，是移动端唯一可行的路**：

- iOS 的 AVPlayer **不支持 mkv**，也不支持 DTS / TrueHD 音轨。
- 但只要视频编码是 H.264/HEVC、音轨能挑出一条 AAC/AC3，`ffmpeg -c copy` 转 fMP4 就是**纯拷贝**——单核可跑 10 倍速以上，1–2 秒出首片，几乎不耗 CPU。
- 如果用 `transcode` 处理这件事，等于为了换个容器把 2 小时视频重新编码一遍。这在移动端是灾难：要么等 40 分钟，要么撞 60 秒准入线被拒。

**契约 v1.3 已补完这三处的定义**（`PlaybackMode` / `PlaySource.protocol` / `GET /play` 规则 0）：

| 项 | v1.3 的最终规定 |
|---|---|
| **判定条件** | 四条同时成立：① 客户端已上报 `containers` ② 源容器不在其中 ③ `video.codec` 在 `codecs` 里为 `true` ④ 存在音轨其 `codec` 在 `audio` 里为 `true`。另有前置 `probe.ok=true` |
| **配额语义** | 不占外网转码那 1 路；与 `transcode` **共享** ffmpeg 进程池（上限 2），池满时优先级更高。不返回 `queuePosition`、不产生 `TRANSCODER_BUSY` |
| **传输形态** | `remux` **必须** `protocol: hls`，禁止 `http`；`source.container` 报 `fmp4` |

补的时候又挖出**第 4 个缺口**，比前三处更硬：**契约里没有让客户端在起播前上报能力的通道**。`ClientCaps` 此前只出现在 `POST /play/{sid}/error` 的请求体里——也就是**失败之后**才带上来。规则 0 要靠 `containers` 判断，却拿不到它。

解法是新增 `PUT /devices/{id}/caps`，由设备用自身 token 上报，服务端存最新值，`/play` 只读一行。**未上报则整条规则跳过，一律 `direct`**——「能力未知」自动退化成「先播再报错」。这个降级方向是刻意的：宁可多一次失败往返，也不要凭猜测拦在起播前。

顺带定了一条分界线，写进了 §5.1 的规则里：**`containers` 是确定性事实，可以参与起播决策；`hwdec` / `codecs` / `hdr` / `audio` 是概率性能力，只能用于失败后选降级方案。** 混用就等于把预判门禁从后门放回来。

### 5.2 「先播再报错」的实现落点

契约 0.7 定了这条，实现上有三个具体动作：

1. **起播不做探测**。`GET /play/{mediaId}` 读的是**扫描时存下的 probe 结果**，不现场跑 ffprobe。也就是说这个接口是纯 DB 读 + 签名，目标 p99 < 5ms。
2. **mpv 用 `hwdec=auto-safe`**，不用 `auto`。`auto` 失败会回落到软解而不报错，用户看到的是"能播但 CPU 100%、卡"；`auto-safe` 只选白名单内的硬解，失败就明确失败，才能进后面的诊断。
3. **失败识别在三处取并集**：
   - `MPV_EVENT_END_FILE` 且 `reason == error`
   - `MPV_EVENT_FILE_LOADED` 后 5 秒内 `time-pos` 不动（解码器"假活"）
   - mpv stderr 采样命中 `Could not open codec` / `Failed to initialize`
   
   第三条不能省：mpv 有些解码失败只打日志、不抛事件。

### 5.3 转码器：并发由 profile 决定 + 队列

```
   fallback / rendition 请求
        │
        ▼
   ┌─ transcodeQueue (持久化在 tasks 表, state=queued) ─┐
   │  估算: 剩余时长 ÷ 本机实测编码倍率 ÷ 可用槽位      │
   │        = estWaitMs（倍率来自 host_profile，非常数）│
   └──────────────────┬────────────────────────────────┘
        estWait ≤ 60s │ estWait > 60s
        ▼             ▼
    202 + queuePos   409 TRANSCODER_BUSY
        │
        ▼
   semaphore(N)  ──▶ ffmpeg 子进程 (独立进程组，便于整组 kill)
        │              N 由 host_profile.transcode_concurrency 决定：
        │              S=3 / M=2 / L=1 / X=1(仅 remux)
        │              且受"礼貌模式"下调（见下）
        ├─ 分片就绪回调 → hub 推 task.progress{firstSegmentReady:true}（跳过 500ms 节流）
        └─ 退出 → 清理 / 保留 stderr 尾部 8KB
```

**上一版写的是 `semaphore(1)`，那是把 NAS 的能力当成了普遍事实。** 在带 NVENC 的台式机上硬限 1 路是浪费——它能同时转 3 路 1080p 而 CPU 占用不到 30%。所以槽位数和编码器都从 `host_profile` 读：

| 档 | 编码器优先级 | 槽位 |
|---|---|---|
| S | `h264_nvenc` → `h264_amf` → `h264_qsv` → `libx264` | 3 |
| M | `h264_qsv` → `h264_videotoolbox` → `h264_vaapi` → `libx264` | 2 |
| L | `h264_qsv`/`vaapi`（若有）→ `libx264` | 1 |
| X | 仅 `-c copy`（remux），不做编码 | 1 |

**逐级回落，并且回落要记日志**：硬编不可用时（驱动更新、远程桌面会话、显卡被独占）不能直接失败，要退到下一级。这跟 mpv 用 `hwdec=auto-safe` 是同一个思路——**失败要明确，但别在能降级的时候失败**。

### 礼貌模式（C4 的落点，这是"可能是电脑"带来的新东西）

主机是专用 NAS 时，转码可以跑满；主机是用户正在打游戏的台式机时，跑满就是事故。三条开关，都写进设置：

| 触发 | 行为 |
|---|---|
| 电池供电 | 不启动新转码；正在播的会话不打断（已生成的分片还够撑一会儿），排队等插电 |
| 前台有全屏应用 / 用户输入活跃 | 槽位降到 1，进程降优先级（§4.4 的三平台做法） |
| 配置了转码时段（默认不限） | 时段外排队，29 屏的定时卡可读 |

**降级而不是拒绝**：用户在外网点"转为 1080p"，如果服务端因为"主人正在用电脑"直接给 409，那是不可接受的。正确做法是**接受请求、进队列、告诉他大概等多久**，然后在条件允许时开始。这跟 §5.3 上面那条 60 秒准入线是同一个逻辑的两个层级。

**用子进程而不是 cgo 调 libav**。理由：转码是唯一会因文件损坏而崩的环节，子进程崩了不影响服务端；而且 `ffmpeg` 的 `-progress` 输出天生就是可解析的进度源，不必自己算帧。

`ffmpeg` 必须用 `Setpgid` 起在独立进程组：取消任务时 `kill(-pgid, SIGKILL)` 才能连它 fork 的线程一起收掉，否则会留僵尸进程吃 CPU。**Windows 上没有 `Setpgid`**，对应做法是 `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F /PID`（`/T` 才会杀子树）。

**HLS 参数**（需 spike 校准，这里给起点）：

```
-f hls -hls_time 4 -hls_playlist_type event
-hls_segment_type fmp4            # iOS 必需，MPEG-TS 在 AVPlayer 上不认
-hls_flags independent_segments+temp_file
-start_number <startSegment>
-ss <alignedStartSec> -copyts
```

`-start_number` 与 `actualStartMs` 的对应关系是续播精度的关键，必须由同一个函数计算，不能一处算段号一处算毫秒。

### 5.4 GC

一个 30 秒的 ticker，做四件事：

| 对象 | 条件 | 动作 |
|---|---|---|
| 播放会话 | `now - last_seen_at > 90s` | 置失效，停转码 |
| 转码进程 | 会话已失效 | kill 进程组，删临时分片 |
| 诊断包 | `now > expires_at`（30 分钟） | 删行 + 删日志文件 |
| 分片目录 | 会话失效 | 删 `hls/<ps_id>/` |

GC 只跑一个 goroutine，通过 `store` 的写句柄串行执行——它和扫描器抢的是同一把写锁，不能并发。

---

## 6. 事件总线（Hub）

契约 `/api/stream` 单连接同时承载任务与设备事件，实现上是标准 pub/sub：

```go
type Hub struct {
    mu   sync.RWMutex
    subs map[string]*sub          // deviceId → 订阅者
    ring [1024]frame              // 环形缓冲，编码后的字节，供 since 补发
    seq  atomic.Uint64
}
```

四个实现细节，都对应契约里的硬约束：

1. **一个 device 只允许 1 条连接**。新连接进来直接 `Close(4000, "superseded")` 旧的。这是契约写的，落地时要先关旧再注册新，顺序反了会出现"两个都活着"。
2. **`seq` 在写入 ring 之前分配**，且必须在持锁区内完成，否则并发写会导致 seq 与 ring 位置错位——重连补发会补出乱序帧。
3. **背压策略按事件类型分**：`task.progress` 满了丢最旧的；`task.state` / `task.finished` / `device.*` 满了**阻塞**（宁可让产生方慢一点，也不能丢状态迁移）。这一条不写进代码注释就一定会被后来者改成统一丢弃。
4. **`firstSegmentReady` 变化时绕过节流**。节流的实现如果是"每 500ms 刷一次最新值"，这句话就得单独开一条立即推送路径，不能用同一把节流锁。

`snapshot` 帧在握手后立即推，内容是 `{tasks, devices}` 的完整快照。客户端拿到就丢掉本地所有状态——这条让"断线重连"变成"重新加载一次首页"，不需要客户端实现增量合并。

---

## 7. 客户端

### 7.1 桌面端（Tauri 2 + React 19）

| 层 | 选型 | 理由 |
|---|---|---|
| 壳 | Tauri 2 (Rust) | 体积 ~10MB vs Electron ~150MB；需要 Rust 侧直接管 mpv 进程 |
| UI | React 19 + TS + Tailwind v4 | 设计稿是 32 屏的高密度信息界面，Tailwind 的约束比 CSS-in-JS 快 |
| 状态 | TanStack Query（服务端态）+ Zustand（播放器态） | 播放位置每 250ms 变一次，不能进 Query 缓存 |
| 长列表 | TanStack Virtual | 28 屏待整理可能几千行 |
| 播放器 | libmpv（JSON IPC，unix socket） | 唯一能可靠播 HEVC 10bit HDR + TrueHD 的桌面方案 |
| 地图 | MapLibre GL | 一期不用，为「回忆」地图预留 |

**mpv 接入方式**：Tauri 的 Rust 侧 spawn `mpv --input-ipc-server=/tmp/lumen-mpv.sock`，宽高由窗口传给 mpv（`--wid` 嵌入）。前端通过 Tauri command 读写属性，事件由 Rust 侧转成 Tauri event 推给前端。

不要用 `--wid` + 前端 `<video>` 混用：mpv 走的是原生窗口，和 WebView 是两个图层，遮罩/弹层会被它盖住。设计稿 24 屏那个设置弹层必须用 mpv 的 `--osc=no` + 前端自绘 UI，弹层出现时把 mpv 画面缩到左侧——这正是设计稿里它占 70% 宽度的原因。

### 7.2 移动端（React Native）

必须说清楚一件事：**RN 不能内嵌 libmpv**（没有可靠的 iOS/Android binding）。移动端用的是系统播放器：

| 平台 | 播放器 | 能播什么 |
|---|---|---|
| iOS / iPadOS | AVPlayer | mp4/mov/fMP4 HLS；**不支持 mkv**；不支持 DTS/TrueHD |
| Android | Media3 ExoPlayer | mkv 可以，但 HEVC 依赖设备硬解 |

这就是 5.1 里 `remux` 分支存在的全部理由。移动端的能力上报（`ClientCaps`）来自原生解码器查询，不是猜的：iOS 侧 `AVURLAsset.isPlayable` + `AVPlayerItem.videoComposition` 试探，Android 侧 `MediaCodecList`。

### 7.3 契约的单一来源

`docs/lumen-openapi.yaml` 用 `oapi-codegen` 生成：

- Go 侧：`internal/api/dto.gen.go`（只生成类型 + 严格服务端 handler 接口）
- TS 侧：`client/src/api/types.gen.ts`

**生成的只有类型，HTTP 调用手写**。理由：`FallbackRequest` 用了 `anyOf` 表达"两个凭据二选一"，多数 codegen 会生成"两个都必填"的客户端；自动生成的调用层会把这类错误藏得很深，手写一层构造器反而能显式校验。

---

## 8. 网络与鉴权

| 层 | 方案 |
|---|---|
| 外网入口 | Tailscale（主机装 `tailscaled` / Windows 客户端，客户登录同一 tailnet）。**不映射公网端口** |
| 传输 | 局域网走明文 HTTP（同一广播域）；Tailscale 内已加密，不再套 TLS |
| 设备身份 | 配对码 → `deviceToken`（随机 32B，服务端只存 SHA-256），`X-Lumen-Device` 头 |
| 媒体 URL | HMAC-SHA256 签名，含 `expiresAt`，一次性会话绑定 |
| 权限 | 每个 handler 显式声明所需 `DevicePermission`，中间件校验 |

两个必须显式做的检查：

1. **`permissions` 逐项校验，不做隐含推导**。契约 v1.2 定了三档互不隐含，实现上就是每个路由挂 `requires: manage_library` 之类的注解，中间件按位判断。不要在 `domain` 里写 `if has(manage_devices) { 也允许删片 }`——那正是这次拆权限要消灭的东西。
2. **媒体 URL 的签名绑 `sessionId`**，不是绑 `deviceId`。会话失效后 URL 立刻作废，这是"撤权 60 秒内断开"能兑现的技术前提。

---

## 9. 风险与验证顺序

这一节是本文档里最该先读的。下面 6 条里任何一条不成立，方案要改。

### R1 · 实时转码能力跨主机差 10 倍（**最高风险，但已从"架构决策"降级为"运行时分支"**）

契约里 `estStartupMs ≈ 8000` 是**基于 NAS 写的**。v1.5 起脚注不再写死秒数和核心数（31/32 屏已改为「约 {N} 秒启动，不影响正常使用」），但**服务端算这个数的依据仍然必须按主机分档**——文案脱敏了，参数没脱敏。同一片源在三种主机上：

| 主机 | 4K HEVC → 1080p 1080p 实时倍率 | 结论 |
|---|---|---|
| NAS（J4125 级，软编） | **0.3–0.6×** | 边转边播不成立，必须预转代理版 |
| 桌面核显（QSV / VT / VAAPI） | **2–5×** | 实时可行，1–2 路 |
| 桌面独显（NVENC / AMF） | **6–12×** | 实时很轻松，2–3 路，CPU 几乎不占 |
| 老机器 / 虚拟机 / 无硬编 | **< 0.3×** | 只做 remux，转码一律走代理版 |

**关键结论：A/B/C 不再是"给这个项目选一条路"，而是"每台主机在启动时自选一条"。**

| 分支 | 触发条件（来自 host_profile） | 行为 |
|---|---|---|
| **A · 实时转码** | `encoder` 有可用硬编，或软编 ≥ 1× | 常规路径，`estStartupMs` 由实测倍率算，不再写死 8000 |
| **B · 预转代理版** | 软编 0.3–0.6×，且无硬编 | 外网请求遇到 4K 源时排一个"代理版"任务，返回 `mode:transcode` + 代理未就绪；生成完通知 |
| **C · 降档兜底** | 软编 < 0.3× 或探测失败 | 最高只给 720p，`-preset ultrafast` |

这样做的直接好处：**台式机用户不会被 NAS 的保守参数拖累，NAS 用户也不会拿到"8 秒起播"这种兑现不了的承诺。**

**代价：`estStartupMs` 必须从常数变成计算值。** 这是契约层的真实变化——它目前是文档里的一个固定数字，落地时应该是 `f(实测倍率, 目标分辨率, 源时长)`。客户端拿到的仍然是数字，但服务端不能再写死 8000。

**B 分支仍会改契约**（只有 L/X 档会走到，但必须实现）：`GET /play` 需要能表达"代理版未就绪"，`tasks` 多一类常驻预转任务。这条和 v1.3 已定的东西不冲突，只是补一个分支。

**验收标准（不再需要问型号）**：在一台目标机器上跑
```
ffmpeg -i <4K样本> -t 30 -vf scale=1920:-2 -c:v libx264 -preset veryfast -f null -
```
把这段的耗时换算成倍率，和 `host_profile.chain_fps_1080` 折算出来的倍率对比。两者一致说明探测逻辑对，不一致就是探测实现有 bug。

**脚本**：`spikes/r1-transcode/bench.sh --sample <真实4K片源>`，一次跑完硬编真编验证（七个编码器逐个试编）、
软编全核与单核、tier 分档、槽位、`est_startup_ms` 预估，输出 `r1-result-<host>.json`。

**但它的测量方法不能照搬到 `host_probe` 里**：脚本取 ffmpeg 自报的 `speed=`，而实测短片段里
这个值是 `0.0`（进度行只在收尾输出一行），且它算的是"距上次上报的增量"。
正式实现改成"扣基线 + 自算帧率"，见 §1.1 的三个反直觉点。脚本保留，用于人工重测与交叉对照。

### R1 实测（i7-9700K 8C/8T + RTX 2080 Ti，真实片源，2026-11-11）

样本：`All.Wishes.Come.True.2026.2160p.YK.WEB-DL.H.265.DV.HFR.HQ.DTS5.1`，
**3840×1608 / 60fps / HEVC Main10 / Dolby Vision Profile 5 / 30.6 Mbps / 143 分钟 / 32.9 GB**。

下表是**修正后**的数（8 秒窗口 × 两个取样点取小，扣掉 0.85 秒打开与 seek 开销）：

| 路径 | 吞吐 | 相对 60fps 实时 |
|---|---|---|
| 仅解码（软解，无滤镜） | 171.1 fps | 2.85× |
| 1080p `h264_nvenc`（NVDEC + NVENC，全链） | **480.5 fps** | **8.01×** |
| 2160p `h264_nvenc`（全链） | 172.6 fps | 2.88× |
| 1080p `libx264 -preset veryfast`（全链，软解） | 76.2 fps | **1.27×** |
| 2160p `libx264`（全链，软解） | 26.7 fps | 0.45× |
| **DV P5 → SDR 1080p（libplacebo + nvenc）** | **83.4 fps** | **1.39×** |
| `-c:v copy` 转封装 | 8257 fps | 137.6× |
| 3 路并发 1080p `h264_nvenc` | 每路 152–159 fps | 2.5–2.65× |
| 5 路并发 1080p `h264_nvenc` | 全部成功，无会话数报错 | — |

**这份表和第一版差了一倍，原因值得记下来。** 第一版用"总帧数 ÷ 总墙钟"：同一个 60 秒命令量出
14.15 秒（254 fps），而今天同样的命令量出 8.16 秒（441 fps）。差的 6 秒不是算力变化，
是**固定开销被摊进了帧率**——打开 33 GB 的 mkv 并 seek 到 120 秒处要 0.85 秒以上，
第一版那一次还叠加了当时的系统负载。**测量方法错了，结论就会错得很有说服力。**
现在用"基线运行扣除 + 两取样点取小"，并用 8 秒与 60 秒两种窗口交叉验证（480 vs 494，差 3%）。

**三条原假设被推翻，都要改：**

**① 倍率是相对帧率算的，吞吐才是绝对的。** 同一台机器 1080p 硬编在 24fps 片源上是 20×，
在 60fps 的 HFR 片源上只剩 8×——因为每秒要处理 60 帧而不是 24 帧。
所以 `host_profile` 里存倍率是不够的，**必须存绝对吞吐（fps）**，由 `fps ÷ 片源帧率` 现算倍率。
否则一部 60fps 的电影会被判定成"轻松可转"，实际余量只有标称的一半。

**② "分别测解码 D 与编码 E，按 `1/(1/D+1/E)` 合成"这条改法，只在软件链路成立，必须收回。**
它假设解码与编码串行排队。软件编解码确实如此（解码 129.8、全链 67 ⇒ 纯编码 ≈138，残差 <3%）；
但**硬件解码与硬件编码跑在不同引擎上，是重叠的**——本次实测全链 480 fps 比"仅解码 171 fps"还高，
套那个式子会算出负数。
所以 v3.7 选的那条"便宜做法"在硬件机器上是错的，正确做法是：
- **直接测全链**（现在就是这么做的），它是唯一对两种拓扑都成立的量；
- 解码吞吐单独测，但**只用于诊断**（区分"解码慢"还是"编码慢"），不参与合成。

仍然成立的部分：验收命令「1080p 片段 → 1080p 编码」测的只是编码那一段，会高估——这一点没错，
所以不能用它当分档依据。

**③ 「降分辨率就能救」对 DV 不成立。** DV→SDR 的 tonemap 是**逐像素**成本，与输出分辨率无关：
720p 输出 72.1 fps、1080p 输出 69.4 fps，几乎一样（第一版测得，本次未重测）。
全链 83.4 fps 而仅解码 171 fps，说明瓶颈在 tonemap 而不是编码——
在 60fps 片源上只有 **1.39×**，第二路必然掉帧。
所以"降档兜底"那一档对 DV 片源无效：720p 一样跟不上第二路。
DV 片源只有两条出路——**客户端自己能播 DV（直连）**，或**预转一份 SDR 代理版**
（1.39× 实时，一部 143 分钟的电影要跑约 1 小时 45 分）。
并发上限因此不是"档位 × 槽位"，而是**每台机器同时只允许 1 路 tonemap**，其余排队并如实显示预计等待。

**并发不是许可问题**：5 路 NVENC 同时开没有触发任何会话数错误，且聚合吞吐反而上升
（单路 480 fps → 3 路合计约 474×3）。消费卡的会话数限制在这代驱动上不构成约束，
`transcode_concurrency` 应由**吞吐**推导，不是由许可数推导。
**但吞吐推导出来的数会被档位上限截断**：这台机器算出 14 路，实际放 3 路——
再往上加，用户在用的这台机器就会被转码拖慢（C4）。


### R2 · macOS 上 libmpv 的分发

libmpv 是动态库。Tauri 打包 macOS 应用时，未签名的 dylib 会触发 Gatekeeper 拦截；要过公证还得把 dylib 一起签进 bundle。**这件事有大量踩坑**（`@rpath`、`install_name`、arm64/x86_64 双架构）。

**先验证**：一个最小 Tauri app 能否在 Apple Silicon + Intel 上都成功启动并加载 libmpv、并播一个 4K HEVC 10bit HDR 文件。这也顺带验证 VideoToolbox 硬解这条路是否通。

如果不通，桌面端的退路是**用系统 mpv**（用户自行安装），代价是分发体验变差，但能让一期跑起来。

**检查单已就绪**：`spikes/r2-mpv/CHECKLIST.md`，把「A 分发」与「B 播放」拆成两个独立结论
（A 不过可以退回用户自装 mpv，B 不过是硬解路线本身有问题，两者退路不同）。
里面记了三个已经知道答案的坑：`disable-library-validation` entitlement 必须加、
dylib 签名顺序不能反、**Windows 上 mpv 原生层会被 WebView 遮挡**（这条直接决定播放器 4 屏怎么做）。

### R3 · mpv 的失败信号是否够用

"先播再报错"完全依赖"能可靠感知失败"。mpv 的错误上报有三处（见 5.2），但实际覆盖度要实测：拿一批畸形文件（截断的 mkv、错标的 mp4、坏的音轨）逐个播，看哪条路径下三类信号都能触发。

**先验证**：准备 10 个"应该失败"的样本，跑一遍，统计漏报率。漏报意味着 31/32 屏永远不出现，用户对着黑屏。

**脚本已就绪**：`spikes/r3-failure/make-samples.sh` 造 12 类畸形样本（0 字节、只有文件头、
截断一半、缺尾 1KB、文件头被覆盖、文本改名、纯音频、纯视频、音轨损坏、扩展名骗人、mkv 截断、多音轨）。
`classify.py` 把它们过一遍，得出「容器可读 / 视频可解 / 音频可解」三维事实并映射到 `reasonCode`。

**已经改掉一个定义错误**：`playable` 不能定义成"所有流都健康"。视频正常但音轨损坏的文件，
按"全流健康"算就是不可播——但契约给的是 `AUDIO_CODEC_UNSUPPORTED` + `switch_audio`，
用户点一下就能救回来。正确定义是**"能不能出画面"**。这个错会让 31 屏把可救的文件当成整片失败。

### R4 · SQLite 写竞争

扫描（批量插入）+ 进度回写（每 10 秒）+ 任务状态（频繁）+ GC，四个写源共用单写连接。`SetMaxOpenConns(1)` 保证不锁死，但队头阻塞可能让进度回写延迟。

**先验证**：扫描 5 万文件的同时，`PUT /progress` 的**最坏**延迟。可接受线 50ms。

**实测（2026-11-11 · Windows / SSD / Python sqlite3，脚本 `spikes/r4-sqlite/bench.py`）**

| 单事务行数 | p50 | p95 | p99 | **max** |
|---|---|---|---|---|
| 50 | 0.1 | 0.1 | 0.2 | **0.5 ms** |
| 500 | 0.1 | 0.1 | 0.3 | **7.3 ms** |
| 5000 | 0.1 | 0.3 | 0.7 | **11.5 ms** |
| 50000（全库一个事务） | 0.1 | 0.1 | 0.1 | **72.4 ms** ✗ |

**判据从 p99 改成 max。** 这不是措辞调整——全库一个事务时 p99 只有 0.1ms，看起来完美，
而同一组的 max 是 72.4ms，已经用户可感。队头阻塞的表现是"绝大多数很快 + 偶发一次很慢"，
p99 恰好把那个尖峰稀释进 1% 的分母里。**任何"排队"类问题都不能用 p99 验收。**

**落地结论**：单次事务 ≤ 500 行（max 7.3ms，留 7 倍余量）；ffprobe 必须在锁外。
这两条合起来，队头阻塞就不存在了——真正让进度回写变慢的从来不是"只有一个连接"，
而是"有人拿着那个连接在做长事务"。

### R5 · iOS 无法直连 mkv（已在 5.1 处理，但需确认）

`remux` 分支是设计上的解法，但要验证：`ffmpeg -c copy -f hls -hls_segment_type fmp4` 对 mkv 源（含多条音轨、PGS 字幕）是否稳定，PGS 字幕必须丢弃（AVPlayer 不支持），音轨要挑出第一条兼容的。

**先验证**：10 个 mkv 样本（含 PGS、TrueHD、DTS-HD）批量 remux，看成功率与首片耗时。

**脚本已就绪**：`spikes/r5-remux/remux-matrix.sh --dir <片库目录>`，逐个走
`-c copy -sn -f hls -hls_segment_type fmp4`，记录成功率、**首片毫秒数**、丢弃了什么。
`--auto` 能造出多音轨 / srt / EAC3 / TrueHD / HEVC 五种变体做冒烟，
但 **PGS 与 DTS-HD 无法用 ffmpeg 生成**（没有对应编码器），只能从真实片库取样。

脚本会额外区分一种情况：**唯一音轨是 TrueHD/DTS 时，"纯 copy"做不到**，
必须视频 copy + 音频转 aac。这时起播耗时接近转码而不是 remux，
`PlaybackMode` 应记 `transcode`——记成 `remux` 会让 `estStartupMs` 承诺错。

### R6 · TMDB 刮削的速率与配额

一次全库扫描可能产生上万次查询请求，TMDB 免费档有速率限制。这条不是"技术能不能跑通"，而是"跑通要花多少时间"。

**已验证**（2026-11-11，真实 token，24 条剧名 + 冷门抽样，明细见 `spikes/r6-tmdb/`）：

| 结论 | 数据 | 对设计的影响 |
|---|---|---|
| 中文覆盖不是问题 | hit@1 **92%**、hit@5 **96%**、中文名 **100%**、中文简介 **100%** | 28 屏按现设计可用，不需要"手动填 TMDB ID"入口 |
| **脏文件名彻底搜不到** | `Interstellar.2014.2160p.UHD.BluRay...` → `total_results = 0`，三条全 0 | 文件名解析是**硬前置**不是优化；兜底展示只能是文件名本身 |
| **匹配器不能用字符串相等** | `昼颜` 的 zh-CN 名是 `昼颜：工作日下午3点的恋人们` | 译名比俗称长是常态 → 用 token 覆盖率打分，不因长度差异降权 |
| **短标题被综艺抢占** | `信号` top1 = `心动的信号`(综艺)，正确项 rank 2 | 排序必须吃「年份 + 类型 + 原名语言」三类信号，只靠 popularity 必错 |
| **回退目标是原名，不是英文** | 两部韩剧在 `zh-CN` 返韩文原名，`en-US` 才有英文 | 标题行必须兼容任意文字；不能假设"没中文就给英文" |
| 冷门条目缺一半 | 中文名 1/10、中文简介 1/10、海报 6/10 | 空简介、空海报都要有占位设计 |
| 首扫很慢 | p50 647ms，单条 3 请求 → **5 万文件串行 ~27 小时**，8 并发 ~3.4 小时 | 28 屏不能"刮完才出"，必须**先入库可播、后台补元数据** |
| 速率上限测不出 | 本机最高 14.7 次/秒未 429，**并发 20→50 吞吐不涨**（瓶颈在出网代理） | 契约不写死速率常量，用自适应令牌桶；`Retry-After` 不当作依赖 |

**一处需要跟着改的契约项**：`tmdb_cache` 除 TTL 30 天外要有 **180 天硬上界**（条款要求不超 6 个月），
并配一个「清空元数据缓存」入口——换 key 或停用刮削时用户得有地方清空。

**一处自我更正**：此前我反复说「28 屏的 38 个候选要改成 20」，这是**记错了**。
契约里的 `candidateCount: 38` 指**待批量匹配的条目数**（`batch-match` 一次处理 38 项），
不是每个条目的候选数；设计稿 28 屏右栏本来只放 3 个候选，`/tmdb/search` 的 `limit` 上限也已经是 20。
所以候选数**不需要改**——只需要在契约里写明 `/unmatched/{id}/candidates` 单页硬上限 20、不翻页。

**仍未验证的一条**：目标机（家宽直连 / NAS）能否访问 `api.themoviedb.org` 与 `image.tmdb.org`。
本机全部流量经出网代理，**这个数只能在你实际部署的那台机器上测**。
若不通，NFO 优先 + 可配代理 + 文件名兜底三层降级就是唯一出路。

### R7 · 跨平台文件系统的三个静默差异（新增，因"媒体可能在任何盘上"）

这三条都不会报错，只会让数据慢慢变错：

| 差异 | 症状 | 先验证 |
|---|---|---|
| **Unicode 规范化**（macOS NFD vs Win/Linux NFC） | 同一个中文文件名在两平台字符串不等 → 换平台后整库重扫、去重失效 | 在 macOS 上建一个含「旅」「é」的文件名，`xxd` 看字节；Go 里 `norm.NFC` 前后对比 |
| **大小写敏感性** | 同一批文件在 Win 上一套结果、在 Linux 上另一套 | 在 NTFS 上建 `A.mkv` 和 `a.mkv`，看能否共存；导出 `foldCase` 后的 key 是否重复 |
| **盘符/挂载点漂移** | 拔插移动硬盘后 `D:`→`E:`，所有绝对路径失效 | 拔插一次，确认 `volume_key` 不变而 `mount_hint` 变了 |

**先验证**：三平台各建一个 500 文件的小库，导出 `path_key` 集合，三者应当完全一致。不一致就说明归一化没做对，而这个问题在上线后极难定位（表现为"记录莫名重复"）。

**实测（2026-11-11 · Windows 11 / NTFS · 脚本 `spikes/r7-path-norm/probe.py`，500 个样本）**

样本造了 500 个，Windows 上实际建成 494 个（6 个名字该平台根本不允许），
探针最终读到 488 个。丢失的 12 个分成四类，**每一类都不会报错**：

| 类型 | 数量 | 具体表现 |
|---|---|---|
| `case_collapsed` | 6 | `Movie.mkv` 与 `movie.mkv` 只能存在一个。写入第二个时**成功返回**，但名字仍是第一个的 |
| `win32_trimmed` | 2 | 尾随空格和尾随点被静默裁掉：`trailing dot.mkv.` → `trailing dot.mkv` |
| `ads_shadowed` | 1 | `colon:name.mkv` 被解释成 NTFS 数据流，磁盘上只留下一个叫 `colon` 的文件 |
| `platform_skipped` | 6 | `? " \| * < >` 这些字符直接 `EINVAL`，**但只在这台机器上**——同一个库搬到 ext4 就都能建 |

三条可直接落地的结论：

1. **NFC 归一化是必需的，不是防御性的。** 本机 `raw sha ≠ nfc sha`，9 个名字在归一化前后不同——
   NTFS 原样保存 NFD 字节，不做归一化的话同一个中文名在两个平台上算出两个 key。
2. **`foldCase` 必须用 Unicode 大小写折叠，不能用 `strings.ToLower`。** 实测 `ﬁ`（U+FB01）在两个函数下结果不同：
   `ToLower` 保持 `ﬁ`，`casefold` 变成 `fi`。Go 里要用 `x/text/cases.Fold()`，否则 `ﬁlm` 和 `film` 会成为两条记录。
3. **入库前必须拒绝含 `:` 的 relPath**（Windows 上），或统一转义。否则扫描器读到 `colon`、
   数据库里存着 `colon:name.mkv`，一条记录永久 pending + 一个孤儿文件，而且日志里一句错误都没有。

**待补**：Linux（ext4）与 macOS（APFS）各跑一遍，三份 `probe-*.json` 喂给 `probe.py compare`。
预期 ext4 上 `case_collisions` 不再是 0——那正是"换平台后记录翻倍"的现场。

### 建议的 spike 顺序

```
Week 0（1 周，不写产品代码）
  D1    R7 三平台路径归一化一致性 ← 便宜且能立刻证伪    [Windows 已跑 ✓  待 Linux/macOS]
  D1-2  R1 在目标机上实测转码倍率（含 host_probe 原型）  [脚本就绪，待真机 + 真片源]
  D3    R2 libmpv 在 macOS / Windows 跑通 4K HEVC HDR   [检查单就绪，需一台 Mac]
  D4    R3 失败样本矩阵                                [脚本就绪，需 ffmpeg]
  D5    R5 mkv remux 矩阵                              [脚本就绪，需真实片源]
  D6-7  R4 并发压测 + SQLite 基准                      [已跑 ✓  判据已从 p99 改为 max]
```

脚本都在 `spikes/`，入口见 `spikes/README.md`。R7 与 R4 只依赖 Python，**当天就能出结论**；
R1/R3/R5 只要有 ffmpeg；R2 要一台 Mac；R6 要 TMDB token（申请可能等审核，建议第一天就去申请）。

**R7 提到最前面**，因为它最便宜（半天）且失败后果最隐蔽——它不会让程序崩，只会让数据在换平台时悄悄错掉。

**R1 紧随其后**。它原来被当成"决定要不要走 B"的一锤子买卖，现在改成"验证探测逻辑是否正确"：在**一台**目标机上跑，拿到实测吞吐，与 `host_profile.chain_fps_1080` 比对。剩下两档（强/弱机器）靠逻辑覆盖，不需要真的买三台机器。

---

## 10. 目录结构

```
lumen/
├── cmd/lumen/main.go
├── internal/
│   ├── api/        router.go  middleware.go  handlers_*.go  dto.gen.go
│   ├── domain/     parse.go  match.go  play_decision.go  diagnose.go  perm.go
│   ├── store/      db.go  media.go  unmatched.go  tasks.go  playback.go  migrate/
│   ├── pipeline/   queue.go  scan.go  scrape.go  thumb.go  transcode.go
│   ├── media/      ffprobe.go  ffmpeg.go  hls.go  vips.go
│   ├── hub/        hub.go  frame.go
│   └── lib/        ulid.go  sign.go  clock.go
├── web/                     # Tauri 打包的前端
│   ├── src/api/             # types.gen.ts + 手写 client
│   ├── src/player/          # mpv IPC 封装
│   └── src/pages/           # 32 屏对应
├── docs/                    # 本文件、契约、openapi
└── Makefile                 # codegen / test / build / migrate
```

---

## 11. 一期范围

| 模块 | 一期 | 二期 |
|---|---|---|
| **主机** | **Windows + Linux**（NAS 同时也是 Linux），单二进制；数据目录与媒体卷可分开配置 | macOS（打包/公证成本高，见 R2） |
| 扫描 | 电影 + 剧集；本地卷 + 网络卷（网络卷落 `poll` 模式） | 照片、音乐 |
| 刮削 | TMDB 自动 + 手动修正 | 音乐指纹 |
| 播放 | 直连 / remux / 转码，断点续播；**能力由 host_probe 决定** | 多端同步播放 |
| 客户端 | 桌面（Tauri，Windows 优先，macOS 紧随） | 移动、tvOS |
| 页面 | 首页、详情、媒体库、播放器、28/29/30/31/32 | 照片 12 屏、音乐 5 屏 |

**一期不做移动端（已定）。** 理由是 R5——iOS 的 mkv 问题需要 remux 通道先稳定，而 remux 通道又要靠桌面端先把播放链路跑顺。先做移动端会把两个未验证的东西叠在一起调，出问题时分不清是 remux 不稳还是 RN 这层的问题。

**一期必须做 Windows，这件事之前漏了。** 上一版把 macOS 排在一期、Windows 没提，隐含假设是"服务端在 NAS（Linux）、客户端在 Mac"。但媒体目录完全可能就在一台 Windows 台式机上——那时服务端也跑在 Windows。路径归一化（§1.2）、进程组（§5.3）、IO 优先级（§4.4）在这三个平台上是**三套实现**，Windows 排第一是因为它的坑最多（盘符、长路径、大小写、进程组）。

工时（按 1 人全职）：

| 阶段 | 内容 | 周 |
|---|---|---|
| S0 | spike（R1–R7，见 §9） | 1 |
| S1 | store + 迁移 + 卷/路径归一 + 扫描 + 解析 | **2.5** |
| S2 | 刮削 + 28 屏接口 | 2 |
| S3 | 播放 API + 转码/remux + 诊断 + host_probe | 2 |
| S4 | Tauri 壳 + mpv + 4 屏 UI | 3 |
| S5 | 联调 + 打包（Win/Linux） | 1.5 |
| S2b | 组 E 设置（07 屏）：凭据校验 / 代理 / 缓存清理接口 + 设置区 UI | **+0.5** |

合计 **12.5 周（已确认）**。相对最初的 11 周多出 1.5 周，全部来自两处具体的加价：

- **+1 周 通用化**：S1 的路径归一化 +0.5、S5 的 Windows 打包 +0.5。**这不是可选项的加价，是"服务端可能跑在用户电脑上"这个前提的必然成本。**
- **+0.5 周 S2b**：07 屏原来只有一行静态文字，现在变成四个分类、四张卡（媒体库路径 / 扫描与刮削 / 播放与字幕 / 账户与设备）。含 3 个端点、4 种失效状态、一个配置界面。它不能摊进 S2——S2 是后台抓取，S2b 是"写配置 + 立刻验证 + 失败要能说清是网络还是 key"，两件事的出错方式不一样，混在一起估会把两者都低估。

**S2b 里有一条会腐蚀设计的细节，单独记一笔**：07 屏左栏是**段落索引**（点击滚动到锚点），不是页面切换。所以四个分类必须在同一页上都有内容——否则点了没反应。这不是视觉问题，是"控件承诺了一个不存在的目的地"。

---

## 12. 明确不做的事

| 不做 | 原因 |
|---|---|
| 用户系统 / 多租户 | 家庭场景，设备即身份 |
| 数据库服务化（Postgres） | 单机 + 万级数据，SQLite 更快更省 |
| 容器编排 | 单二进制直跑（systemd / launchd / Win 服务）；NAS 上 Docker 是额外故障面，且会挡住 `/dev/dri` 透传 |
| ~~实时转码多路并发~~ | **已推翻**。并发数改由 `host_profile` 决定（S=3 / M=2 / L=1）。强机器上硬限 1 路是浪费，弱机器上本来就只有 1 路，没必要写进"不做" |
| 客户端本地缓存库 | 服务端是唯一真相，客户端不落盘（进度除外） |
| 服务端拼中文文案 | 契约 0.6 已定，违反会导致文案散在 Go 里无法迭代 |
| 把 DB 放媒体目录旁边 | WAL 在 SMB/NFS 上会静默损坏，见 §1.2 ①；必须单独配置 dataDir |
| 跟随符号链接扫描 | PC 上链接成环会把扫描变成死循环，且 Windows 上 junction 行为不一致；一期一律跳过 |

---

## 13. 决策状态

| # | 事项 | 状态 | 说明 |
|---|---|---|---|
| 1 | `remux` 三处定义 | **已定，契约 v1.3 已落地** | 补的过程中挖出第 4 个缺口（能力上报无通道），一并补了 `PUT /devices/{id}/caps`。详见 §5.1 |
| 2 | 主机形态 | **已定：不预设，运行时探测** | 不再分"NAS 版/桌面版"，改成 `host_probe` + 四档 profile（§1.1）。A/B/C 从架构决策降级为运行时分支（§9 R1） |
| 3 | 一期砍移动端 | **已定：砍** | 一期只做桌面端；移动端留二期。若改主意，S0 需加"iOS remux 通道验证"，+2 周 |
| 4 | 一期平台 | **已定：Windows + Linux 优先，macOS 二期** | 媒体很可能就在一台 Windows 电脑上，那服务端也跑 Windows，而它的坑最多（§11） |
| 5 | 31/32 屏脚注文案 | **已定，契约 v1.5 已落地，设计稿已改** | 改为「转码需要约 {N} 秒启动，不影响正常使用」。去掉"一个 CPU 核心"的理由：NVENC 机器上 `cpuCoresUsed` 是 `0.3`，这个词组既别扭又对用户无意义（§9 R1） |
| 6 | 工期 | **已确认 12.5 周** | 比原估 11 周多 1.5 周：通用化（路径归一化 +0.5、Windows 打包 +0.5）+ 组 E 设置 S2b（+0.5）。两笔都是"前提变了"的必然成本，不是乐观度下调（§11） |
| 7 | 组 E 设置（07 屏） | **新增，已确认 +0.5 周** | 契约 v1.7 加了 `GET/PUT /settings/metadata` + `DELETE /tmdb/cache`。07 屏从一行静态文字变成四个分类、四张卡（媒体库路径 / 扫描与刮削 / 播放与字幕 / 账户与设备），左栏是段落索引（§11 S2b） |

### 13.1 已经没有"等你给型号"这回事了

上一版这里写的是"需要 NAS 型号"。**在主机不预设之后，这个问题自动消失了**——不是靠问出来，是靠程序自己测：

```
首次启动 → host_probe（≤10 秒）
   ├─ ffmpeg -encoders 列表 + 真编 1 秒验证 → 哪些硬编真能用
   ├─ 全链 1080p 吞吐 → chain_fps_1080
   ├─ 顺序读 64MB → 卷吞吐（每个卷一次）
   └─ 落 host_profile，推导 tier 与转码槽位
```

**这不是把问题推给运行时，是把一次性的人工判断变成可复现的自动判断。** 好处很直接：用户换机器、加显卡、把库从 NAS 搬到台式机，都不用改配置或重装——重启一次就重新分档。

**剩下的只有一件需要人工确认的事**：`host_probe` 测出来的倍率对不对。验收方法在 §9 R1 末尾，一条命令，不比原计划多花时间。

### 13.2 这一轮改动里唯一"倒退"的地方

`estStartupMs ≈ 8000` 这个数原本在契约里是写死的常量，而它只对 NAS 级软编成立。v1.5 已经把它定义成计算值、并把文案里的具体秒数与核心数一起拿掉，但**实现上仍然是从"一个常数"变成"一个函数"**。

这算一处轻微的复杂度上升——从"一个常数"变成"一个函数"。但它换掉的是"在所有强机器上承诺一个过于保守的数字，或者在弱机器上承诺一个兑现不了的数字"。**这笔交易划算，因为错误的承诺比复杂的实现更贵。**

---

*文档版本 v3.8 · 2026-11-11 · 对应契约 v1.9 · 工期 12.5 周（已确认）*

**v3.8 变更**（写出 host_probe 并与 R1 数据对账，一处结论收回、一处测量方法作废）：
1. **收回 v3.7 第 3 条**。"分别测解码 `D` 与编码 `E`、按 `1/(1/D+1/E)` 合成"只在**软件**链路成立。
   硬件解码与硬件编码跑在不同引擎、是重叠的——实测全链 480 fps 比"仅解码 171 fps"还高，套那个式子会算出负数。
   改为**直接测全链**，解码吞吐单独测但只用于诊断
2. **R1 的吞吐数全部修正，硬编高了约一倍**。第一版用"总帧数 ÷ 总墙钟"，
   把打开 33 GB 文件 + seek 的 0.85 秒摊进了帧率：同一命令第一版量 14.15 秒、今天量 8.16 秒。
   修正后 1080p 硬编 **480.5 fps**（原记 254.4）、软编 **76.2 fps**（原记 67.0）、DV→SDR **83.4 fps**（原记 71.1）
3. **§1.1 探测方法重写**，写下三个反直觉点：必须扣基线开销、不能信 ffmpeg 自报的 `fps=`、一个取样点不够（同机不同片段差 13%）
4. **分档判据改成绝对 fps**（48 / 24 / 7.2），并把原表 0.6–1.0× 的断档并入 L
5. **`host_profile` 删掉两个倍率列**，改存 `chain_fps_1080` / `chain_fps_2160` / `decode_fps` / `tonemap_fps`，
   并补齐"这个数是哪来的"：样本路径、样本 HDR、置信度、决策依据 JSON
6. 探测耗时从"10 秒内"改为**全量约 28 秒**（`--probe-fast` 约 8 秒）。10 秒是在只有一项测量时定的，加入 4K 与 DV 后不成立

**v3.7 变更**（R1 用真实 4K DV 片源实测，三处原假设被推翻）：
1. §9 R1 新增实测表（i7-9700K + RTX 2080 Ti，3840×1608 60fps HEVC Main10 DV P5）。**软编 1080p 只有 1.12× 实时**，与 tier 表里"S 强 ≥ 2×"的判据差一倍
2. **倍率必须由绝对吞吐除以片源帧率现算**。同一台机器在 24fps 片源上是 6–12×，在 60fps 上只剩 4.24×；只存倍率会把 HFR 片源误判成"轻松可转"
3. **探测方法要改**：原验收命令测的是「1080p→1080p 编码」，真实链路是「4K 解码→下采样→编码」。反推下来探测会报 5.75× 而真实可用 1.12×，**高估 5 倍**。改为分别测 `D`（解码）与 `E`（编码），按 `1/(1/D + 1/E)` 合成
4. **DV Profile 5 需要单独的播放分支**：无 HDR10 基础层，非 DV 客户端拿到不报错而是整片偏绿；tonemap 成本与分辨率无关（720p 与 1080p 都是 72 fps），降档救不回来 → 每机限 1 路。契约 v1.9 已加 `dvProfile` 与规则 0b
5. **§4.2 加三级匹配优先级**：容器 tag（`TMDB`/`IMDB`）与 `.nfo` 优先于文件名解析，请求数从 3 次/片降到 1 次/片
6. 并发不构成许可约束：5 路 NVENC 同时开无报错，`transcode_concurrency` 由吞吐推导


**v3.6 变更**（写扫描器时实测发现的三处缺项）：
1. **`media` 补 `mtime` 列**。§4.1 说增量扫描「只认 mtime + size」，但 §3.1 的 media 表只有 `size_bytes`。缺了它，已入库文件的内容变化检测不到——只能靠 size，而重压一遍的片子 size 完全可能一致，于是永远不更新
2. **澄清 `rel_path` 与 `path_key` 用的不是同一个路径**。`rel_path` 是库相对（给客户端显示），`path_key` 必须用卷相对——同一块盘上 `media/movie` 与 `media/tv` 两个库都可以有 `a.mkv`，用库相对算键会撞
3. **补 `missing_since` 软删标记**（`media` 与 `unmatched` 各一列）。不直接 DELETE 的理由：`playback_progress` 是 `ON DELETE CASCADE`，删 media 会连带删掉续播点，拔一次盘就是整卷观影记录清空

**v3.5 变更**（S1 落地时发现的自相矛盾，一处更正）：
1. **`media` 的唯一键从 `file_path` 改为 `path_key`**。§3.1 原写 `UNIQUE(file_path)`，与 §1.2 ①「不存绝对路径」、§4.1「与 `media.path_key` 比对」、契约第 241 行「绝对路径不是稳定标识」三处冲突。取 `path_key`：盘符变化时才能命中同一行，否则同一部片子会入库两遍、进度分裂成两份，且不报错。`file_path` 降级为可空的展示/排障列
2. 补齐 `media` 表此前缺失的 `volume_id` / `rel_path` / `path_key` 三列——原文表定义里没有这三列，照着建库必然建错

**v3.2 变更**（S0 脚本落地，两条结论反过来改了设计）：
1. **R4 判据从 p99 改为 max**。实测全库单事务时 p99 = 0.1ms 而 max = 72.4ms——队头阻塞是长尾单点，p99 会把它稀释掉。落地为"单事务 ≤ 500 行 + ffprobe 在锁外"
2. **`playable` 定义为"能不能出画面"**，不是"所有流都健康"。音轨坏但画面正常的文件走 `switch_audio`，不是整片失败
3. R7 在 Windows/NTFS 上跑出四类静默差异（大小写折叠 6、尾随裁切 2、ADS 折叠 1、平台不允许 6），并确认 `foldCase` 必须用 Unicode 折叠而非 `strings.ToLower`（`ﬁ` 上两者结果不同）
4. 新增 `spikes/` 六个验证脚本 + 两份检查单，入口 `spikes/README.md`
5. **R6 用真实 token 跑完**：中文命中 hit@1 92% / hit@5 96%、中文名与简介 100%；但脏文件名（含标准英文发布名）**全部返回 0**，所以文件名解析是硬前置；匹配器不能用字符串相等（译名比俗称长是常态）；回退目标是原名而非英文名；5 万文件首扫约 27 小时 → 28 屏必须"先入库可播、后台补元数据"
6. **更正一处**：此前说"28 屏 38 个候选要改成 20"是记错的——`candidateCount: 38` 是待批量匹配的**条目数**，设计稿右栏本来只放 3 个候选。只需在契约里写明候选单页硬上限 20、不翻页
