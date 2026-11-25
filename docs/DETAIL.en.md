# kore-plugin-logs (log viewer and analysis plugin)

## What it does

Reads real host log files and systemd journal, with source allowlisting, tail reading, chunked full-file search, statistics, chunked download, cursor-based follow, multi-source aggregation and log rotation. Production grade. No in-memory samples and no bundled log content.

Plugin-level capability system:read; read-only methods use system:read, write methods such as rotate and retention.set use system:admin.

## Key points

- No whole-file reads: tail scans backwards in 64 KiB chunks, search scans forwards; hard limits of 64 MiB per scan and 8 KiB per line, so GB-sized logs are never loaded entirely.
- Source allowlist: only names registered in KORE_LOGS_SOURCES are accepted; requests never carry paths and responses never expose absolute host paths.
- Protocol kore.plugin.v1 over the UDS injected by the core; kore.hello handshake then the session loop; stdout reserved for protocol frames, logs go to stderr.
- Methods: logs.sources, logs.tail, logs.search, logs.stats, logs.download, logs.follow, logs.aggregate, logs.rotate, logs.retention.get/.set, logs.usage (full parameter tables in DETAIL.md).

## Installation and verification

Install `logs` from the plugin marketplace or import dist/logs-0.5.4.tar.gz built with make dist. Verify with make check (gofmt, go vet, go test -timeout 200s), including boundary, fault and e2e suites.

---

简体中文原文见 [DETAIL.md](DETAIL.md)。
