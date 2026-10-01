---
name: go-hex-antipatterns
description: Review or refactor Go code in this hexagonal backend for layer-violation anti-patterns — handlers with business logic, services taking concrete drivers, repos with validation, cross-feature imports, error swallowing, leaking domain entities through HTTP. Use when reviewing a PR, refactoring a feature, or when the user asks "is this idiomatic for this repo".
---

# Hexagonal Anti-Patterns (BAD / GOOD)

When reviewing code in this repo, scan for these specific layer violations. Each is rejected in code review and most will fail `make lint`.

## 1. Marshalling domain entities directly

```go
// BAD — leaks internal domain fields, couples HTTP to domain
c.JSON(200, order) // order is domain.Order

// GOOD — explicit mapping lives in dto_http.go
c.JSON(200, toOrderResponse(order))
```

## 2. Business logic in handler

```go
// BAD
if req.Amount > 10000 {
    order.Status = "pending_review"
}

// GOOD — rule lives in domain or service
result, err := h.svc.PlaceOrder(ctx, req.toInput())
```

## 3. Concrete types in service constructor

```go
// BAD
func NewService(llm *openai.Client) *Service

// GOOD — accept interfaces, R3.4
func NewService(llm LLM) *Service
```

## 4. Cross-feature import

```go
// BAD — kb package imports auth package
import "github.com/linkc0829/llm-platform/internal/auth"

// GOOD — kb defines a capability port in its own ports.go; bootstrap wires auth into it
// internal/kb/ports.go
type <Capability> interface { ... }
```

This is enforced by depguard (R1.4) — it will fail `make lint`. If you see it pass lint, the rule glob is wrong.

## 5. Business logic in repo

```go
// BAD — in a repo_*.go adapter
if order.Amount.IsZero() {
    return ErrInvalidOrder
}

// GOOD — validation lives in domain.NewOrder constructor (R3.7)
```

## 6. Error swallowing / re-stringifying

```go
// BAD — loses original error, breaks errors.Is/As chain
return errors.New("failed to save")
return fmt.Errorf("failed to save: %s", err.Error())

// GOOD — wrap with %w (R3.3)
return fmt.Errorf("save order to postgres: %w", err)
```

## 7. Returning domain from HTTP handler without mapping

```go
// BAD
order, _ := h.svc.GetOrder(ctx, id)
c.JSON(200, gin.H{"order": order})

// GOOD
order, err := h.svc.GetOrder(ctx, id)
if err != nil { ... handle ... }
c.JSON(200, toOrderResponse(order))
```

## 8. Missing context / timeout on external calls

```go
// BAD — no timeout on DB call
rows, err := r.db.Query(ctx, sql, id)

// GOOD — bound external work (R3.2)
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
rows, err := r.db.Query(ctx, sql, id)
```

## 9. `fmt.Println` / `log.Printf` in production code

```go
// BAD
fmt.Println("user created:", user.ID)

// GOOD — use project logger (R3.6)
logger.Info(ctx, "user created", "user_id", user.ID)
```

## 10. Zero-value invalid domain construction

```go
// BAD
order := domain.Order{}
order.Amount = -5  // negative amount silently accepted

// GOOD — constructor validates (R3.7)
order, err := domain.NewOrder(userID, amount)
if err != nil { return err }
```

## 11. Mapping domain errors → HTTP status in service or repo

The repo returns domain errors; the **handler** maps to HTTP status via `errors.Is`:

```go
// In handler_http.go
order, err := h.svc.GetOrder(ctx, id)
switch {
case errors.Is(err, ErrOrderNotFound):
    c.JSON(404, errResp(err))
case err != nil:
    c.JSON(500, errResp(err))
}
```

## Quick review checklist

When given a diff, scan in this order:
1. Any new import of `internal/<otherFeature>`? → R1.4 violation.
2. Any concrete client (`*openai.Client`, `*gin.Engine`) in a service signature? → R3.4 violation.
3. Any `if`/`switch` on business state inside a `handler_*.go`? → move to service/domain.
4. Any `fmt.Errorf("...: %s", err.Error())` or `errors.New(err.Error())`? → use `%w`.
5. Any direct `c.JSON(..., domainEntity)`? → add mapper in `dto_http.go`.
6. Any external call without `context.WithTimeout`? → add one (R3.2).
7. Any `domain.Foo{}` literal outside a constructor or test? → use `NewFoo(...)`.
