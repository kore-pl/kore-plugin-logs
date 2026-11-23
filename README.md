# kore-plugin-logs

kore 日志查看分析插件（M5 批1 · 生产级）：真实读取日志文件与 systemd journal，
支持来源白名单、尾部读取、全文件分块搜索、统计、分块下载、带游标的增量跟踪与切割。

## 定位

- **真实读取**：按配置登记的来源读取宿主日志；不做内存样例、不内置任何日志内容。
- **不整读**：`tail` 从文件尾按 64 KiB 分块反向扫描，`search` 正向分块扫描；
  单次扫描硬上限 64 MiB，单行上限 8 KiB，GB 级日志也不会被整体读入内存。
- **来源白名单**：只接受 `KORE_LOGS_SOURCES` 中登记的 `name`，请求永不携带路径。
- **协议**：`kore.plugin.v1`，连接内核注入的 UDS，`kore.hello` 握手后进入会话循环；
  stdout 留给协议帧，日志写 stderr。
- **能力**：插件级 `system:read`；`method_capabilities` 按方法细化——只读方法
  （`sources` / `tail` / `search` / `stats` / `follow` / `download` / `config.get`）为
  `system:read`，写方法（`rotate` / `config.set`）为 `system:admin`；`lifecycle=lazy`；
  `depth_level=production`。

## 配置来源

`KORE_LOGS_SOURCES` 是 JSON 数组，**省略即无来源**（`sources` 返回空列表）：

```json
[
  {"name": "syslog",  "path": "/var/log/syslog",      "type": "file"},
  {"name": "app",     "path": "/var/log/myapp/app.log", "type": "file"},
  {"name": "journal", "path": "",                     "type": "journal"}
]
```

约束（启动时校验，非法即退出，不静默降级）：

- `name`：1–64 位字母数字或 `. _ -`，首字符为字母或数字，全局唯一；
- `type`：`file` 或 `journal`；
- `path`：必须是 `filepath.Clean` 后的**绝对路径**，含 NUL/反斜杠即拒绝；
  `file` 类型必填，`journal` 类型可留空；
- 路径解析：装载时固定父目录的真实路径，每次读取重新解析并比对，最终文件不得是符号链接，
  打开后再用 `os.SameFile` 复核 inode——防目录/文件被替换攻击。

## 方法

| 方法 | 入参 | 结果要点 |
| --- | --- | --- |
| `logs.sources` | 无 | `sources[{name,file,type,exists,readable,size_bytes,modified_at,note}]`、`count`；**不含宿主绝对路径** |
| `logs.tail` | `source`、`lines`（默认 100，1–500）、`keyword?`、`since?`（RFC3339）、`position?` | `entries[{text,time,level,truncated}]`（旧→新）、`count`、`position`、`scanned_bytes`、`truncated`、`line_truncated`、`file` |
| `logs.search` | `source`、`query`（必填，≤256 字节；`keyword` 兼容）、`limit`（默认 100，1–1000） | `matches[{line_no,text,time,level,truncated}]`、`total`、`scanned_bytes`、`truncated` |
| `logs.stats` | `source` | `path`（`来源名/文件名`）、`size_bytes`、`lines`（>64 MiB 为 `null`）、`modified_at`、`readable`、`note` |
| `logs.download` | `source?` / `lines?`（默认 1000，1–10000）/ `from_line?` + `to_line?`（行区间）；或 `id` + `offset?` + `limit?`（默认 1 MiB，≤4 MiB） | `path` / `filename` / `id` / `size_bytes` / `lines` / `data_base64`、`offset` / `next_offset` / `eof` / `chunk_bytes` / `max_chunk_bytes`、`truncated` |
| `logs.follow` | `source`、`cursor?`、`max_lines?`（默认 200，1–500）、`max_bytes?`（默认 1 MiB，≤4 MiB）、`keyword?` | `cursor`、`entries[]`、`count`、`eof`、`reset`、`position`、`scanned_bytes`、`truncated`、`line_truncated` |
| `logs.aggregate` | `sources?`（为空=全部，顺序同配置）、`keyword?`、`since?`（RFC3339）、`limit?`（全局上限，默认 100，1–500） | `sources[]`、`groups[{source,type,file,count,truncated,error?,entries[{source,text,time,level,truncated}]}]`、`count`、`truncated`、`errors` |
| `logs.rotate` | `source`、`confirm`（**必须 true**）、`keep?`（默认 5，1–100） | `rotated_to`、`size_bytes`、`kept`、`removed[]`、`note`（rename 语义：写旧 inode 的进程需重载） |
| `kore.ping` / `kore.shutdown` / `kore.reload` | — | 协议标准方法 |
| `logs.retention.get` | 无 | `defaults`（30 天 / 1024 MB / 7 份）、`policies{source:{max_age_days,max_size_mb,max_files}}`、`note` |
| `logs.retention.set` | `source`、`max_age_days?`（0–3650）、`max_size_mb?`（0–1048576）、`max_files?`（0–10000） | `policy`、`saved`；0=不限制；未传维度保持不变；落 `<state>/retention.json`（0600） |
| `logs.usage` | 无 | 各来源 `active_bytes` / `rotated_bytes` / `total_bytes` / `files` / `oldest_at`、`policy`、`over_policy` / `reasons`；`over_count` |
| `logs.clean` | `source?`、`dry_run?`（缺省 true）、`confirm?` | `candidates[{source,file,size_bytes,modified_at,reason}]`、`total_bytes`、`moved`、`quarantine_dir` / `quarantine_bytes` / `evicted`、`note`；真实清理须 `dry_run=false`+`confirm=true` |
| `logs.clean.restore` | `source?` | `restored[]`、`skipped[]`（同名目标已存在则跳过，不覆盖）、`note` |

### 语义说明

- `tail` 从文件尾反向定位行边界，**凑够 `lines` 条命中即停**；`keyword` 为 ASCII 大小写不敏感子串，
  `since` 取行首时间戳（RFC3339 或 `YYYY-MM-DD HH:MM:SS`）粗略过滤，解析失败的行**跳过过滤保留**。
- `search` 从文件头逐块扫描，`line_no` 为 1 起真实行号；达到 `limit` 或 64 MiB 上限时 `truncated=true`。
- 单行超过 8 KiB 时保留前缀、对应条目 `truncated=true`，累计体现在 `line_truncated`。
- `journal` 来源通过 `journalctl --output=short-iso` 读取（`--grep` + `--case-sensitive=no`），
  无 journalctl / 无权限时 `sources` 标记 `readable=false`，`tail`/`search` 返回 `E_UPSTREAM`。
- 文件不存在：`tail`/`search` 返回 `E_NOTFOUND`；`stats` 返回成功 + `readable=false` + `note`。
- 未登记的 `source`、非法 `lines`/`limit`/`since`/`keyword`、非对象参数返回 `E_INVALID`。
- `follow` 不启动 ticker/后台 goroutine：每次调用只做一次前向读取，返回 `cursor` + 增量 + `eof`，
  是否继续拉取由调用方按需决定（空闲 CPU/定时器为 0，满足 G-19）。只回完整行、不重不漏；
  文件轮转/截断或游标不匹配时 `reset=true` 并回退尾部窗口；journal 来源返回 `E_UNSUPPORTED`。
- `download` 分块模式只回 `[offset,next_offset)` 一段（`eof` 表到文件尾），大文件不整份塞单帧；
  `id` 只接受导出目录内单段文件名，含 `..`/路径分隔符/符号链接一律拒绝；导出总量 5 MiB 封顶。
- `rotate` 是**破坏性**操作：`confirm=true` 必填，只接受登记来源（永不接受路径），只切割
  登记目录内的常规文件（符号链接/目录拒绝），并按 `keep` 只删除同目录下匹配
  `<base>.<UTC时间戳>(-n)` 的旧切割文件；rename 后原路径消失，写旧 inode 的进程需重载。

## 故障注入

运行期设置 `KORE_FAULT`（`kill` / `timeout` / `disk-full` / `cgroup-oom`），方法入口统一返回结构化错误：

| 故障 | 错误码 |
| --- | --- |
| `kill`、`timeout` | `E_UPSTREAM` |
| `disk-full`、`cgroup-oom` | `E_INTERNAL` |

`tests/fault/` 四文件逐一覆盖，并带真实系统语义断言（SIGKILL / context deadline / `/dev/full` ENOSPC /
cgroup `memory.max` 只读探测）。`tests/e2e/` 用真实二进制 + UDS 跑完整生命周期。

## 构建与验收

```sh
make build   # 静态二进制 bin/logs（CGO_ENABLED=0 + GOAMD64=v1 + -s -w）
make check   # gofmt + go vet + go test -timeout 200s
make dist    # 确定性 tar.gz：固定 mtime/owner/顺序 + gzip -9n + SHA256SUMS
```

打包首条目为 `info.json`，`size_bytes` 经不动点校准（偏差≤10%）；两次 `make dist` 的 sha256 一致。

## 许可

MIT © pengtang1024

<!-- oss:begin -->
## 开源与贡献

[![CI](https://github.com/kore-pl/kore-plugin-logs/actions/workflows/ci.yml/badge.svg)](https://github.com/kore-pl/kore-plugin-logs/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)

本仓库是 **kore** 的 `日志查看分析插件`（id `logs`），独立仓库、独立版本线、独立 CI（少触发：push main / tag / PR / 手动）。内核与协议见 [kore](https://github.com/kore-pl/kore)。

- **许可**：MIT（inbound = outbound，不要求 CLA）；提交用 `git commit -s` 签 DCO。
- **贡献**：[CONTRIBUTING.md](CONTRIBUTING.md)（构建、门禁、发版、边界）。
- **安全**：[SECURITY.md](SECURITY.md)——漏洞不要开公开 issue。
- **行为准则**：[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。
- **兼容性**：`protocol: kore.plugin.v1` · `requires_kore: >=0.1.0 <0.2.0` · `lifecycle: lazy` · 深度等级 `production`。
- **验证方式**：本地 `make check` 优先（CI 只在 push main / tag / PR / 手动跑；发布前 `make dist` 连跑两次核对 sha 一致）。
- **发布产物**：`make dist` → `dist/logs-0.4.1.tar.gz` + `.sha256`（位级可复现，连跑两次 sha 相同）。
- **相关仓库**：[索引 kore-plugins](https://github.com/kore-pl/kore-plugins) · [协议文档](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-PROTOCOL.md) · [SDK 指南](https://github.com/kore-pl/kore/blob/main/docs/PLUGIN-SDK.md)

<!-- oss:end -->
