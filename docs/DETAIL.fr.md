# kore-plugin-logs (plugin de consultation et d'analyse des journaux)

## Ce que fait le plugin

Lit les vrais fichiers de journal de l'hôte et le journal systemd, avec liste blanche de sources, lecture de fin, recherche par blocs sur tout le fichier, statistiques, téléchargement par blocs, suivi par curseur, agrégation multi-sources et rotation. Qualité production. Aucun échantillon en mémoire et aucun contenu de journal intégré.

Capacité au niveau plugin system:read ; les méthodes en lecture seule utilisent system:read, les méthodes d'écriture comme rotate et retention.set utilisent system:admin.

## Points clés

- Aucune lecture intégrale : tail analyse en arrière par blocs de 64 KiB, search en avant ; limites strictes de 64 MiB par analyse et 8 KiB par ligne, donc les journaux de plusieurs Go ne sont jamais chargés entièrement.
- Liste blanche : seuls les noms enregistrés dans KORE_LOGS_SOURCES sont acceptés ; les requêtes ne contiennent jamais de chemin et les réponses n'exposent pas les chemins absolus de l'hôte.
- Protocole kore.plugin.v1 sur l'UDS injecté par le noyau ; poignée de main kore.hello puis boucle de session ; stdout réservé aux trames du protocole, journaux sur stderr.
- Méthodes : logs.sources, logs.tail, logs.search, logs.stats, logs.download, logs.follow, logs.aggregate, logs.rotate, logs.retention.get/.set, logs.usage (tableaux complets dans DETAIL.md).

## Installation et vérification

Installez `logs` depuis le marché de plugins ou importez dist/logs-0.5.4.tar.gz produit par make dist. Vérifiez avec make check (gofmt, go vet, go test -timeout 200s), y compris les suites boundary, fault et e2e.

---

简体中文原文见 [DETAIL.md](DETAIL.md)。
