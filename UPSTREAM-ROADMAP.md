# Upstream Roadmap

Status fork relative to `router-for-me/CLIProxyAPI` upstream as of 2026-05-26.

## Already integrated (cherry-picked manually)

### Sprint 1 (2026-05-25) — universal bugfixes

| Date | Commit (fork) | Upstream origin | Notes |
|---|---|---|---|
| 2026-05-25 | `4c00e2cb` | `ec79951e` | HTTP CONNECT proxy dialer support |
| 2026-05-25 | `3ac34eb6` | `33f4904b` | Translator: system role as developer in Claude→Codex |
| 2026-05-25 | `37ca98a6` | `8bc2eff5` | Shorten Claude codex tool call IDs |
| 2026-05-25 | `9209cf2f` | `1c2153a2` | Stabilize OpenAI→Claude streaming tool_use blocks (manual merge with fork's incremental input_json_delta) |
| 2026-05-25 | `33fb8647` | `ad868308` | Codex context-length stream errors caught mid-stream (manual merge with fork's SSE buffering + error sanitization) |
| 2026-05-25 | `69f34694` | `0ec07e57` (JSON-only) | Gemini 3.5 Flash entry in models.json |

### Sprint 2 (2026-05-26 morning) — Tier 1 + 2

| Date | Commit (fork) | Upstream origin | Tier | Notes |
|---|---|---|---|---|
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

### Sprint 3 (2026-05-26) — Tier 3 Feature A: xAI/Grok provider

| Commit (fork) | Upstream origin | Notes |
|---|---|---|
| `757be1e1` | `e4c95707` | xAI OAuth2 PKCE foundation (manual merge across server.go, auth_files.go, oauth_sessions.go, model_definitions.go, service.go) |
| `2a6f7b4d` | `2ff9e33e` | xAI Grok image models + endpoints |
| `32cf44bb` | `53d1fd6c` | xAI Grok video model support — also brings the OpenAI Videos handler |
| `31fb5e89` | `ddd10539` | Normalize xAI input reasoning items + tool tests |
| `7a0d4d72` | `8b3670b8` | Namespace tools + tool normalization (1st pass) |
| `27f4e11c` | `2607888a` | Default missing function tool parameters |
| `9ca774d2` | `74cb53de` | Namespace tools (2nd pass) |
| `78a5b454` | `bac006e7` | xAI thinking provider with reasoning.effort |
| `9b1af0e6` | `aaec9194` | Grok Build 0.1 model registry entry |

### Sprint 4 (2026-05-26) — Tier 3 Feature B: Codex client models

| Commit (fork) | Upstream origin | Notes |
|---|---|---|
| `98947f8f` | `088ab33d` | Codex client models foundation (Home-aware handlers stripped — depends on full cluster mode) |
| `af911d72` | `96754f5a` | Move Codex client models to registry package |
| `c3f97f10` | `de039491` | Expanded reasoning levels (`none`/`minimal`/`unsupported`) |
| `8b9cb959` | `efa200ec` | `fetch_codex_models` CLI (does not affect runtime) |

## Tier 3 Feature C — OpenAI Videos handler — partially integrated

`/v1/videos` route + `openai_videos_handlers.go` came along with `32cf44bb` (xAI video models) since they share infrastructure. **Functional from xAI side immediately.**

What's NOT taken: `feebe6c7 feat(api): add OpenAI compatibility for image models` foundation. That commit refactors `helps.ApplyPayloadConfigWithRequest` (new signature, `payloadModelRulesMatch` change, `resolvePayloadRulePaths`), and the chain of helpers makes it a multi-file refactor that conflicts with fork's existing `payload_helpers.go`. **Plan when adopting OpenAI image-gen for non-xAI providers**: cherry-pick `feebe6c7`, port the new helper signature, migrate fork's existing payload helpers to the new shape, run the disable_image_generation tests.

## Tier 3 Feature D — Cluster mode — DEFERRED

**Why deferred:** Genfity runs a single VPS today. Cluster mode requires:
- Redis cluster (not the single Redis instance currently used for caching)
- Multi-instance VPS or k8s setup
- mTLS bootstrap configuration
- Operational changes to deployment pipeline

Plus, every cluster commit modifies `internal/api/server.go` heavily, which fork has already extensively customized. The first attempted cherry-pick (`3a9fb378 fix(home): JoinHostPort`) hit conflicts in `server.go` even though it's a tiny fix.

**Adoption plan when business needs HA:**

1. Provision Redis cluster (DigitalOcean Managed Redis or self-hosted).
2. Provision a second VPS in a different AZ.
3. Cherry-pick in this order, resolving server.go conflicts each time:

| Step | Commit | Description |
|---|---|---|
| 1 | `48104abf` | Home control plane integration with Redis + TLS (foundation) |
| 2 | `c66fa376` | Cluster nodes payload parsing |
| 3 | `644d5ea6` | Cluster discovery toggle |
| 4 | `7a1a3408` | net.JoinHostPort consistency |
| 5 | `bbe30f53` | Home certificate + CA fingerprint verify |
| 6 | `77ba15f7` | mTLS bootstrap via JWT |
| 7 | `ed0ac683` | HOME_ADDR / HOME_PASSWORD env vars |
| 8 | `5f039654` | Move home env vars after godotenv |
| 9 | `605adaa3` | Local management password validation |
| 10 | `a726e373` | Redis subscription + queue operations |
| 11 | `bb5ac40a` | Redis client timeout handling |
| 12 | `7efc1629` | docker-compose.cluster.yml |
| 13 | `bcbb9490` | Cluster node failover + reconnection |
| 14 | `1d529c3c` / `9d01c80d` | Pub/Sub usage tracking |
| 15 | `3a9fb378` | Home dispatch headers |
| 16 | `437aa87c` | Dynamic Gemini handler with home integration |
| 17 | `412d3442` | RequestID in home request logging |
| 18 | `a0bb1f3a` | File-backed log sources |
| 19 | `82c9e0de` | zstd decoding for request logs |

Estimate: 1-2 days of focused work for a developer who already has Redis cluster experience.

## Skipped — refactors that conflict with fork's contributions

| Type | Reason |
|---|---|
| `auth_files.go` 760-line refactor | Fork has heavy auth flow modifications for Genfity OAuth providers (Kiro/GitHub/Qwen/Cline/KiloCode/Cursor) |
| `conductor.go` 466-line refactor | Fork modified for model_cooldown sanitization and resilience layer |
| `service.go` 142-line refactor | Fork's executor binding pattern intentionally different |
| `model_definitions.go` full rewrite | Fork added Genfity-specific model entries; upstream restructured the data model |
| `feebe6c7` OpenAI image foundation | Refactors `payload_helpers.go` API across multiple files; needed only when OpenAI image-gen is wanted for non-xAI providers |
| `66c5d60b` removed `newTestServerWithOptions` | Test infrastructure change with no functional value |
| `9ef99aa7` rename `FormProtocol`→`FromProtocol` | Could conflict with fork code; low value |
| README updates (`50d19e20`, `5ef76939`, `7f68fa24`, `67f22514`) | Sponsor/docs noise |

## Update workflow

When new upstream commits land:

```bash
git fetch upstream main
git log --oneline origin/main..upstream/main   # what's new
git show <sha> --stat                          # see what each touches
git cherry-pick <sha>                          # try; resolve as needed
```

For larger features, branch off, cherry-pick, run `go build ./...` + `go test ./...`, then merge back to main.

## Recovery if a cherry-pick goes sideways

```bash
git cherry-pick --abort     # roll back the in-progress cherry-pick
git reset --hard origin/main # nuclear: throw away local commits not on remote
git revert <bad-sha>        # safer: undo a pushed commit with a new commit
```
