# Project Rules for AI

> Source of truth for how to write code in this repo. Violations of `R1.x`
> rules will fail `golangci-lint` (depguard). For deeper guidance not covered
> here, the following skills load on demand:
>
> - `new-feature` — full checklist for adding a feature
> - `go-hex-antipatterns` — BAD/GOOD examples for review/refactor
> - `go-hex-recipes` — recipes for common tasks (new endpoint, new port, cross-feature dep)
>
> For general Go style (naming, errors, packages, testing, etc.), defer to the
> installed `go-*` skills. This file only states what is **specific to this
> repo's hexagonal layout.**

## Rules

These rules apply to every task in this project unless explicitly overridden.
Bias: caution over speed on non-trivial work. Use judgment on trivial tasks.

### Rule 1 — Think Before Coding
State assumptions explicitly. If uncertain, ask rather than guess.
Present multiple interpretations when ambiguity exists.
Push back when a simpler approach exists.
Stop when confused. Name what's unclear.

### Rule 2 — Simplicity First
Minimum code that solves the problem. Nothing speculative.
No features beyond what was asked. No abstractions for single-use code.
Test: would a senior engineer say this is overcomplicated? If yes, simplify.

### Rule 3 — Surgical Changes
Touch only what you must. Clean up only your own mess.
Don't "improve" adjacent code, comments, or formatting.
Don't refactor what isn't broken. Match existing style.

### Rule 4 — Goal-Driven Execution
Define success criteria. Loop until verified.
Don't follow steps. Define success and iterate.
Strong success criteria let you loop independently.

### Rule 5 — Use the model only for judgment calls
Use me for: classification, drafting, summarization, extraction.
Do NOT use me for: routing, retries, deterministic transforms.
If code can answer, code answers.

### Rule 6 — Token budgets are not advisory
Per-task: 4,000 tokens. Per-session: 30,000 tokens.
If approaching budget, summarize and start fresh.
Surface the breach. Do not silently overrun.

### Rule 7 — Surface conflicts, don't average them
If two patterns contradict, pick one (more recent / more tested).
Explain why. Flag the other for cleanup.
Don't blend conflicting patterns.

### Rule 8 — Read before you write
Before adding code, read exports, immediate callers, shared utilities.
"Looks orthogonal" is dangerous. If unsure why code is structured a way, ask.

### Rule 9 — Tests verify intent, not just behavior
Tests must encode WHY behavior matters, not just WHAT it does.
A test that can't fail when business logic changes is wrong.

### Rule 10 — Checkpoint after every significant step
Summarize what was done, what's verified, what's left.
Don't continue from a state you can't describe back.
If you lose track, stop and restate.

### Rule 11 — Match the codebase's conventions, even if you disagree
Conformance > taste inside the codebase.
If you genuinely think a convention is harmful, surface it. Don't fork silently.

### Rule 12 — Fail loud
"Completed" is wrong if anything was skipped silently.
"Tests pass" is wrong if any were skipped.
Default to surfacing uncertainty, not hiding it.


## R0. Local setup

| ID | Rule |
|----|------|
| R0.1 | **Do not auto-create `.env`.** `.env` is gitignored. Tell the user to `cp .env.example .env` and set `KB_AUTH_FILE` (or `KB_AUTH_DISABLED=true` for local-only runs); do not write a `.env` file on their behalf unless explicitly asked. |

## Architecture

**Hexagonal Architecture (Ports & Adapters) with a Feature-first Package Layout.**

- Each feature is a single Go package under `internal/<feature>/`.
- Inside the package: domain, service, ports, and adapters live side-by-side as separate files. Go's package boundary enforces the hexagon's edge.
- `internal/shared/` — zero-dependency value objects shared by features.
- `internal/platform/` — infrastructure utilities (DB pool, logger, etc.).
- `internal/bootstrap/` — composition root (`services.go`, `kb.go`); the only place features get wired together.

### Request flow

```
HTTP Request → handler_http.go → service.go → ports.go → adapters (repo_markdown.go, repo_vector.go, adapter_openai.go)
```

### Cross-feature communication

Feature A may need a capability from Feature B. **A must NOT import B directly.**

1. A defines a capability interface in its own `ports.go`, named after the *capability*, not the provider:
   ```go
   // internal/order/ports.go
   type PaymentCharger interface {
       Charge(ctx context.Context, userID shared.UserID, amount shared.Money) error
   }
   ```
2. B's service structurally satisfies that interface (Go duck typing).
3. `internal/bootstrap/` injects B's service as A's port.

---

## R1. Dependency Rules (enforced by depguard)

| ID | Rule |
|----|------|
| R1.1 | `domain.go` — stdlib + `internal/shared/` only. No platform, no adapters, no third-party drivers. |
| R1.2 | `service.go` — no third-party drivers or web frameworks. Depends only on interfaces in its own `ports.go`. |
| R1.3 | `handler_*.go` — no repo / cache / adapter imports. Depends only on the service interface and DTOs. |
| R1.4 | `internal/<featureA>` must not import `internal/<featureB>`. Use port injection. |
| R1.5 | `internal/shared/` — zero-dependency value-object kernel. No platform, no third-party. |

---

## R2. File Responsibilities

| File | Responsibility | Forbidden |
|------|---------------|-----------|
| `domain.go` | Entities, value objects, pure-function business rules. | `context.Context`, I/O, framework imports |
| `service.go` | Use-case orchestration. Methods take `ctx context.Context` first. Calls ports. | Direct adapter calls, business rules belonging in domain |
| `ports.go` | Outbound interfaces (repo, cache, external API, cross-feature ports). | Implementation, non-interface structs |
| `errors.go` | Sentinel errors and error types. Names: `ErrXxxNotFound` / `ErrXxxInvalid`. | Wrapping logic tied to one call site |
| `dto_http.go` | HTTP request/response structs. JSON + validation tags. Mapping `to<Entity>` / `from<Entity>`. | Domain logic |
| `dto_internal.go` | Persistence rows, cache values, cross-feature DTOs. | HTTP-only concerns |
| `handler_http.go` | Parse → validate → call service → map → respond. Maps domain errors to HTTP via `errors.Is`. | Business logic, direct adapter access |
| `routes.go` | `RegisterRoutes(rg *gin.RouterGroup, h *Handler)`. | Handler implementation |
| `repo_*.go` / `adapter_*.go` | Implements an outbound port (storage, vector index, LLM). Maps domain ↔ wire/storage shapes via `dto_internal.go`. | Business logic |
| `service_test.go` | Unit-tests service with fake ports. | Real network / LLM calls |
| `handler_http_test.go` | `httptest` + fake service. | Real network / LLM calls |

---

## R3. Coding Conventions

| ID | Rule |
|----|------|
| R3.1 | Service / repo / external-call methods take `ctx context.Context` first. |
| R3.2 | All external calls (DB, Redis, HTTP, Kafka) have a timeout via `context.WithTimeout`. |
| R3.3 | Wrap errors with context: `fmt.Errorf("save order: %w", err)`. Never `errors.New(err.Error())`. |
| R3.4 | Service constructors accept interfaces only — never `*pgxpool.Pool`, `*redis.Client`, `*gin.Engine`. |
| R3.6 | Use `internal/platform/logger`. `fmt.Println` / `log.Printf` forbidden in production code. |
| R3.7 | Domain entities expose `New<Entity>(...)` constructors that validate invariants. Zero-value invalid state is forbidden. |

---

## R5. Testing Conventions

| ID | Rule |
|----|------|
| R5.1 | Service tests replace every port with a hand-written fake (see `internal/kb/fake_llm.go`). Cover happy, validation, port-error, and edge cases. Target ≥ 80%. |
| R5.2 | Handler tests use `httptest` + a fake service. Verify status, response shape, validation. |
| R5.4 | Table-driven by default. Case names use `snake_case`. |

---

## Adding a new feature

Invoke the `new-feature` skill — it has the full checklist, scaffolding steps, and wiring edits. Depguard rules for `domain.go` / `service.go` / `handler_*.go` are glob-based and auto-cover new features, but **cross-feature isolation** (`no-cross-feature-<name>`) requires one explicit block per feature in `.golangci.yml`, alongside the `internal/bootstrap/` wiring.

## Reviewing or refactoring existing code

Invoke the `go-hex-antipatterns` skill for BAD/GOOD pairs (handler business logic, marshalling domain entities, error swallowing, etc.).

## Common tasks

Invoke the `go-hex-recipes` skill: add an endpoint to existing feature, add an outbound dependency, add a cross-feature dependency.
