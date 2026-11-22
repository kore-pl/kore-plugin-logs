# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-11-22

### Added

- 日志插件骨架：`kore.plugin.v1` 握手 + 会话循环 + UDS 连接（复用 site 骨架口径）。
- `logs.list`：按 system/kernel/app/access/error 来源分流统计（骨架样例，不读宿主日志）。
- `logs.query`：`source` / `keyword` / `limit` 显式校验，limit 上限 1000（防 DoS），结构化结果含 `truncated`。
- 四类故障注入（`kill` / `timeout` / `disk-full` / `cgroup-oom`）与 `tests/fault/` 真实断言测试（G-20）。
- 确定性打包：`Makefile` `dist` 目标，显式列条目、固定 mtime/owner/顺序、`gzip -9n`、`SHA256SUMS`，`size_bytes` 不动点校准。
- 文档与 MIT 许可。
