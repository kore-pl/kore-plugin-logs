# kore-plugin-logs

kore 日志查看分析插件（M5 批1 · 生产级）：真实读取日志文件与 systemd journal，
支持来源白名单、尾部读取、全文件分块搜索与统计。

## 定位

- **真实读取**：按配置登记的来源读取宿主日志；不做内存样例、不内置任何日志内容。
- **不整读**：`tail` 从文件尾按 64 KiB 分块反向扫描，`search` 正向分块扫描；
  单次扫描硬上限 64 MiB，单行上限 8 KiB，GB 级日志也不会被整体读入内存。
- **来源白名单**：只接受 `KORE_LOGS_SOURCES` 中登记的 `name`，请求永不携带路径。
- **协议**：`kore.plugin.v1`，连接内核注入的 UDS，`kore.hello` 握手后进入会话循环；
  stdout 留给协议帧，日志写 stderr。
- **能力**：`system:read`（只读）；`lifecycle=lazy`；`depth_level=production`。

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
| `logs.tail` | `source`、`lines`（默认 100，1–2000）、`keyword?`、`since?`（RFC3339） | `entries[{text,truncated}]`（旧→新）、`count`、`scanned_bytes`、`truncated`、`line_truncated`、`file` |
| `logs.search` | `source`、`keyword`（必填，≤256 字节）、`limit`（默认 100，1–1000） | `matches[{line_no,text,truncated}]`、`scanned_bytes`、`truncated` |
| `logs.stats` | `source` | `path`（`来源名/文件名`）、`size_bytes`、`lines`（>64 MiB 为 `null`）、`modified_at`、`readable`、`note` |
| `kore.ping` / `kore.shutdown` / `kore.reload` | — | 协议标准方法 |

### 语义说明

- `tail` 从文件尾反向定位行边界，**凑够 `lines` 条命中即停**；`keyword` 为 ASCII 大小写不敏感子串，
  `since` 取行首时间戳（RFC3339 或 `YYYY-MM-DD HH:MM:SS`）粗略过滤，解析失败的行**跳过过滤保留**。
- `search` 从文件头逐块扫描，`line_no` 为 1 起真实行号；达到 `limit` 或 64 MiB 上限时 `truncated=true`。
- 单行超过 8 KiB 时保留前缀、对应条目 `truncated=true`，累计体现在 `line_truncated`。
- `journal` 来源通过 `journalctl --output=short-iso` 读取（`--grep` + `--case-sensitive=no`），
  无 journalctl / 无权限时 `sources` 标记 `readable=false`，`tail`/`search` 返回 `E_UPSTREAM`。
- 文件不存在：`tail`/`search` 返回 `E_NOTFOUND`；`stats` 返回成功 + `readable=false` + `note`。
- 未登记的 `source`、非法 `lines`/`limit`/`since`/`keyword`、非对象参数返回 `E_INVALID`。

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
