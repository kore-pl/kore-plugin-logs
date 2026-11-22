# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- 统一插件配置契约（`logs.config.get` / `logs.config.set`）：界面可配日志来源、保存即生效、无需重启；
  字段类型 `list(item=string)`，每项 `名称=绝对路径` 或 `名称=journal`。
- 配置存储 `<KORE_STATE_DIR>/config.json`（0600，原子写），优先级 `config.json` > `KORE_LOGS_SOURCES` > 默认值；
  老用户环境变量继续兼容。
- 通用 `internal/plugincfg`：schema 校验、0600 原子存储、sensitive 字段 `***` 掩码；日志来源列表复用
  `logic.ValidateSources`，写盘前与启动加载同一套校验。

## [0.2.0] - 2026-11-22

### Added

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
