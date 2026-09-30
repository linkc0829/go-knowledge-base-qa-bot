# Knowledge Base Q&A Bot

A local Go service that indexes Markdown files from `docs/`, retrieves relevant sections with hybrid BM25 + vector search, and answers questions with cited, grounded sources — over HTTP (`/chat`) or MCP (`search_kb`) for coding agents.

The repo also ships `cmd/gateway`, a minimal inference gateway that sits in front of the OpenAI-compatible backend. It authenticates callers, caps concurrent requests, and logs usage for every LLM call, including the KB's own. See [Inference gateway](#inference-gateway).

**A fresh clone ships no corpus.** `docs/` and `eval/` are gitignored: they hold `make import` output, which is reproducible from a team's exporter bundle and runs to several megabytes of screenshots per team. Import at least one bundle before the service can answer anything — without it `POST /index` reports zero sections and every question is refused.

## Quick Start

```powershell
cp .env.example .env
# Set OPENAI_API_KEY, or use fake mode for quota-free local checks:
$env:KB_LLM_MODE="fake"
# This quick-start bypass is local-only and forces the server to 127.0.0.1:
$env:KB_AUTH_DISABLED="true"

# Required: a fresh clone has an empty docs/. See "Import a team bundle".
# FROM must be a MERGED staging tree holding every source for that team.
make import TEAM=Store.POS FROM=<staging>/kb

go run ./cmd/kb
```

In another terminal:

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:12598/index

Invoke-RestMethod -Method Post -Uri http://localhost:12598/chat `
  -ContentType "application/json" `
  -Body '{"query":"登入畫面有哪些按鈕?"}' |
  ConvertTo-Json -Depth 5
```

## Endpoints

- `GET /health` - public health check. It reports `vectors` (`ok`, `stale`, `not_indexed` or `disabled`), the chat model, its decoding parameters, and the grounding-prompt fingerprint. `stale` means the index is loaded but its vectors are missing or were built by another embedding model, so retrieval is BM25-only until `POST /index` runs again.
- `GET /ready` - public readiness probe: `200` when vectors are `ok` or `disabled`, `503` when `not_indexed` or `stale`. `/health` stays `200` in every state. See [deploy/README.md](deploy/README.md).
- `POST /index` - requires a bearer token with the `Indexer` capability; parses and audits all Markdown below `docs/`, rejects unknown or drifting section access metadata before saving or embedding, then loads the in-memory index. The request times out after `KB_INDEX_TIMEOUT` (default `60s`), and the server's write timeout is raised to match.
- `POST /chat` - requires any valid bearer token and answers a question from indexed sections only, returning `answer`, `grounded`, `sources`, `images`, `strategy`, and `session_id`. `grounded` is `false` when retrieval fell below threshold or the model declined to answer despite context — branch on it instead of parsing the answer text. Transient upstream failures are reported as such: `429` when the model provider rate-limits (echoing its `Retry-After` if it sent one), `503` when the provider is unavailable or times out. Retry those; a `500` means the request itself failed and retrying will not help.
- `/mcp` - Streamable HTTP MCP endpoint exposing the read-only `search_kb` tool; it requires a bearer token.
- `/admin/tokens` routes are registered only when authentication is enabled. The API can grant `indexer`, but it can never create or delete an admin principal.
- `GET /admin/tokens` - requires an admin bearer token; lists principal IDs, names, capabilities, and creation times without token hashes.
- `POST /admin/tokens` - requires an admin bearer token; creates a non-admin token and returns its plaintext exactly once. A request `admin` field is ignored.
- `DELETE /admin/tokens/:id` - requires an admin bearer token; revokes a non-admin principal by immutable ID. Admin principals can only be revoked by the local `kbtoken` CLI.
- `cmd/kbmcp` stdio - local process transport with no HTTP trust boundary; it intentionally runs with full access for the user who can execute the process.

## MCP for coding agents

One read-only tool, `search_kb`, with a required `query` and optional `session_id`. It returns the answer, an explicit `grounded` boolean, session ID, citations, image paths, and retrieval strategy. `grounded` lets an agent decide programmatically whether the KB answered, without parsing refusal prose — it is `false` when retrieval fell short or the model declined to answer despite context.

The same tool is served over two transports, backed by the same service and index:

| Transport | Binary | When |
| --- | --- | --- |
| Streamable HTTP, `/mcp` | `cmd/kb` (`make run`) | **Default.** One process, one index in memory, one query log |
| stdio | `cmd/kbmcp` (`make mcp`) | Development, MCP Inspector, or a client that cannot speak Streamable HTTP |

Prefer HTTP. Each stdio client spawns its own process with its own copy of the index, and its queries land in a separate log file, which makes the metrics in `log/kb.log` incomplete.

`cmd/kb` protects `/chat` and `/mcp` with the bearer token from `KB_AUTH_FILE`; `cmd/kbmcp` deliberately has no token header because the process owner already has local access to the corpus.

When authentication is enabled, `cmd/kbtoken` is the local bootstrap and admin-recovery tool. Stop the service first; the CLI also checks `/health`, while the shared `auth.json.lock` is the actual cross-process write lock:

```powershell
$env:KB_AUTH_FILE="auth.json"
go run ./cmd/kbtoken create-admin -name platform-admin
# Save the token from this output once, then start cmd/kb.
go run ./cmd/kbtoken list
go run ./cmd/kbtoken rotate-admin -id <old-admin-id>
go run ./cmd/kbtoken revoke-admin -id <old-admin-id>
# Non-admin token with a workload label; -trusted lets it send X-On-Behalf-Of to the gateway:
go run ./cmd/kbtoken create -name kb-service -workload rag -trusted
```

`create` accepts `-workload` (`rag`, `fim`, `agent`, `chat`), `-teams`, `-all-teams`, `-engineering` and `-indexer`. `-trusted` is available only through this CLI; the `/admin/tokens` API never grants it.

`rotate-admin` creates a new principal ID and leaves the old admin alive until it is explicitly revoked. Revoking the last admin requires `-force`; if that is intentional, `create-admin` can bootstrap a new admin from the resulting empty auth file. A stale lock is never removed automatically—stop any writer and remove `auth.json.lock` manually only after confirming no process owns it.


Import a bundle and build the index before starting either one:

```powershell
make import TEAM=Store.POS FROM=<staging>/kb
go run ./cmd/kb
# In another terminal: Invoke-RestMethod -Method Post -Uri http://localhost:12598/index
```

### Shared intranet server (default)

Run `cmd/kb` on the central host after importing and indexing its knowledge base. Its MCP endpoint is `http://192.168.17.139:12598/mcp`; configure OpenCode 1.1.34 or later with:

```jsonc
{
  "mcp": {
    "knowledge_base": {
      "type": "remote",
      "url": "http://192.168.17.139:12598/mcp",
      "headers": {
        "Authorization": "Bearer {env:KB_QA_TOKEN}"
      },
      "oauth": false,
      "enabled": true,
      "timeout": 60000
    }
  }
}
```

Allow TCP port 12598 only from approved company network ranges in Windows Firewall. `/health` is public; `/chat`, `/index`, and `/mcp` require a bearer token when auth is enabled.

This example uses plain HTTP for a trusted-network test. Bearer tokens are credentials: before rollout, put the endpoint behind HTTPS or use a confirmed encrypted trusted channel.

Missing or invalid bearer tokens intentionally return 401 without WWW-Authenticate. This service does not implement OAuth discovery or flow; that is a deliberate deviation from RFC 9110, so do not add resource_metadata to the MCP auth wrapper.

### Local stdio server (development)

`make mcp` runs `cmd/kbmcp`, the stdio transport of the same server. Use it when driving MCP Inspector (below), or for a client that only accepts a `command`. Point the client at this repository:

```json
{
  "mcpServers": {
    "knowledge-base": {
      "command": "go",
      "args": ["run", "./cmd/kbmcp"],
      "cwd": "C:\\path\\to\\llm-platform"
    }
  }
}
```

The same stdio command shape is supported by Codex, Claude Code, and Cline; place it in that client's MCP configuration file. MCP does not rebuild the index or expose the HTTP endpoints.

### Test the server with MCP Inspector

Verify the tool without wiring it into a client. Inspector's CLI mode speaks the real stdio protocol and prints structured output — requires Node.js and a built `.kb` index (import + `POST /index` above):

```powershell
go build -o bin/kbmcp.exe ./cmd/kbmcp

# List the tool and its schema (no LLM call):
npx -y @modelcontextprotocol/inspector --cli ./bin/kbmcp.exe --method tools/list

# Call it — a grounded question:
npx -y @modelcontextprotocol/inspector --cli ./bin/kbmcp.exe --method tools/call `
  --tool-name search_kb --tool-arg query="登入畫面有哪些按鈕?"

# An unrelated question comes back grounded:false:
npx -y @modelcontextprotocol/inspector --cli ./bin/kbmcp.exe --method tools/call `
  --tool-name search_kb --tool-arg query="今天台北天氣如何?"
```

Drop `--cli` to open the browser UI instead: `npx @modelcontextprotocol/inspector ./bin/kbmcp.exe`, then open the printed `http://localhost:6274/?MCP_PROXY_AUTH_TOKEN=...` URL and drive `search_kb` from the Tools tab. `search_kb` needs a reachable chat model (`OPENAI_BASE_URL`) to generate the answer.

## Inference gateway

`cmd/gateway` (`make run-gateway`, default port `12599`) forwards an allow-list of OpenAI-compatible routes to the upstream model server. It uses its own upstream key, so callers never see it:

| Route | Upstream |
| --- | --- |
| `POST /v1/chat/completions`, `POST /v1/completions`, `GET /v1/models` | `GATEWAY_UPSTREAM_BASE_URL` |
| `POST /v1/embeddings` | `GATEWAY_EMBED_UPSTREAM_BASE_URL` if set, otherwise the chat upstream |
| `GET /healthz` | answered by the gateway, no auth (liveness) |
| `GET /readyz` | no auth; `200` only if the chat upstream answers `GET /v1/models` within 2 s (readiness) |

Any other path or method returns 404 and is never forwarded.

- **Auth.** Bearer tokens come from the same `KB_AUTH_FILE` as `cmd/kb`. The gateway checks the file's mtime every 10s and reloads it. If a reload finds the file invalid, the previous snapshot stays in place. A file with no principals is valid and rejects every token.
- **Per-user attribution.** Only a `trusted` token may name the real caller in `X-On-Behalf-Of`. The header is ignored for every other token, and the gateway always strips it before forwarding. Set `KB_LLM_FORWARD_USER=true` so the KB attaches it.
- **Admission.** In-flight requests are capped per effective user (`GATEWAY_MAX_INFLIGHT_PER_USER`) and globally (`GATEWAY_MAX_INFLIGHT`). A request over either cap gets `429` and `Retry-After: 1` right away. Nothing is queued. The counters live in process memory: they reset on restart and are not shared between instances.
- **Embedding model binding.** When `GATEWAY_EMBED_MODEL` is set, a `/v1/embeddings` request with any other model name, no model, or invalid JSON gets `400` and is never forwarded. The gateway checks only the requested name, not which model the backend has loaded.
- **Usage log.** Each authenticated request writes one `gateway_usage` line with `user_id`, `user_kind` (`user`/`test`/`service`, or `unknown` for an `X-On-Behalf-Of` ID not in `auth.json`), `principal_id`, `workload`, `model`, `status`, token counts, latency, `ttft_ms` for streams, and `error`. Embedding lines also carry `input_count` and `input_chars`, because some upstreams return no usage. Prompts and responses are never logged. For streams, the gateway adds `stream_options.include_usage=true` unless the client set it. A client that sends `false` keeps it, and that request logs 0 tokens.
- **Errors.** Client cancel is logged as `499`. An upstream that sends no response headers within `GATEWAY_UPSTREAM_HEADER_TIMEOUT` gets `504`. Other upstream failures get `502` with only an error code in the body. A body over 4MB gets `413`, and an unreadable body gets `400`. The server has no write timeout, so long streams are never cut off.

To route the KB through the gateway, set `OPENAI_BASE_URL=http://localhost:12599/v1` and make `OPENAI_API_KEY` a trusted gateway token (`kbtoken create -trusted -workload rag`). `KB_EMBED_MODEL` must equal `GATEWAY_EMBED_MODEL`. `.env.example` holds this setup.

## Configuration

Environment variables:

- `APP_PORT` - HTTP port, default `12598`.
- `APP_SHUTDOWN_TIMEOUT` - graceful shutdown timeout, default `10s`.
- `KB_AUTH_FILE` - required auth snapshot for `cmd/kb` unless auth is explicitly disabled.
- `KB_AUTH_DISABLED=true` - local-only escape hatch; the server forces `127.0.0.1` and leaves HTTP/MCP routes unguarded.
- `LOG_LEVEL` - zap log level, default `info`.
- `LOG_ENCODING` - zap encoding, default `json`.
- `LOG_OUTPUT` - comma-separated log sinks, default `stdout,log/kb.log`. Missing directories are created. Set `stdout` alone to stop writing files. `cmd/kbmcp` drops any `stdout` sink regardless: its stdout carries the JSON-RPC protocol.
- `OPENAI_API_KEY` - a trusted gateway token, created with `kbtoken create -name <name> -workload rag -trusted`. The gateway resolves it and forwards with its own upstream key, so no provider key belongs here.
- `KB_LLM_MODE` - `openai` or `fake`, default `openai`.
- `OPENAI_BASE_URL` - the gateway, `http://localhost:12599/v1`. Chat and embeddings both go through it. The KB does not connect to a cloud provider directly.
- `KB_CHAT_MODEL` / `KB_EMBED_MODEL` - chat and embedding model names (e.g. `gemma-4-26b-a4b` / `gemini-embedding-2`; defaults `gpt-4o-mini` / `text-embedding-3-small`). `KB_EMBED_MODEL` must equal `GATEWAY_EMBED_MODEL`.
- `KB_CHAT_TEMPERATURE` (default `0`) / `KB_CHAT_MAX_TOKENS` (default `1024`, `0` omits the field) - decoding parameters sent with every chat completion. Without them the upstream falls back to the served model's own `generation_config`, so repeated eval rounds disagree with each other. `GET /health` reports both alongside the chat model and a fingerprint of the grounding prompt, so an eval run can prove which build answered it.
- `KB_DOCS_DIR` / `KB_INDEX_DIR` - source and local index directories.
- `KB_INDEX_TIMEOUT` - `POST /index` timeout, default `60s`. The KB server's write timeout is `max(30s, KB_INDEX_TIMEOUT + 10s)`. Raise it, for example to `10m`, when a full reindex re-embeds the whole corpus.
- `KB_CHAT_TIMEOUT` - `POST /chat` timeout, default `30s`, covering retrieval and the model answer. The write timeout above also stays at least `KB_CHAT_TIMEOUT + 10s`. Raise it for reasoning models: with thinking on, K2 answered 4 of 606 eval questions in over 30s, and a timeout returns 503.
- `KB_LLM_FORWARD_USER` - default `false`. When `true`, the caller's principal ID goes out as `X-On-Behalf-Of`. Enable it only when `OPENAI_BASE_URL` is the internal gateway, never a public provider.

Gateway (`cmd/gateway`; it reads `KB_AUTH_FILE` and the `LOG_*` variables too):

- `GATEWAY_UPSTREAM_BASE_URL` - required chat upstream. A trailing `/v1` is stripped.
- `GATEWAY_UPSTREAM_API_KEY` - key sent upstream. If empty, the caller's `Authorization` header is removed and nothing is sent.
- `GATEWAY_UPSTREAM_HEADER_TIMEOUT` - default `300s`.
- `GATEWAY_EMBED_UPSTREAM_BASE_URL` / `GATEWAY_EMBED_UPSTREAM_API_KEY` - optional dedicated embedding upstream. Give the full base URL including its version path (Google's is `/v1beta/openai`). This requires `GATEWAY_EMBED_MODEL`.
- `GATEWAY_EMBED_MODEL` - the only embedding model name accepted.
- `GATEWAY_MAX_INFLIGHT` (default `64`) / `GATEWAY_MAX_INFLIGHT_PER_USER` (default `4`) - `0` or less means unlimited.
- `GATEWAY_PORT` - default `12599`.
- `GATEWAY_LOG_OUTPUT` - default `stdout,log/gateway.log`.

## Retrieval

Every `/index` run audits section visibility before `Save` or embedding. `procedure`
sections whose heading contains 工程對應 are engineering-only; `ui_inventory`,
`playlist`, `reference` and `distilled` sections are public; other or missing
`doc_type` values fail the index. Endpoint markers under a non-engineering heading also fail closed. The
rule is intentionally asymmetric: an engineering `ViewModels` or `Commands`
section may contain no endpoint and still indexes successfully.

The 2026-08-28 corpus baseline is 3,068 sections with anchor version 2 and
fingerprint
`43de814bfe3044e7194a0beb95f7a7dbb6de88c8530a611953a51f260ac57c58`
(the 2026-08-14 baseline was 679 sections — the corpus has since absorbed three
more sources and 47 distilled action chains).
That fingerprint is a precondition, not a permanent claim: if the current
documents produce a different fingerprint, rerun the classification dry run
before comparing counts.

`corpus_baseline_test.go` pins three things that must move together — the
fingerprint, the section count, and the per-`doc_type` distribution. Changing the
corpus without updating all three leaves the test asserting a corpus that no
longer exists.

If an audit fails, the new content is not saved, embedded, or swapped into
memory. The previous last-known-good snapshot continues serving; an existing
section from that snapshot can still be retrieved until a corrected `/index`
succeeds. An index failure is therefore not an immediate revocation mechanism.
Startup applies the same audit to a persisted snapshot; if it fails, the
snapshot is rejected and the service remains unready until `/index` rebuilds it.

Two-way recall, fused, split into two pools, then truncated. There is no reranking stage.

```text
query ─┬─ BM25          → top candidateK (20)
       └─ vector cosine → top candidateK (20)
                ↓
           FuseRRF (rrfK 60)
                ↓
   PartitionAndRankPools  ── L0 pool (source sections) ─┐
   (split BEFORE truncation)  L1 pool (distilled, l1K 2) ┤
                ↓                                        │
   ExpandDistilled: each L1 hit expands deterministically │
   back to the L0 step sections it indexes ───────────────┘
                ↓
       topSections(..., topK 8)  → sent to the model
```

**L1 is an index, never evidence.** A `distilled` section can be retrieved but never
reaches the model: on a hit it expands to the L0 step sections it points at, and every
citation and screenshot comes from L0. That is deliberate — `distilled` is not in the
grounding prompt's evidence allow-list, and folding a `recorded_unlabeled` step's
narrative into a new `doc_type` would launder past the guard that such a step proves
only that a click occurred.

**The split happens before candidate truncation, not after.** BM25 penalises length
(`denom = f + k1*(1 - b + b*DocLen/AvgLen)`), and a distilled chain runs several times
longer than one step section — roughly a 4.6x disadvantage in a shared pool. Partitioning
after truncation would mean the L1 entries were already gone. When no L1 candidate
qualifies, its reserved slots return to L0, so questions that are not about action
sequences behave exactly as they did before distillation existed.

The knobs are constants in `internal/kb/service.go`, not environment variables — changing
one means a rebuild **and a service restart**, and a stale binary silently reports the old
behaviour as if the change had no effect.

RRF fuses by *rank*, never by content. Two sections of the same document at ranks 3 and 4
are indistinguishable to it, so the only lever for "the answering section ranked 6th" is a
larger `topK`. Measure before turning it:

```powershell
$env:KB_RETRIEVAL_PROBE_EVAL_OUT="eval_out.json"
go test ./internal/kb/ -tags retrievalprobe -run TestRetrievalProbe -v
```

The probe reports, per question, the rank at which a retrieved section actually contains
the answer term — distinct from the older file-level metric, which only says the expected
*document* ranked and once led to a wrong "the model is refusing" diagnosis.

### When a reranker becomes worth adding

`topK` trades context for recall, and both sides have a cost. Measured 2026-08-14 on 246
questions over the then 266-section WPF POS corpus — kept because the shape of the
trade-off still holds, but the corpus is now 3,068 sections, so re-measure before acting
on the absolute numbers:

| topK | outcome |
|------|---------|
| 3 | answering section in the top 3 for only 3 of 22 refusals; A1 5/9, A2 145/173 |
| 8 | covers 20 of 22; A1 9/9, A2 166–169/173 — but ~2.7x the context per query |
| 10 | covers 22 of 22; diminishing returns, more dilution |

Raising it is not free: with `topK=8` one question began refusing *because* of the extra
context — the model saw fragments from several screens and concluded the list was
incomplete. That was fixed in the data (the module overview now enumerates its screens in
one self-contained sentence), not by tuning `topK`.

A reranker resolves that trade-off: recall wide (20), rerank by query-passage relevance,
send only the best 3. Consider it when any of these hold:

- **Context cost or latency starts to bite.** `topK=8` is roughly 2.7x the tokens of 3, per
  request, forever.
- **`topK` has to keep growing.** More modules and screens mean more sections competing;
  if the section-level probe starts needing 12–15, that is a ranking problem, not a
  truncation one.
- **Dilution regressions appear.** More than an isolated case of "the model refuses when
  given more context" means the ranking, not the budget, is what needs fixing.

It is not the answer when failures are a question/data-model mismatch. The example that
prompted this note was an A3 failure that retrieved the correct document, quoted its
buttons, then refused because the question asked for a *per-module* control list while
canonical `ui_inventory` documents are deduplicated across modules by design. No amount of
reranking changes that. (That particular case no longer reproduces — A3 is 159/159 as of
2026-08-28 — but the reasoning is why a reranker is not a general answer.)

Cost of adding one: another model call per query (latency, spend, one more key to manage),
and `minThreshold` / `cosineMin` need re-measuring against the new ordering. The
section-level probe above is the instrument to judge whether it paid off — have it in place
before, not after.

## Import a team bundle

```powershell
make distill TEAM=Store.POS BUNDLE=<staging>/kb   # L1 action chains, before -check
make import  TEAM=Store.POS FROM=<staging>/kb
Invoke-RestMethod -Method Post -Uri http://localhost:12598/index
```

> **`FROM` must be a merged tree containing every source for that team.** `kbimport`
> replaces `docs/<team>/` and `eval/<team>/` wholesale, so pointing it at a single
> source deletes the others — and `-check` still passes, because it validates that the
> tree is *self-consistent*, not that it is *complete*. This has already cost one corpus:
> a 176-document team was cut to 22, taking its eval suite with it. Neither directory is
> in version control, so there is nothing to restore from.
>
> Which sources make up a team is recorded in `kb_sources.json` (gitignored — it holds
> internal absolute paths; copy `kb_sources.json.example` and fill it in). Merging,
> distilling, checking and importing are driven end to end by the `ui-kb-merge-import`
> skill; do not run `make import` from anywhere else.

`make distill` runs before `kbimport -check`, not after: `validateEvalIndex` requires
`kb_index.json` to cover exactly the staged file set, so distilled output arriving later
fails the check. It uses no LLM — it concatenates each module's Gherkin steps in order,
preserving every source anchor and evidence class.

Imported files live below `docs/<team>/`; screenshots are copied below that team's `_assets/` directory. `images` in `/chat` are paths relative to `KB_DOCS_DIR`, so they only resolve on a machine that has run the import — they are identifiers, not URLs a client can fetch.

The import is transactional: it validates the staged bundle against every team already present and only then swaps the team's directory into place, so a rejected bundle leaves `docs/` untouched. Re-running it for the same team replaces that team's tree and nothing else.

All imported documents currently share one access level and one index. If a team needs a separate trust boundary, run a separate instance with its own `KB_DOCS_DIR` and `KB_INDEX_DIR`.

## Layout

```text
cmd/kb/                  # HTTP server entrypoint (/health, /index, /chat)
cmd/gateway/             # inference gateway in front of the model upstream
cmd/kbimport/            # transactional team-bundle importer (make import)
cmd/kbdistill/           # L1 action-chain distiller, zero LLM (make distill)
cmd/kbtoken/             # local auth bootstrap, admin rotation, and revoke CLI
cmd/kbmcp/               # stdio MCP server exposing the search_kb tool
internal/kb/             # domain, service, ports, handlers, markdown/vector repos, LLM adapters
internal/auth/           # token resolution, CRUD, atomic auth persistence, HTTP adapter
internal/gateway/        # gateway auth, admission limiter, reverse proxy, usage log
internal/shared/         # zero-dependency value objects (Principal)
internal/mcpserver/      # MCP adapter over the kb service (parallel to the HTTP handler)
internal/bootstrap/      # composition root — wires the kb service for both entrypoints
internal/platform/config # env/.env config loading
internal/platform/httpserver
internal/platform/atomicfile
internal/platform/lockfile
internal/platform/logger
kb_sources.json.example  # template for the (gitignored) per-team source manifest
thoughts/qrspi/          # QRSPI artifacts
```

Not in version control — recreate with `make import` and `POST /index`:

```text
docs/<team>/             # imported Markdown and _assets/ screenshots
eval/<team>/             # bundle eval YAML and kb_index.json
.kb/index.json
.kb/faiss_index/vectors.bin
```

Also gitignored, because they derive from a customer corpus rather than from this
repository — keep local copies, they are not recoverable from a clone:

```text
testdata/                # requirement and retrieval-probe suites (business rules verbatim)
internal/kb/testdata/    # corpus fixtures
metrics/                 # eval runs — these store model answers and cited passages
postman/                 # acceptance queries
kb_sources.json          # which sources compose each team; internal absolute paths
```

## Make Targets

```powershell
make run            # run ./cmd/kb; needs $env:KB_AUTH_FILE or KB_AUTH_DISABLED=true
make run-gateway    # run ./cmd/gateway; needs GATEWAY_UPSTREAM_BASE_URL and KB_AUTH_FILE
make mcp            # run the stdio MCP server (dev / Inspector only)
make build          # build bin/kb.exe, bin/kbmcp.exe, bin/kbtoken.exe, and bin/gateway.exe
make prompt-check   # compare the grounding-prompt fingerprint of source, built binaries, and running service
make import         # import a team bundle (FROM must be a merged tree — see above)
make distill        # generate L1 action chains into a staging bundle, before -check
make test           # go test -race -short -count=1 ./...
make test-cover     # same, plus coverage.out and coverage.html
make lint           # golangci-lint run ./...
make fmt            # gofmt -s -w .
make vet            # go vet ./...
make tidy           # go mod tidy
make verify         # lint + test
make hooks-install  # enable .githooks/pre-commit (gofmt, lint, unit tests)
make clean          # remove bin/ and coverage output (PowerShell/cmd only)

go run ./cmd/kbtoken list   # list auth metadata while cmd/kb is stopped
```

`make build` writes the `.exe` suffix explicitly, because `go build -o` uses the
name verbatim rather than adding it. The Inspector command above and
`run_eng_eval.py` both invoke `bin/kbmcp.exe`, so the suffix is not optional.

Run `make hooks-install` once per clone. The hook runs `gofmt -l`, `golangci-lint`
and the unit tests before each commit; `git commit --no-verify` skips it.
