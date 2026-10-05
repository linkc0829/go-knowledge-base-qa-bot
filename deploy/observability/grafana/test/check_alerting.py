"""CI check: starts the pinned Grafana from ../../compose.yaml with the production
alerting provisioning and asserts the rules and contact points are loaded.
Stdlib only; needs Docker.

    python deploy/observability/grafana/test/check_alerting.py
"""
import base64
import json
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

OBS = pathlib.Path(__file__).resolve().parents[2]
NAME = "grafana-alerting-ci"
PORT = 13001
ADMIN_USER = "admin"
ADMIN_PASS = "adminpassword"


def get(path: str) -> tuple[int, str]:
    url = f"http://127.0.0.1:{PORT}{path}"
    auth = "Basic " + base64.b64encode(f"{ADMIN_USER}:{ADMIN_PASS}".encode()).decode()
    req = urllib.request.Request(url, headers={"Authorization": auth})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, r.read().decode("utf-8")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8")
    except OSError as e:
        return 0, str(e)


def main() -> int:
    image = re.search(r"image:\s*(grafana/grafana:\S+)", (OBS / "compose.yaml").read_text(encoding="utf-8")).group(1)
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME, "-p", f"127.0.0.1:{PORT}:3000",
        "-v", f"{OBS / 'grafana/provisioning'}:/etc/grafana/provisioning:ro",
        "-e", f"GF_SECURITY_ADMIN_USER={ADMIN_USER}",
        "-e", f"GF_SECURITY_ADMIN_PASSWORD={ADMIN_PASS}",
        "-e", "GF_AUTH_ANONYMOUS_ENABLED=false",
        "-e", "ALERT_EMAIL_TO=test@example.com",
        "-e", "GF_SMTP_ENABLED=true",
        "-e", "GF_SMTP_HOST=localhost:25",
        "-e", "GF_SMTP_FROM_ADDRESS=alert@example.com",
        "-e", "GF_SERVER_ROOT_URL=http://localhost:3000",
        image,
    ], check=True, capture_output=True)

    try:
        deadline = time.time() + 60
        while True:
            code, _ = get("/api/health")
            if code == 200:
                break
            if time.time() > deadline:
                print(subprocess.run(["docker", "logs", NAME], capture_output=True, text=True).stderr)
                sys.exit("grafana never became ready")
            time.sleep(2)

        code, body = get("/api/v1/provisioning/alert-rules")
        if code != 200:
            sys.exit(f"GET /api/v1/provisioning/alert-rules failed with {code}: {body}")
        rules = json.loads(body)
        titles = {r.get("title") for r in rules}
        expected = {"Gateway not ready", "KB not ready", "Ready probe missing", "Gateway 5xx rate", "Disk usage"}
        missing = expected - titles
        if missing:
            sys.exit(f"missing alert rules: {missing}, got: {titles}")

        code, body = get("/api/v1/provisioning/contact-points")
        if code != 200:
            sys.exit(f"GET /api/v1/provisioning/contact-points failed with {code}: {body}")
        contact_points = json.loads(body)
        cp_names = {cp.get("name") for cp in contact_points}
        if "ops-email" not in cp_names:
            sys.exit(f"missing contact point 'ops-email', got: {cp_names}")

    finally:
        subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)

    print(f"alerting provisioning ok ({len(rules)} rules, ops-email verified) ({image})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
