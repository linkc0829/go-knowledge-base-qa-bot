---
name: new-feature
description: Scaffold and wire up a new feature in this hexagonal Go backend (e.g. "add an inventory feature", "create a new shipments package", "implement the notifications module"). Walks through the per-feature file structure, bootstrap wiring, and depguard isolation. Use when the user wants to add a new feature package under internal/.
---

# Adding a new feature

This repo uses hexagonal architecture with a feature-first layout. Each feature is a single Go package. `internal/kb/` is the slice with every layer, including outbound adapters (`repo_markdown.go`, `repo_vector.go`, `adapter_openai.go`) and a hand-written fake (`fake_llm.go`); `internal/gateway/` is a smaller slice with no service layer.

## Step 1 — Confirm scope with the user

Before scaffolding, confirm:
- Feature name (snake_case package, e.g. `inventory`)
- Endpoints (method + path)
- Whether it needs cross-feature ports (does it call `auth`, `kb`, `gateway`?)
- Which outbound dependencies it needs (storage, LLM, external HTTP)

## Step 2 — Scaffold the package

Copy the files you need from `internal/kb/` to `internal/<feature>/`, rename, and gut the bodies. Create the files your feature needs:

- [ ] `internal/<feature>/domain.go` — entities, value objects, `New<Entity>` constructors (R3.7)
- [ ] `internal/<feature>/errors.go` — `ErrXxxNotFound` / `ErrXxxInvalid` sentinels
- [ ] `internal/<feature>/ports.go` — outbound interfaces (repo, cache, cross-feature)
- [ ] `internal/<feature>/service.go` — use-case orchestration, all methods `ctx context.Context` first
- [ ] `internal/<feature>/dto_http.go` — request/response structs + `to<Entity>` / `from<Entity>` mappers
- [ ] `internal/<feature>/dto_internal.go` — conversion helpers between domain entities and storage / wire shapes
- [ ] `internal/<feature>/handler_http.go` — parse → validate → service → map → respond
- [ ] `internal/<feature>/routes.go` — `RegisterRoutes(rg *gin.RouterGroup, h *Handler)`
- [ ] `internal/<feature>/repo_*.go` / `adapter_*.go` — one per outbound port, only when needed
- [ ] `internal/<feature>/service_test.go` — table-driven, fake ports
- [ ] `internal/<feature>/handler_http_test.go` — `httptest` + fake service

## Step 3 — Wire it up

- [ ] In `internal/bootstrap/`, add a constructor that builds the adapters and the service (see `NewKBService` in `kb.go`), and expose it via `Services` in `services.go` if other code needs it.
- [ ] In the serving binary's `cmd/<binary>/main.go`, construct the handler and call `<feature>.RegisterRoutes(...)` (see `cmd/kb/main.go`).
- [ ] If cross-feature: define the capability port in the **caller's** `ports.go`, then inject in `internal/bootstrap/`.
- [ ] **Add a depguard block** in `.golangci.yml`: copy the `no-cross-feature-kb` block, rename to `no-cross-feature-<name>`, and add the new feature's path to the sibling blocks. The `domain` / `service` / `handler` rules are glob-based and pick up the new feature automatically; cross-feature isolation is not.

## Step 4 — Verify

```bash
make lint     # depguard will catch R1.* violations
make test
```

If `make lint` fails on depguard: the rules are glob-based (`internal/*/domain.go` etc.) so a new feature should be auto-enforced. If you see a violation, the rule is right — fix the code, don't loosen the rule.

## Reference: feature to copy from

`internal/kb/` shows every layer, multiple outbound adapters, and fakes; `internal/gateway/` is a smaller slice without a service layer; `internal/auth/` shows HTTP and MCP middleware.

## Common gotchas

- **Don't import another feature package.** Define a port in your own `ports.go` and inject from `internal/bootstrap/`. (R1.4)
- **Don't return domain entities from handlers.** Map via `to<Entity>Response` in `dto_http.go`.
- **Don't put business rules in the handler or repo.** They live in `domain.go` or `service.go`.
- **Don't accept a concrete client (`*openai.Client`, `*gin.Engine`) in the service constructor.** Only interfaces. (R3.4)
