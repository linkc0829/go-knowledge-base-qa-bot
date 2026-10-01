---
name: go-hex-recipes
description: Recipes for common modifications to an existing feature in this hexagonal Go backend — adding an endpoint, adding an outbound dependency (DB/cache/external API), or adding a cross-feature dependency via capability ports. Use when the user wants to extend an existing feature rather than create a new one.
---

# Recipes

## Add a new endpoint to an existing feature

1. **DTO** — add request/response struct to `dto_http.go`, with JSON + validation tags. Add `to<Entity>` / `from<Entity>` mappers if needed.
2. **Handler** — add the method on `*Handler` in `handler_http.go`. Pattern: bind → validate → call service → map domain error → respond.
3. **Route** — register in `routes.go`.
4. **Service** — if it's new business behavior, add a method on `service.go`. If it just exposes an existing operation, no service change needed.
5. **Port + adapter** — only if the endpoint needs I/O the service can't already do. Add to `ports.go` and implement in the relevant `repo_*.go` / `cache_*.go`.
6. **Tests** — extend `service_test.go` and `handler_http_test.go` (table-driven, fakes).

## Add a new outbound dependency to a service

E.g. service needs a persistent store, or an external HTTP API.

1. **Define the port** — in `ports.go`, named after the **capability**, not the provider:
   ```go
   // GOOD — internal/kb/ports.go
   type VectorStore interface {
       Load(ctx context.Context) (string, map[string][]float32, error)
       Save(ctx context.Context, identity string, vectors map[string][]float32) error
   }
   ```
   Name it for what it does, not for the storage or client behind it.
2. **Inject** — add a field to the service struct, accept it in `NewService(...)`.
3. **Implement adapter** — new file (e.g. `repo_vector.go`) implementing the port. Marshal via `dto_internal.go`. Apply `context.WithTimeout` per R3.2.
4. **Wire** — construct and pass in `internal/bootstrap/` (see `NewKBService` in `kb.go`).
5. **Tests** — update `service_test.go` with a hand-written fake for the new port.

## Add a cross-feature dependency

E.g. `gateway` needs `auth` to resolve bearer tokens. **Do not import the other feature.**

1. **Define capability port in your own `ports.go`:**
   ```go
   // internal/gateway/ports.go
   type TokenResolver interface {
       Resolve(ctx context.Context, token string) (shared.Principal, error)
       Lookup(id string) (shared.Principal, bool)
   }
   ```
2. **Verify duck-typing** — check that the provider's existing method signatures match (`auth.Store` satisfies `TokenResolver`). If not, either:
   - Adjust your port to match (preferred — your feature is the consumer)
   - Write a thin adapter struct in `internal/bootstrap/`
3. **Inject at the composition root** (`internal/bootstrap/` or `cmd/<binary>/main.go`):
   ```go
   // cmd/gateway/main.go
   h := gateway.NewHandler(..., limiter, authStore, ...)
   ```
4. **Test** — fake the new port in `service_test.go`. The test never imports the other feature.

`make lint` will reject any `import "internal/<otherFeature>"` — that's the rule working as intended.

## Add a new domain field

1. **Domain** — add the field to the struct in `domain.go`. If it has invariants, validate in `New<Entity>(...)`.
2. **Persistence** — update the repo/adapter mapping in `dto_internal.go` if the field is stored.
3. **DTO** — add to relevant DTOs in `dto_http.go` and `dto_internal.go`. Update mappers.
4. **Tests** — extend table cases.

## Change a domain rule

Rule lives in `domain.go` (pure functions / constructor validation) or `service.go` (multi-step orchestration). Never in `handler_*.go` or `repo_*.go`.

- Invariant on a single entity → `domain.go` (e.g. `NewOrder` rejects negative amount).
- Rule involving multiple entities or external state → `service.go`.

Add a test case to `service_test.go` (or domain-level test if the rule is pure).

## Verify a change

```bash
make lint    # depguard + static checks
make test    # unit tests with fakes
```
