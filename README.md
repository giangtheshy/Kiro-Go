# Kiro-Go (Enhanced Fork)

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Turn Kiro accounts into an OpenAI- and Anthropic-compatible API service.

[English](README.md) | [中文](README_CN.md) | [Tiếng Việt](README_VI.md)

> This is an **enhanced fork** of the upstream Kiro-Go. It keeps full compatibility with the original endpoints and admin panel, and adds per-key rate limiting, a shared proxy pool with failover, model overrides, DoS protection, and a self-service usage dashboard. See [What's new vs. the original](#whats-new-vs-the-original).

## What it does

Kiro-Go is a reverse proxy. It manages a pool of Kiro accounts, translates incoming Claude/OpenAI requests into Kiro's upstream AWS format, streams responses back, and serves a web admin panel. It's a single Go module built on the standard library `net/http` — one dependency (`github.com/google/uuid`).

Request flow: **client → routing → auth → translator → pool picks account → stream from AWS → translate events back → client.**

## Features

- Anthropic `/v1/messages` and OpenAI `/v1/chat/completions` (+ `/v1/responses`, `/v1/models`, `/v1/stats`)
- Multi-account pool with weighted round-robin load balancing and automatic failover
- Auto token refresh, SSE streaming, web admin panel
- Multiple auth methods: AWS Builder ID, IAM Identity Center (Enterprise SSO), Kiro Hosted SSO (incl. Microsoft Entra / external IdP), SSO token import, local Kiro credential cache, credentials JSON
- Usage tracking, account import/export, i18n (EN / 中文 / Tiếng Việt)
- Outbound proxy support (SOCKS5 / HTTP)
- Thinking mode (per-suffix or Claude `thinking` config)

## What's new vs. the original

Compared to the upstream Kiro-Go, this fork adds:

| Area | Feature |
|------|---------|
| **Per-key limits** | RPM limit (token-bucket delay, not hard error), concurrent-IP cap, IP allowlist (IP/CIDR), TPM display, per-key token/credit quota |
| **Friendly limit notice** | A configurable in-chat reply shown when a key is blocked (disabled / expired / over-limit / IP-denied) instead of a raw 401/429 |
| **Model overrides** | **Force Model** (global override for every request) and **per-key Model** — remap client-requested model names to a real upstream model without touching the client |
| **Identity model** | Tell the assistant to self-identify as a given model name without changing which upstream model actually serves the request |
| **Bound accounts** | Pin an API key to a fixed set of accounts (empty = shared pool) |
| **Lifetime counters** | Grand-total request/token/credit counters that survive a routine per-cycle "Reset Usage", plus "Reset All" |
| **Bulk key ops** | Bulk create / delete API keys, editable key values, JSON export (preserves External IdP metadata so Entra accounts re-import correctly) |
| **Shared proxy pool** | A pool of outbound proxies with persisted health and proxy-level failover — a dead proxy is skipped and retried after a cooldown, across restarts. Plus a **Require-proxy** safety toggle and per-request routing logs |
| **DoS protection** | App-layer guard (`proxy/dos_guard.go`): body-size cap, global concurrency limit, per-IP RPM reject, per-key inflight cap, trusted-proxy IP resolution — all tunable via env vars |
| **Admin hardening** | Brute-force throttle and session handling for the admin API |
| **Self-service usage** | A `/usage` dashboard where a key owner can view their own usage/logs, plus the `/check` portal |
| **Realtime UI** | The admin panel auto-refreshes lists (accounts, keys) without a manual reload |

## Quick start

### Windows (local, one-click)

```bat
run.bat
```

`run.bat` stops any previous instance, finds a free port, builds, and runs. Requires [Go](https://go.dev/dl/) installed and an existing `data/config.json`.

### Build from source

```bash
go build -o kiro-go .
./kiro-go
```

Config is auto-created at `data/config.json` if missing. Open <http://localhost:8080/admin>.

### Docker Compose (recommended for servers)

```bash
docker compose up -d --build
```

Use `--build` whenever the code changes, otherwise compose reuses a stale cached image. See [DEPLOYMENT.md](DEPLOYMENT.md) for the full Docker guide, including the SSO loopback-port mechanism.

The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

When any API key is enabled, requests must carry a valid key (`Authorization: Bearer sk-...`).

### Endpoints

| Type | Paths |
|------|-------|
| API (key-gated) | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`, `/v1/responses`, `/v1/models`, `/v1/stats` |
| Self-service (caller's own key) | `/v1/key/info`, `/v1/key/logs`, `/usage` |
| Admin | `/admin` (page), `/admin/api/*` (password-gated) |
| Public | `/health`, `/check` |

### Model overrides & identity

- **Force Model** (Settings → Force Model): overrides the model of *every* request. Use it when a client asks for a model name that doesn't exist upstream (e.g. remap `claude-sonnet-4.8` → a real model) to avoid 503s.
- **Per-key Model** (API-key modal): same idea, scoped to one key. Force Model takes precedence.
- **Identity Model**: only changes how the assistant answers "what model are you?" — it does not change the upstream model.

### Thinking mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude requests with a top-level `thinking` config (`{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}`) also enable it. Output format is configurable in Settings → Thinking Mode.

### External providers (fallback capacity)

The **Providers** tab registers OpenAI- or Anthropic-compatible upstreams that serve requests alongside the Kiro pool, so traffic keeps flowing when every account is out of quota or cooling down.

Requests are **passed through unchanged** — only the model name is rewritten — so a provider only serves the endpoints that speak its own protocol:

| Provider protocol | Serves |
|---|---|
| `anthropic` | `/v1/messages` |
| `openai` | `/v1/chat/completions`, plus `/v1/responses` when *Also serve /v1/responses* is on |

A mismatched endpoint skips the provider and falls through to the next upstream. **If your customers use Claude Code (which calls `/v1/messages`), the fallback provider must be an Anthropic-compatible endpoint** — an OpenAI-compatible one will never be selected for that traffic.

Routing order comes from the **Tier** field, which Kiro accounts share (account detail → Routing Priority). Tiers are tried in ascending order, and within a tier accounts go before providers, so `acc1 = 0`, `provider = 1`, `acc2 = 2` yields exactly that chain. Leaving everything at the default `0` preserves the previous behaviour: the whole Kiro pool is tried first and providers act as pure fallback.

Each provider carries its own price table in **credits per 1M tokens** (input / output / cache write / cache read), billed against the same credit balance customers buy. Token counts are read from the provider's own usage report; for OpenAI-protocol streaming this requires `stream_options.include_usage`, which is injected automatically when the client did not set it (the upstream then emits one extra final chunk carrying only usage).

### Outbound proxy

Configure in Settings → Outbound Proxy. Supports SOCKS5 and HTTP. This fork adds a **shared proxy pool** with health tracking and failover, and a **Require-proxy** toggle that blocks outbound Kiro requests if no proxy is available (prevents leaking the server's real IP). Changes take effect immediately without a restart.

## Environment variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config at startup) | - |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LOOPBACK_HOST` | Host to bind the SSO loopback server. **Set to `0.0.0.0` in Docker.** | `127.0.0.1` |
| `KIRO_MAX_BODY_BYTES` | Max request body size (`0` = disable) | `10485760` (10 MiB) |
| `KIRO_MAX_CONCURRENT` | Global concurrent-request cap | `256` |
| `KIRO_IP_RPM` | Requests/minute/IP before reject | `120` |
| `KIRO_PER_KEY_INFLIGHT` | Concurrent RPM-delayed requests per key before 429 | `8` |
| `KIRO_TRUST_PROXY` | Read the real client IP from `X-Forwarded-For` / `X-Real-IP`. Only enable behind a trusted reverse proxy. | `false` |

## Troubleshooting

**`Refusing to start: admin password is still the default on a non-loopback host`.** This is a deliberate safety guard: the app won't boot when the admin password is still `changeme` **and** the host is not loopback (e.g. `0.0.0.0`). Two fixes: for local use, set `"host": "127.0.0.1"` in `data/config.json` (you can keep `changeme`); for a public/Docker deploy, set a strong `ADMIN_PASSWORD` env var instead. Never expose `0.0.0.0` with the default password.

**"All loopback ports busy" / Start Login returns HTTP 500.** Usually a bad `LOOPBACK_HOST` (e.g. `0.0.0` missing an octet). In Docker, it must be exactly `0.0.0.0`. Check with `docker compose exec kiro-go printenv LOOPBACK_HOST`, fix the compose file, then `docker compose up -d --force-recreate` (env is baked at container creation). Starting a new login also cancels pending sessions to free leaked loopback ports.

**App runs an old version after a code change.** `docker compose up` won't rebuild a cached image. Run `docker compose up -d --build`.

**Compose fails on port `49153`/`5015x` ("address already in use") on macOS.** Those ports are in the OS ephemeral range. They're unnecessary — only the low SSO ports (3128–9091) are needed. Remove the `49153`–`53153` mappings.

**Getting 503 on a specific model.** The client is likely requesting a model that doesn't exist upstream. Use Force Model or per-key Model to remap it to a real one.

**Per-IP rate limits look wrong behind a proxy.** If you run Nginx/Cloudflare in front, set `KIRO_TRUST_PROXY=true` — otherwise every request looks like it comes from `127.0.0.1`. If you're exposing Go directly, keep it `false`, or attackers can spoof `X-Forwarded-For`. See [deploy/HARDENING.md](deploy/HARDENING.md).

**Lost config / accounts after a crash.** Config lives in `data/config.json`; mount `/app/data` as a volume so it survives container recreation. Always run behind the `./data` volume in `docker-compose.yml`.

For production DoS/DDoS hardening (Cloudflare + Nginx + fail2ban), see [deploy/HARDENING.md](deploy/HARDENING.md).

## Development

```bash
go build -o kiro-go .          # build
go test ./...                  # run all tests
go test ./proxy/               # test one package
go vet ./...                   # vet
```

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
