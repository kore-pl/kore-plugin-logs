# kore-plugin-logs (plugin de visualização e análise de logs)

## O que faz

Lê arquivos de log reais do host e o journal do systemd, com lista branca de fontes, leitura de cauda, busca em blocos no arquivo inteiro, estatísticas, download em blocos, acompanhamento por cursor, agregação de várias fontes e rotação. Grau de produção. Sem amostras em memória e sem conteúdo de log embutido.

Capacidade no nível do plugin system:read; métodos somente leitura usam system:read, métodos de escrita como rotate e retention.set usam system:admin.

## Pontos-chave

- Sem leitura integral: tail varre para trás em blocos de 64 KiB, search para frente; limites rígidos de 64 MiB por varredura e 8 KiB por linha, então logs de GB nunca são carregados por inteiro.
- Lista branca de fontes: apenas nomes registrados em KORE_LOGS_SOURCES são aceitos; requisições nunca carregam caminhos e respostas nunca expõem caminhos absolutos do host.
- Protocolo kore.plugin.v1 pelo UDS injetado pelo núcleo; handshake kore.hello e loop de sessão; stdout reservado para quadros do protocolo, logs vão para stderr.
- Métodos: logs.sources, logs.tail, logs.search, logs.stats, logs.download, logs.follow, logs.aggregate, logs.rotate, logs.retention.get/.set, logs.usage (tabelas completas em DETAIL.md).

## Instalação e verificação

Instale `logs` no mercado de plugins ou importe dist/logs-0.5.4.tar.gz gerado por make dist. Verifique com make check (gofmt, go vet, go test -timeout 200s), incluindo as suítes boundary, fault e e2e.

---

简体中文原文见 [DETAIL.md](DETAIL.md)。
