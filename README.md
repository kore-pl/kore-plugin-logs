# kore-plugin-logs

kore 日志查看分析插件（M5 批1 · 生产级）：按来源分流 + 关键字过滤。

## 定位

- **骨架口径**：方法只操作内存固定样例条目，不读取宿主日志文件、不内置任何二进制。真实日志源接入属后续版本。
- **协议**：`kore.plugin.v1`，连接内核注入的 UDS，`kore.hello` 握手后进入会话循环；stdout 留给协议帧，日志写 stderr。
- **能力**：`system:read`（只读）；`lifecycle=lazy`；`depth_level=production`。

## 方法

| 方法 | 说明 | 入参 | 结果 |
| --- | --- | --- | --- |
| `logs.list` | 按来源分流统计 | 无 | `sources`（system/kernel/app/access/error 各条数）、`count`、`note` |
| `logs.query` | 分流 + 关键字过滤 + 限量 | `source`（枚举或 `all`）、`keyword`（≤256 字符）、`limit`（0=默认100，上限1000） | `source`、`keyword`、`limit`、`count`、`truncated`、`entries` |
| `kore.ping` / `kore.shutdown` / `kore.reload` | 协议标准方法 | — | 见 PLUGIN-PROTOCOL §7 |

`limit > 1000` 返回 `E_INVALID`（防 DoS）；非法 `source`、负数 `limit`、非对象参数同样返回 `E_INVALID`。

## 故障注入

运行期设置 `KORE_FAULT` 环境变量（`kill` / `timeout` / `disk-full` / `cgroup-oom`），方法级统一返回结构化错误：

| 故障 | 错误码 |
| --- | --- |
| `kill`、`timeout` | `E_UPSTREAM` |
| `disk-full`、`cgroup-oom` | `E_INTERNAL` |

`tests/fault/` 四文件逐一覆盖，并带真实系统语义断言（SIGKILL / context deadline / `/dev/full` ENOSPC / cgroup `memory.max` 只读探测）。

## 构建与验收

```sh
make build   # 静态二进制 bin/logs（CGO_ENABLED=0 + GOAMD64=v1 + -s -w）
make check   # gofmt + go vet + go test -timeout 200s
make dist    # 确定性 tar.gz：固定 mtime/owner/顺序 + gzip -9n + SHA256SUMS
```

打包首条目为 `info.json`，`size_bytes` 经不动点校准（偏差≤10%）；两次 `make dist` 的 sha256 一致。

## 许可

MIT © pengtang1024
