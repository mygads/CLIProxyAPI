# Upstream Roadmap

Status fork relative to `router-for-me/CLIProxyAPI` upstream as of 2026-05-26.

## Already integrated (cherry-picked manually)

| Date | Commit (fork) | Upstream origin | Tier | Notes |
|---|---|---|---|---|
| 2026-05-25 | `4c00e2cb` | `ec79951e` | 1 | HTTP CONNECT proxy dialer support |
| 2026-05-25 | `3ac34eb6` | `33f4904b` | 1 | Translator: system role as developer in Claude→Codex |
| 2026-05-25 | `37ca98a6` | `8bc2eff5` | 1 | Shorten Claude codex tool call IDs |
| 2026-05-25 | `9209cf2f` | `1c2153a2` | 1 | Stabilize OpenAI→Claude streaming tool_use blocks (manual merge with fork's incremental input_json_delta) |
| 2026-05-25 | `33fb8647` | `ad868308` | 1 | Codex context-length stream errors caught mid-stream (manual merge with fork's SSE buffering + error sanitization) |
| 2026-05-25 | `69f34694` | `0ec07e57` (JSON-only) | 1 | Gemini 3.5 Flash entry in models.json |
| 2026-05-26 | `1b5cddff` | `33130f18` | 2 | Antigravity: require project_id at auth time |
| 2026-05-26 | `44e8fad5` | `809feb1e` | 1 | Antigravity: mask project_id in logs |
| 2026-05-26 | `c95cd166` | `bfdc0b39` | 1 | Antigravity credits fallback gate scoped correctly |
| 2026-05-26 | `380e160d` | `d606faa9` | 1 | Strip Claude Code attribution from non-Anthropic translations |
| 2026-05-26 | `17c19aad` | `be841b88` | 1 | Replace panic with warning on embedded model parse failure |
| 2026-05-26 | `8071278f` | `1583cb4e` | 1 | Cap Gemini max output tokens |
| 2026-05-26 | `69dc1264` | `32a0d69b` | 1 | Fix Antigravity Gemini thought signatures |
| 2026-05-26 | `4bf74a9b` | `ad98c954` | 2 | Track upstream response headers in logging and usage reporting |
| 2026-05-26 | `f74f6115` | `0de0ad0d` | 2 | Reasoning effort surfaced in usage events |
| 2026-05-26 | `aee52a7b` | `1c632d15` | 1 | Translator: skip empty text parts in Claude request conversion |
| 2026-05-26 | `c18f8fa6` | (post-fix) | — | Repair v7 import path in cherry-picked auth test |

## Pending — Tier 3 features (need feature-by-feature manual merge)

These features were attempted but require coordinated work across files the fork heavily diverged on (`server.go`, `model_definitions.go`, `service.go`, `auth_files.go`, `oauth_sessions.go`, `conductor.go`). Each one needs a focused session — they are NOT a single cherry-pick.

### Feature A — xAI / Grok provider

**Why pick:** add Grok-4 as a routable provider in combos. Some customers prefer Grok for technical writing.

**Cost:** medium-high. Touches the same five fork-critical files at multiple call sites.

**Commits (chronological):**

| SHA | Description |
|---|---|
| `2607888a` | fix(xai): default missing function tool parameters |
| `e4c95707` | feat(auth): OAuth2 with PKCE + token persistence (foundation) |
| `2ff9e33e` | feat(api,xai): integrate xAI Grok image models + endpoints |
| `53d1fd6c` | feat(api,xai): xAI Grok video model support |
| `ddd10539` | feat(xai): normalize xAI input reasoning items |
| `8b3670b8` | feat(xai): namespace tools + tool normalization |
| `74cb53de` | feat(xai): namespace tools (follow-up) |
| `4b13f9c2` | merge: ben-vargas/fix-grok-tool-params |
| `bac006e7` | feat(thinking): xAI provider with reasoning.effort |
| `aaec9194` | feat(models): Grok Build 0.1 in registry |

**Conflict areas:** `server.go` (route registration), `config.go` (xai config struct), `auth_files.go` (xai auth file format), `service.go` (executor binding), `model_definitions.go` (Grok model defs).

**Plan when adopting:**
1. Cherry-pick `e4c95707` first; resolve `server.go` conflict by keeping fork's combo/management routes AND adding the xai routes.
2. Resolve `config.go` by ADDING the xAI config struct (not replacing).
3. Resolve `model_definitions.go` by ADDING Grok models (not replacing the registry refactor).
4. Apply remaining commits in chronological order.
5. Add `XAI_*` env vars to deployment `.env`, redeploy.

### Feature B — Codex client models foundation

**Why pick:** dynamic Codex model registry — adds endpoints that surface a JSON catalog of Codex models with metadata. Required by `de039491` (expanded reasoning levels) and `efa200ec` (CLI fetcher).

**Cost:** medium. Touches `server.go`, `xai_executor.go` (so requires Feature A first).

**Commits:**

| SHA | Description |
|---|---|
| `088ab33d` | foundation (depends on xai_executor.go from Feature A) |
| `96754f5a` | refactor: move codex client models to registry package |
| `de039491` | expand reasoning levels (`none`/`minimal`/`unsupported`) |
| `efa200ec` | CLI: fetch_codex_models for dynamic refresh |

**Plan when adopting:** Adopt **after** Feature A. Then cherry-pick chronologically. The CLI is `cmd/fetch_codex_models/main.go` — runtime is unaffected.

### Feature C — OpenAI Videos handler

**Why pick:** add `/v1/videos` endpoint for OpenAI sora-style video generation. Useful only when a customer needs it.

**Cost:** medium. Depends on `feebe6c7 feat(api): OpenAI compatibility for image models` which touches `config.go`, `model_registry.go`, `executor` interfaces.

**Commits:**

| SHA | Description |
|---|---|
| `feebe6c7` | feat(api): OpenAI image model compatibility (foundation) |
| (xAI image/video commits in Feature A overlap — adopt Feature A first) |

**Plan when adopting:** Adopt **after** Feature A. Resolve `config.go` and `model_registry.go` carefully — fork's allowedModels logic stays.

### Feature D — Cluster mode

**Why pick:** horizontal scaling, HA via Redis cluster + Home control plane. Multi-instance VPS with shared usage tracking, auth file sync, breaker state.

**Cost:** highest. Lots of new infrastructure (`internal/home/`, `internal/redisqueue/` enhancements, mTLS bootstrap). Touches `internal/api/server.go` heavily.

**Commits (sample, chronological):**

| SHA | Description |
|---|---|
| `48104abf` | feat(home): control plane integration with Redis + TLS |
| `c66fa376` | feat(home): cluster nodes payload parsing |
| `644d5ea6` | feat(home): disable cluster discovery toggle |
| `7a1a3408` | fix(home): net.JoinHostPort consistency |
| `bbe30f53` | feat(server): Home certificate + CA fingerprint verify |
| `77ba15f7` | feat(server): mTLS certificate bootstrap via JWT |
| `ed0ac683` | feat(server): HOME_ADDR / HOME_PASSWORD env vars |
| `a726e373` | feat(redis): subscription + queue operations |
| `bb5ac40a` | feat(client): timeout handling for Redis ops |
| `7efc1629` | feat(docker): docker-compose.cluster.yml |
| `bcbb9490` | feat(client): cluster node failover + reconnection |
| `1d529c3c` / `9d01c80d` | feat(redis): Pub/Sub for usage tracking |
| `412d3442` | feat(logging): RequestID in home request logging |
| `a0bb1f3a` | feat(logging): file-backed sources for request logging |

**Conflict areas:** `server.go` (huge — central wiring), `config.go` (home config struct), `usage/manager.go` (Redis usage sync), `gitstore` interactions.

**Plan when adopting:** This is a 1-2 day project, not an afternoon task. Steps:
1. Decide if you actually need cluster (single-VPS Genfity right now doesn't).
2. Provision Redis cluster (e.g., DigitalOcean managed Redis) and a second VPS.
3. Cherry-pick Home foundation first (`48104abf` + immediate fixes), wire mTLS.
4. Add Redis cluster commits, test pubsub usage tracking with a single node first.
5. Add `docker-compose.cluster.yml` and validate failover.

## Skipped — refactors that conflict with fork's contributions

| Type | Reason |
|---|---|
| `auth_files.go` 760-line refactor | Fork has heavy auth flow modifications for Genfity OAuth providers (Kiro/GitHub/Qwen/Cline/KiloCode/Cursor) |
| `conductor.go` 466-line refactor | Fork modified for model_cooldown sanitization and resilience layer |
| `service.go` 142-line refactor | Fork's executor binding pattern intentionally different |
| `model_definitions.go` full rewrite | Fork added Genfity-specific model entries; upstream restructured the data model |
| `66c5d60b` removed `newTestServerWithOptions` | Test infrastructure change with no functional value |
| `9ef99aa7` rename `FormProtocol`→`FromProtocol` | Could conflict with fork code; low value |
| README updates (`50d19e20`, `5ef76939`, `7f68fa24`) | Sponsor/docs noise |

## Update workflow

When new upstream commits land:

```bash
git fetch upstream main
git log --oneline origin/main..upstream/main   # what's new
git show <sha> --stat                          # see what each touches
git cherry-pick <sha>                          # try; resolve as needed
```

For larger features, branch off, cherry-pick, run `go build ./...` + `go test ./...`, then merge back to main.
