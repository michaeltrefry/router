# internal/api — AGENTS

> **Mirror notice.** Generated from [CLAUDE.md](CLAUDE.md). Edit CLAUDE.md, then run `make generate-agent-guides`; CI rejects drift.

Presentation layer. Handlers adapt HTTP ↔ Service. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Subpackages

- `admin/` — operational endpoints: `/health`, `/validate`, `/v1/client-events` (harness CLI off/on/uninstall report → log + `router.harness_lifecycle` span, nothing stored), `/admin/v1/*`, and `/v1/sessions/:session_id/cost` (authed by `middleware.WithReadKey`: an `rk_` or `ra_` key resolves its installation; per-installation `WithInstallationRateLimit`; no admission, billing or spend gates)
- `anthropic/` — Anthropic Messages surface (`/v1/messages`, passthrough, `/v1/route`)
- `openai/` — OpenAI Chat Completions (`/v1/chat/completions`)
- `gemini/` — Gemini native (`/v1beta/models/:modelAction`)
- `analytics/` — read-only routing-decision export (`/v1/analytics/routing-decisions`, `/models`, `/schema`). Authed by `ra_` analytics keys via `middleware.WithAnalyticsKey` **only** — no `WithAuth`, no balance check, no spend cap, since nothing here can route or spend.
- `subscriptions/` — authenticated (`WithAuth`) subscription surface. `/v1/subscriptions/accounts` manages router-held accounts (mounted only when managed accounts are configured). `GET /v1/subscriptions/usage` is a read-only view of `internal/proxy/usage` quota state for exactly the credentials the caller can be routed onto: subscription tokens presented on that request (`source: presented`), pass-through tokens this process recorded on inference turns sent with the same router key ID (`observed`, so a poller holding only the router key sees its Claude Code / Codex workers), and the managed accounts serving admission grants the caller (`managed` with `account_id`; another member's shared account as `shared`, state only, no account id or display name). It reuses the candidate listing `WithAuth` already ran (live subject membership) rather than querying again. Each credential reports `routable` (false = disabled / reconnect_required, needs a human), managed `state`/`enabled`/`cooldown_until`, `exhausted` (spent window, paid overage, or active cooldown — what the router will not treat as free capacity) and `resumes_at`. Top level: `all_exhausted` is true only when at least one routable credential is known and all are exhausted; `resumes_at` is the earliest known instant one becomes usable; `known_credentials` / `observed_credentials` separate "nothing known" from headroom — `observed: false` is not headroom. Credentials appear only as the salted `credential_key`, which is stable only within one process (the salt is per process); never echo a token. Readings and the router-key index are per-process memory, so a multi-worker deployment answers from the serving worker only.
- `feedback/` — no-login feedback-link surface (`/f/<token>`, rating submit). The token itself (signed via [`internal/feedback`](../feedback)) is the sole credential, so this is the one subpackage that **deliberately carries no auth middleware** — do not add `WithAuth`/`WithAdminOnly` here; that would break the whole point of a shareable no-login link.

## Import rules

- May import `internal/auth` (Service handle + middleware-context types) and `internal/proxy` (routing/dispatch service handle).
- May import `internal/observability` for logging, `internal/providers` for shared sentinel errors, `internal/router/cluster` for `ErrClusterUnavailable` sentinel + `DeployedModelsSource` interface, `internal/analytics` for the export Service handle + row/schema types.
- **Must not import** `internal/postgres`, any concrete `internal/providers/*` adapter, or `internal/translate` directly.
- Concrete instances reach presentation only via constructor params from composition root.

## Adding an HTTP endpoint

1. **Decide timeout budget.** Cheap auth-only ops use `validateTimeout` / `healthTimeout` (1 s). Provider calls get own constant in [`../server/server.go`](../server/server.go) — pick budget + justify in comment.
2. **Decide auth.** Routes needing valid `rk_` bearer go through `middleware.WithAuth(authSvc)`. Admin endpoints use `WithAdminOrAuth` (admin cookie OR bearer) or `WithAdminOnly` (admin cookie only). Unauthed routes (e.g. `/health`) attach no auth middleware.
3. **Decide if self-hoster dashboard surface.** `/ui/*` static dashboard, `/admin/v1/auth/*`, `/admin/v1` mgmt group (metrics, keys, provider-keys, config, excluded-models) mount only when `mode == server.DeploymentModeSelfHosted`. New endpoints whose only consumer is self-hosted dashboard go inside that block; product-surface endpoints (`/v1/*`, `/v1beta/*`, `/health`, `/validate`) stay outside so they're available in `managed` mode too. **Do not** add new `/admin/v1/*` route outside the selfhosted block — would re-expose redundant control plane on Weave-managed deploys.
4. **Pick (or create) the right subpackage.** Operational → `admin/`; Anthropic Messages → `anthropic/`; OpenAI → `openai/`; Gemini → `gemini/`; no-login feedback-link surface → `feedback/`. New surfaces get their own subpackage.
5. **Use `observability.FromGin(c)` for request-scoped logger.** For authed installation: `middleware.InstallationFrom(c)` (nil if `WithAuth` not applied — handler should be on authed group). For BYOK secrets: there's no gin-context accessor — `WithAuth` stashes them on the *request* context via `context.WithValue(ctx, proxy.ExternalAPIKeysContextKey{}, externalKeys)` (see [`../server/middleware/auth.go`](../server/middleware/auth.go)). Handlers don't read this directly; they forward `c.Request.Context()` into `*proxy.Service` calls, which pull the keys back out internally via `ctx.Value(proxy.ExternalAPIKeysContextKey{})`.
6. **Pick the right service.** Identity-only ops → `*auth.Service`. Routing/dispatch/translate → `*proxy.Service`. Don't touch repositories, router, providers, planner/handover/cache packages from a handler. Handler adapts HTTP ↔ service; service does the work.
7. **Test with in-memory fakes + gin testing harness** (`httptest.NewRequest`/`ResponseRecorder`). No real DB for handler tests — use fakes from [`../auth/service_test.go`](../auth/service_test.go) and [`../proxy/service_test.go`](../proxy/service_test.go) as model.

## History

`internal/router/heuristic` and `internal/router/evalswitch` previously lived in the API ring; both removed when heuristic fallback retired in favor of `cluster.ErrClusterUnavailable` → HTTP 503.
