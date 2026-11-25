# kore-plugin-logs（日志查看分析插件）

## 它做什么

真实读取宿主日志文件与 systemd journal，提供来源白名单、尾部读取、全文件分块搜索、统计、分块下载、带游标的增量跟踪、多来源聚合与日志切割。生产级（depth_level=production）。**不做内存样例、不内置任何日志内容**。

## 设计约束

- **不整读**：`tail` 从文件尾按 64 KiB 分块反向扫描，`search` 正向分块扫描；单次扫描硬上限 64 MiB，单行上限 8 KiB，GB 级日志也不会整体读入内存。
- **来源白名单**：只接受 `KORE_LOGS_SOURCES` 中登记的 `name`，请求永不携带路径；返回中不暴露宿主绝对路径。
- **协议**：`kore.plugin.v1`，连接内核注入的 UDS，`kore.hello` 握手后进入会话循环；stdout 留给协议帧，日志写 stderr。
- **能力**：插件级 `system:read`；只读方法细化 `system:read`，`rotate` / `retention.set` 等写类方法为 `system:admin`。

## 方法

| 方法 | 入参 | 行为 |
| --- | --- | --- |
| `logs.sources` | 无 | 登记来源列表（name/file/type/exists/readable/size_bytes），不含绝对路径。 |
| `logs.tail` | `source`、`lines`（1–500，默认 100）、`keyword?`、`since?`（RFC3339）、`position?` | 反向分块读取尾部，返回 `entries[{text,time,level,truncated}]` 与游标。 |
| `logs.search` | `source`、`query`（必填，≤256 字节）、`limit`（1–1000，默认 100） | 正向分块搜索，返回 `matches[]`、总数与截断标记。 |
| `logs.stats` | `source` | 文件大小、行数（>64 MiB 为 null）、修改时间。 |
| `logs.download` | 按行区间或 `id` + `offset` 分块 | 分块 base64 返回，单块默认 1 MiB、上限 4 MiB。 |
| `logs.follow` | `source`、`cursor?`、`max_lines?`、`max_bytes?`、`keyword?` | 带游标的增量读取，返回新条目与下一游标。 |
| `logs.aggregate` | `sources?`、`keyword?`、`since?`、`limit?` | 多来源聚合分组。 |
| `logs.rotate` | `source`、`confirm`（必须 true）、`keep?` | 切割（rename 语义，写旧 inode 的进程需重载）。 |
| `logs.retention.get` / `.set` | `source`、各维度上限 | 保留策略（默认 30 天 / 1024 MB / 30 份 / 100 MiB 切割阈值），存 `<state>/retention.json`（0600）。 |
| `logs.usage` | 无 | 各来源占用、文件数、最早时间与超策略原因。 |

## 安装与验证

从插件市场安装 `logs`，或通过 `make dist` 产出 `dist/logs-0.5.4.tar.gz` 导入。开发验证：`make check`（gofmt → go vet → go test -timeout 200s），含 boundary / fault / e2e 测试。

## 安全

漏洞不要开公开 issue，按 [SECURITY.md](https://github.com/kore-pl/kore-plugin-logs/blob/main/SECURITY.md) 私下披露。
