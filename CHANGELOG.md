# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.2] - 2026-11-23

### Added

- `logs.aggregate`：跨多个已登记来源的统一检索（`sources?` 空表示全部、`keyword?`、`since?`、`limit?` 全局上限），按来源分组返回命中，每条带 `source` 归属；复用 `logs.tail` 的分块读取与 keyword/since 过滤，只读 `system:read`，无 ticker / 无联网
- 单来源读取失败不拖垮整次聚合：该组带 `error` 字段并在 `errors` 计数中体现；全局额度耗尽后其余来源保留空分组并置 `truncated`
- test: 多源聚合/分组归属/limit 截断/显式 sources 去重与过滤/非法参数（未登记来源、limit、since、keyword）
- `method_capabilities` 增 `logs.aggregate: system:read`（10 条）；版本 0.4.1 → 0.4.2

## [0.4.1] - 2026-11-23

### Fixed

- 插件级 `capabilities` 补 `system:admin`（原仅 `system:read`）：方法级已含 `rotate` / `config.set`
  需要 `system:admin`，按 `pluginapitest` 规则「方法级不得强于插件级（只收窄、不放大）」，
  原声明会误导启用确认的能力范围；方法级 `method_capabilities` 不变，调用契约不变。

## [0.4.0] - 2026-11-23

### Added

- `method_capabilities`（§3.5 #12）：按方法声明宿主能力——`sources` / `tail` / `search` /
  `stats` / `follow` / `download` / `config.get` 为 `system:read`，`rotate` 与 `config.set`
  为 `system:admin`；未声明方法仍回退插件级 `system:read`，既有语义不变。
- `logs.follow`：**带游标的按需增量读取**（G-19：无 ticker、无后台 goroutine、无推送）。
  入参 `source` / `cursor?` / `max_lines?`（默认 200，1–500）/ `max_bytes?`（默认 1 MiB，
  上限 4 MiB）/ `keyword?`；返回 `cursor`、`entries`、`count`、`eof`、`reset`、`position`、
  `scanned_bytes`、`truncated`。只回完整行、不重不漏；末尾不完整行留在文件等补全；
  文件轮转/截断或游标与来源不匹配时置 `reset=true` 并回退尾部窗口重建游标；
  journal 来源无稳定偏移，返回 `E_UNSUPPORTED`（请用 `tail`）。
- `logs.rotate`：**切割**（原子 rename）替代清空，破坏性操作三重门禁——`confirm=true` 必填、
  只接受登记来源（不接受路径）、目标必须在登记目录内且非符号链接；按 `keep`（默认 5，
  1–100）只删除同目录下匹配 `<base>.<UTC时间戳>(-n)` 的常规切割文件，其他文件不碰。
  返回 `rotated_to` / `size_bytes` / `kept` / `removed`，并提示正在写旧 inode 的进程需重载。
- `logs.download` 支持**行区间导出与分块读取**：新增 `from_line` / `to_line`（1 起，含两端，
  最多 10000 行、受 64 MiB 扫描预算约束）、`id` / `offset` / `limit`（默认 1 MiB，上限 4 MiB）
  与 `max_chunk_bytes`、`next_offset`、`eof`；导出仍落 `<state>/exports/`（0700/0600 原子写，
  5 MiB 封顶），大文件按块取、不整份塞进单帧；`id` 只允许导出目录内的单段文件名，
  含 `..` / 路径分隔符 / 符号链接一律拒绝。不传 `offset`/`limit`/`id` 时保持 0.3.0
  「整份 base64 inline」默认语义。

### Changed

- `logs.download` 的响应新增 `id` / `offset` / `next_offset` / `eof` / `chunk_bytes` /
  `max_chunk_bytes`（纯新增字段，旧调用方无感）。

## [0.3.0] - 2026-11-23

### Added

- `logs.download {source, lines?}`：导出日志片段到 `<KORE_STATE_DIR>/exports/<source>-<UTC时间戳>.log`
  （0700 目录 / 0600 文件，临时文件 + rename 原子落盘）；默认 1000 行、上限 10000 行，
  片段总量 5 MiB 封顶并置 `truncated`；响应含 `path` / `filename` / `size_bytes` / `lines` /
  `data_base64`——前端按 file.download 同款 base64 通道触发浏览器下载，内核无需新增静态路由。
- `logs.search` 响应升级：`matches` 每条新增 `time`（行首时间戳 RFC3339 规范形，解析失败为空）
  与 `level`（行首 64 字节窗口解析 ERROR/WARN/INFO/DEBUG，词边界校验，`INFORMATION`/`ERRNO`
  不误判）；新增 `total`（全文命中总行数，达到 limit 后继续扫描计数），`total > count` 置
  `truncated`。入参新增 `query`（0.2.x `keyword` 继续兼容，`query` 优先）。
- `logs.tail` 增量拉取：入参新增 `position`（字节偏移），file 来源从该偏移前向只回**完整新行**，
  响应新增 `position`（最后一条完整行之后的偏移）；末尾不完整行留在文件等补全，不重复下发；
  新增内容超过 1 MiB 窗口跳到最近 1 MiB 并置 `truncated`；文件轮转（position 超出文件大小）
  自动回退全量尾读。journal 来源忽略 position（无稳定偏移）。
- `entries` 每条同步携带 `time` / `level`（tail / journal 同口径）。

### Changed

- `logs.tail` 行数上限 2000 → 500（前端跟随视图保留上限同步 500 行）。
- journalctl 时间布局补充 `--output=short-iso` 的 `+0800` 无冒号偏移格式。

## [0.2.1] - 2026-11-23

- build: 位级可复现打包（stage chmod + tar --mode 归一、size_bytes 收敛），二进制改嵌 manifest.json，消除同版本 sha 漂移

## [0.2.0] - 2026-11-22

### Added

- 统一插件配置契约（`logs.config.get` / `logs.config.set`）：界面可配日志来源、保存即生效、无需重启；
  字段类型 `list(item=string)`，每项 `名称=绝对路径` 或 `名称=journal`。
- 配置存储 `<KORE_STATE_DIR>/config.json`（0600，原子写），优先级 `config.json` > `KORE_LOGS_SOURCES` > 默认值；
  老用户环境变量继续兼容。
- 通用 `internal/plugincfg`：schema 校验、0600 原子存储、sensitive 字段 `***` 掩码；日志来源列表复用
  `logic.ValidateSources`，写盘前与启动加载同一套校验。

- 真实日志读取：`logs.sources` / `logs.tail` / `logs.search` / `logs.stats` 四个方法落地，移除骨架样例。
- 来源白名单：仅接受 `KORE_LOGS_SOURCES` 登记的 `name`（`file` / `journal`），启动时校验；请求不携带路径。
- 安全校验：配置路径必须 Clean 绝对路径；装载时固定父目录真实路径，读取时复核；最终文件禁符号链接；打开后 `os.SameFile` 防替换。
- `logs.tail`：从文件尾按 64 KiB 反向扫描行边界，凑够 N 行即停（1–2000，默认 100），
  支持大小写不敏感 `keyword` 与 RFC3339 `since` 行首时间戳过滤；单次扫描上限 64 MiB。
- `logs.search`：正向 64 KiB 分块扫描，返回真实行号、命中内容、`scanned_bytes` 与 `truncated`；
  达到 `limit`（默认 100，上限 1000）或 64 MiB 即停。
- `logs.stats`：大小、修改时间、可读性与行数；>64 MiB 文件跳过行数统计（`lines=null`）。
- 单行上限 8 KiB：超长行截断并标记（`truncated` / `line_truncated`）。
- journal 来源：经 `journalctl --output=short-iso` 读取，`--grep` + `--case-sensitive=no` 预过滤。
- 路径脱敏：响应只给 `name + 文件名`，宿主绝对路径仅写 DEBUG 日志。
- 单元测试覆盖 1 万行 tail、keyword/since、行号、空/缺失文件、超长行、未登记来源、
  符号链接与目录替换攻击、100 MiB 稀疏文件 tail 200（断言耗时与 `scanned_bytes`）。
- `tests/e2e/` 真实二进制 + UDS 端到端（sources/tail/search/stats + 未登记来源拒绝）。

### Changed

- 方法面由骨架期 `logs.list` / `logs.query` 替换为上述四个真实方法；`tests/fault/` 的入口同步到新 API（四类故障断言不变）。

## [0.1.0] - 2026-11-22

### Added

- 日志插件骨架：`kore.plugin.v1` 握手 + 会话循环 + UDS 连接（复用 site 骨架口径）。
- `logs.list`：按 system/kernel/app/access/error 来源分流统计（骨架样例，不读宿主日志）。
- `logs.query`：`source` / `keyword` / `limit` 显式校验，limit 上限 1000（防 DoS），结构化结果含 `truncated`。
- 四类故障注入（`kill` / `timeout` / `disk-full` / `cgroup-oom`）与 `tests/fault/` 真实断言测试（G-20）。
- 确定性打包：`Makefile` `dist` 目标，显式列条目、固定 mtime/owner/顺序、`gzip -9n`、`SHA256SUMS`，`size_bytes` 不动点校准。
- 文档与 MIT 许可。
