"""CI check: starts the pinned Loki from ../../compose.yaml with the production
config and runs every dashboard query against it. A query Loki cannot parse
returns 400 and fails this check. Stdlib only; needs Docker.

    python deploy/observability/grafana/test/check_queries.py
"""
import json
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

OBS = pathlib.Path(__file__).resolve().parents[2]
NAME = "loki-queries-ci"
PORT = 13100
# Grafana fills these in before the query reaches Loki.
VARS = {"$kind": "user", "$__auto": "5m", "$__range": "1d", "$__interval": "1d"}


def get(path: str, **params) -> tuple[int, str]:
    url = f"http://127.0.0.1:{PORT}{path}?{urllib.parse.urlencode(params)}"
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except OSError as e:
        return 0, str(e)


def main() -> int:
    image = re.search(r"image:\s*(grafana/loki:\S+)", (OBS / "compose.yaml").read_text()).group(1)
    dash = json.loads((OBS / "grafana/dashboards/llm-platform.json").read_text(encoding="utf-8"))
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME, "-p", f"127.0.0.1:{PORT}:3100",
        "--tmpfs", "/loki:rw,uid=10001,gid=10001",
        "-v", f"{OBS / 'loki/config.yaml'}:/etc/loki/config.yaml:ro",
        image, "-config.file=/etc/loki/config.yaml",
    ], check=True, capture_output=True)
    try:
        deadline = time.time() + 120
        while get("/ready")[0] != 200:
            if time.time() > deadline:
                print(subprocess.run(["docker", "logs", NAME], capture_output=True, text=True).stderr)
                sys.exit("loki never became ready")
            time.sleep(2)

        now = int(time.time())
        failures, n = [], 0
        for panel in dash["panels"]:
            for t in panel["targets"]:
                expr = t["expr"]
                for k, v in VARS.items():
                    expr = expr.replace(k, v)
                if t.get("queryType") == "instant":
                    code, body = get("/loki/api/v1/query", query=expr, time=now)
                else:
                    code, body = get("/loki/api/v1/query_range", query=expr,
                                     start=now - 7 * 86400, end=now, step="1h")
                n += 1
                if code != 200:
                    failures.append(f"{panel['title']} [{t['refId']}] -> {code}: {body.strip()[:300]}")
    finally:
        subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)

    if failures:
        print(f"FAIL ({image}):", *failures, sep="\n  ")
        return 1
    print(f"{n} dashboard queries ok ({image})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
