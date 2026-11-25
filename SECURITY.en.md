# Security Vulnerability Disclosure

This file is the vulnerability disclosure policy for **this plugin repository (logs)**, not a security design document.

## Scope

Covers the implementation of 日志查看分析插件 (id `logs`), its `info.json` capability declarations and build artifacts. Out of scope: core and protocol issues — report those via the [kore SECURITY.md](https://github.com/kore-pl/kore/blob/main/SECURITY.md); known vulnerabilities in third-party dependencies should also be reported upstream.

## Supported versions

| Version | Status | Notes |
| --- | --- | --- |
| `0.5.5` (latest) | Security fixes accepted | Only the latest patch release receives fixes |
| Earlier `0.x` versions | Not supported | Upgrade to the latest version first |

In the `0.x` phase, only the latest patch of a given minor line is maintained; older patches are not backported individually.

## Attack surface in this repository

- **RPC methods**: all `logs.*` methods (see README); missing parameter validation may lead to privilege escalation or injection.
- **capabilities**: `info.json.capabilities` is the complete authorization surface; every extra declaration is another attack path.
- **Separate process and UDS**: the plugin runs as a separate process; its socket is created by the core, `chown kore-<id>`, mode `0600`; bypassing socket permissions or connecting to another plugin's socket is a vulnerability.
- **Host write surface**: writes beyond the capabilities actually granted (files, processes, network, databases) are vulnerabilities.

## Reporting

1. **Prefer GitHub Security Advisory**: this repository's Security page → "Report a vulnerability" (private channel, no public issue).
2. **Fallback email**: `pengtang1024@users.noreply.github.com`, subject `[kore security] logs`.
3. **Do not open a public issue or post a PoC** in discussions or PRs.
4. Include: affected version, reproduction steps or PoC, impact assessment, disclosure status and attribution preference.

## Timeline and disclosure

- **Acknowledge within 72 hours** with an initial assessment; complex cases get an investigation plan.
- Coordinate disclosure after a fix, default **90-day** window; shorten only after discussion if actively exploited.
- Record the security fix in `CHANGELOG.md` after release (no exploit details), and publish an advisory as needed.

## Acknowledgements

With consent, credit the reporter in the release notes; anonymous upon request.
