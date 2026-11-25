# kore-plugin-logs (plugin de visualización y análisis de registros)

## Qué hace

Lee archivos de registro reales del host y el journal de systemd, con lista blanca de fuentes, lectura de cola, búsqueda por bloques en todo el archivo, estadísticas, descarga por bloques, seguimiento con cursor, agregación multifuente y rotación. Grado de producción. Sin muestras en memoria ni contenido de registros integrado.

Capacidad a nivel de plugin system:read; métodos de solo lectura usan system:read, métodos de escritura como rotate y retention.set usan system:admin.

## Puntos clave

- Sin lectura completa: tail explora hacia atrás en bloques de 64 KiB, search hacia delante; límites estrictos de 64 MiB por exploración y 8 KiB por línea, así que registros de GB nunca se cargan enteros.
- Lista blanca de fuentes: solo se aceptan nombres registrados en KORE_LOGS_SOURCES; las peticiones nunca llevan rutas y las respuestas no exponen rutas absolutas del host.
- Protocolo kore.plugin.v1 sobre el UDS inyectado por el núcleo; handshake kore.hello y bucle de sesión; stdout reservado a tramas del protocolo, registros a stderr.
- Métodos: logs.sources, logs.tail, logs.search, logs.stats, logs.download, logs.follow, logs.aggregate, logs.rotate, logs.retention.get/.set, logs.usage (tablas completas en DETAIL.md).

## Instalación y verificación

Instale `logs` desde el mercado de plugins o importe dist/logs-0.5.4.tar.gz generado con make dist. Verifique con make check (gofmt, go vet, go test -timeout 200s), incluidas las suites boundary, fault y e2e.

---

简体中文原文见 [DETAIL.md](DETAIL.md)。
