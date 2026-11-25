# kore-plugin-logs (Plugin zum Anzeigen und Analysieren von Logs)

## Was es tut

Liest echte Host-Logdateien und das systemd-Journal, mit Quellen-Whitelist, Tail-Lesen, blockweise Suche im gesamten File, Statistiken, blockweisem Download, cursor-basiertem Follow, Aggregation mehrerer Quellen und Log-Rotation. Produktionsreif. Keine In-Memory-Beispiele und keine integrierten Log-Inhalte.

Plugin-weite Fähigkeit system:read; schreibfreie Methoden nutzen system:read, schreibende Methoden wie rotate und retention.set nutzen system:admin.

## Wichtige Punkte

- Kein Komplettlesen: tail scannt rückwärts in 64-KiB-Blöcken, search vorwärts; harte Grenzen von 64 MiB pro Scan und 8 KiB pro Zeile, daher werden GB-große Logs nie vollständig geladen.
- Quellen-Whitelist: nur in KORE_LOGS_SOURCES registrierte Namen werden akzeptiert; Anfragen enthalten nie Pfade, Antworten zeigen nie absolute Host-Pfade.
- Protokoll kore.plugin.v1 über den vom Kern injizierten UDS; kore.hello-Handshake, danach die Sitzungsschleife; stdout ist für Protokollrahmen reserviert, Logs gehen nach stderr.
- Methoden: logs.sources, logs.tail, logs.search, logs.stats, logs.download, logs.follow, logs.aggregate, logs.rotate, logs.retention.get/.set, logs.usage (vollständige Tabellen in DETAIL.md).

## Installation und Prüfung

Installieren Sie `logs` über den Plugin-Markt oder importieren Sie das mit make dist erzeugte dist/logs-0.5.4.tar.gz. Prüfen Sie mit make check (gofmt, go vet, go test -timeout 200s), einschließlich boundary-, fault- und e2e-Tests.

---

简体中文原文见 [DETAIL.md](DETAIL.md)。
