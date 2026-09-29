# Deploying on the AI Station

Everything runs on the POC host: Ubuntu 24.04, RTX 6000 Ada 48GB, `192.168.22.101`. It is split into three Docker Compose projects on one external network, `ai-net`:

| Project | Services | Talks to |
| --- | --- | --- |
| [`inference/`](inference/compose.yaml) | vLLM | — (port 8000 on loopback only) |
| [`platform/`](platform/compose.yaml) | gateway `:12599`, kb `:12598` (built from this repo) | `vllm:8000` over `ai-net` |
| [`observability/`](observability/compose.yaml) | Loki, Alloy, Grafana `:3000` | reads the platform's log files |

Each project can be restarted without touching the others. For example, a gateway redeploy leaves vLLM running and avoids its ~2-minute warm restart.

Ansible (`deploy/ansible/`, next PR) sets up the host and runs these steps. By hand:

```bash
docker network create ai-net
for p in inference platform observability; do
  (cd deploy/$p && cp .env.example .env && chmod 600 .env)   # then fill each .env
done
(cd deploy/inference && docker compose up -d)
(cd deploy/platform && docker compose up -d --build)
(cd deploy/observability && docker compose up -d)
```

Start them in that order. `depends_on` only works inside one project, so nothing waits across projects. The gateway starts even if vLLM is down, and `/readyz` returns 503 until vLLM is up.

The POC's old `/srv/ai/vllm` project must be stopped first (`docker compose down` there), because both bind port 8000.

## Host directories

| Path | Owner / mode | Used by |
| --- | --- | --- |
| `/srv/ai/platform/data/auth/` | `65532:65532` 0700; `auth.json` 0600 | gateway (ro), kb (rw; the admin API writes tokens) |
| `/srv/ai/platform/data/docs/` | `65532` 0750 | kb (ro) |
| `/srv/ai/platform/data/.kb/` | `65532` 0750 | kb (rw) |
| `/srv/ai/platform/data/log/` | `65532:ailogs` 2750; files 0640 | gateway and kb write; Alloy (UID 473 + group `ailogs`) reads |
| `/srv/ai/observability/data/loki/` | `10001` 0750 | Loki |
| `/srv/ai/observability/data/alloy/` | `473` 0750 | Alloy read positions |
| `/srv/ai/observability/data/grafana/` | `472:0` 0750 | Grafana (users, sessions) |
| `/srv/ai/hf-cache`, `/srv/ai/vllm-cache` | root | vLLM (runs as root in its image) |

- **`auth.json` is mounted as a directory.** kb replaces the file by renaming a new one over it, and a rename fails on a single-file bind mount. The gateway notices the change and re-reads it.
- **Log file permissions.** zap creates log files using the process umask, and distroless has no shell to change it. So Ansible pre-creates `gateway.log`, `kb.log` and `host.log` as 0640. The setgid bit on the directory makes any new file inherit the `ailogs` group.

## What goes where: logs and privacy

| Data | `kb.log` on host | Docker logs | Loki / Grafana |
| --- | --- | --- | --- |
| Question text (`q`), sources | yes | **no** | **no** |
| Names | no (never logged) | no | no |
| `user_id` / `owner_id` | yes | no | yes, if the ID looks generated; otherwise `nonstandard_id` |
| Tokens, latency, status, `user_kind` | yes | no | yes |

- **gateway and kb log to files only.** They are not configured to write to stdout, because the Docker log would repeat what is in `kb.log`, `q` included.
- **Every container** rotates its Docker log at 10 MB, keeping 3 files.
- **Alloy uses an allowlist** ([`alloy/lib/allowlist.alloy`](observability/alloy/lib/allowlist.alloy)):
  - Each known `msg` is rebuilt from its listed fields only, and that rebuilt line replaces the original.
  - Any other `msg`, and any line that isn't JSON, is dropped.
  - A new field added to the code reaches Loki only after it is added to this list.
- **The ID guard.** `user_id` and `owner_id` pass through only if they match `^p_[A-Za-z0-9_-]{21}[AQgw]$`, the exact shape `GeneratePrincipalID` produces. Anything else becomes `nonstandard_id`.
  - This catches misuse, such as a hand-edited `auth.json` ID or a free-form `X-On-Behalf-Of` value. It cannot prove an ID is random.
- **CI** (`observability` job) runs the pinned Alloy image on [fixtures](observability/alloy/testdata/input.jsonl), which include:
  - question text with escapes and Chinese;
  - names used as IDs;
  - an unknown `msg`;
  - malformed and half-written lines;
  - a field that isn't on the allowlist.

  It asserts that none of these leak. It also runs every dashboard query against the pinned Loki.

**What a Grafana Viewer can see:** usage per anonymous `user_id`. They cannot see question text or names.
- Grafana OSS cannot stop a Viewer from querying Loki directly, so the only reliable protection is keeping these out of Loki altogether.
- Anyone who knows which ID belongs to which person can still identify people. That includes admins and anyone who can read `auth.json`.
- To map IDs to names, an admin runs `usagecost` on the host.

**Backups of the data directories are sensitive.** They hold `auth.json` (token hashes) and `kb.log` (question text). Only the maintainers of this host may read them.

## Grafana

- Anonymous access and self sign-up are off. Only the admin creates accounts: **Viewer** for colleagues, **Admin** for maintainers.
- **Cleartext risk:** Grafana (`:3000`), the gateway (`:12599`) and kb (`:12598`) use plain HTTP on the LAN. Passwords and bearer tokens travel unencrypted. TLS or a VPN comes in Phase 6.
  - Until then, UFW limits which source networks can connect.
  - Viewer passwords must not be reused anywhere else.
- **Dashboard: LLM Platform.** Panels:
  - weekly and daily active users
  - chat requests per day by status
  - tokens per day
  - TTFT and latency p95
  - 5xx rate
  - KB refusal rate
  - disk used, with a red line at 85%
  - usage per `user_id`

  The **User kind** filter defaults to `user`, so eval, load-test and service traffic stay out of the people counts.
- **Loki keeps 180 days of logs.** A single query can span about 30 days at most; that is a query limit, not retention.
  - Filesystem storage is fine for this POC, but not supported for production. **The production (96GB) machine needs its own Loki storage decision.**

## Health checks and probes

| Service | Container healthcheck | Interval / grace |
| --- | --- | --- |
| vLLM | `/health` (python3) | 30s / 20m (cold start downloads and compiles) |
| gateway | `/healthz` via `/app/healthcheck` | 10s / 10s |
| kb | `/ready` via `/app/healthcheck` | 15s / 60s |
| Alloy | `/-/ready` via bash `/dev/tcp` (no curl in the image) | 15s / 30s |
| Grafana | `/api/health` via curl | 15s / 30s |
| Loki | none: distroless image, no shell. Check it with `docker compose exec grafana curl -fsS http://loki:3100/ready` | — |

Each healthcheck tests only its own container. The whole request chain is tested by the gateway's readiness probe.

| Endpoint | Kind | 200 means | 503 means |
| --- | --- | --- | --- |
| kb `GET /health` | liveness | process up (always 200) | — |
| kb `GET /ready` | readiness | index loaded, and vectors are `ok` (or deliberately `disabled`) | `not_indexed`, or `stale`: retrieval has fallen back to BM25 |
| gateway `GET /healthz` | liveness | process up | — |
| gateway `GET /readyz` | readiness | vLLM answered `GET /v1/models` within 2 s | vLLM is down, still loading, or hung (logged as `gateway_not_ready`) |

- **B2 (MTTR):** stop `inference` and time how long `/readyz` takes to return 200 again.
- Docker restarts a container only when it exits (`restart: unless-stopped`). It does not restart one that is `unhealthy`.

## Acceptance checklist (on the machine)

1. Every `auth.json` ID has the generated shape. This prints counts only, not the IDs:
   ```bash
   sudo jq '[.principals[].id | test("^p_[A-Za-z0-9_-]{21}[AQgw]$")] | {total: length, bad: map(select(. | not)) | length}' /srv/ai/platform/data/auth/auth.json
   ```
   If `bad` is not 0, re-issue those tokens with `kbtoken` before going live.
2. `docker ps`: vLLM, gateway, kb, Alloy and Grafana are `healthy`. Loki is `running`, and its `/ready` answers when checked with curl as above.
3. Permissions:
   - `gateway.log` and `kb.log` are 0640, group `ailogs`;
   - kb can write to `.kb`;
   - Alloy's log shows no permission errors;
   - Grafana and Loki write their data directories.
4. The request chain:
   - `curl http://127.0.0.1:12599/readyz` returns 200.
   - Make one chat call through the KB, then check that the dashboard shows it.
   - In Loki, `{service="kb"} |= "\"q\""` returns nothing, and neither does a search for `user_name`.
   - `docker logs platform-kb-1` contains no `kb_query`.
   - Log in as a Viewer and query Loki directly in Explore: no names and no question text.
5. B2 dry run:
   - stop `inference`: gateway `/healthz` returns 200, `/readyz` returns 503, and kb stays healthy;
   - start it again: `/readyz` returns to 200.
6. The disk panel has data. The `disk_probe` timer comes with the Ansible PR.
7. `docker stats`: record vLLM's RAM use.

Not yet verified on the machine: everything above. The CI jobs build the image and test the Alloy allowlist and the dashboard queries against the pinned images, but nothing here has run on the AI Station yet.
