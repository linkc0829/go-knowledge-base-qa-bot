"""CI check for the Alloy allowlist: runs the pinned Alloy image from
../../compose.yaml on testdata/input.jsonl with the production allowlist module
and asserts on what loki.echo prints. Stdlib only; needs Docker.

    python deploy/observability/alloy/test/run.py
"""
import pathlib
import re
import subprocess
import sys
import time

HERE = pathlib.Path(__file__).resolve().parent
ALLOY = HERE.parent
COMPOSE = ALLOY.parent / "compose.yaml"
NAME = "alloy-allowlist-ci"

# Must reach Loki: proves the pipeline ran and kept the allowed fields.
MUST = [
    "p_3q2-7wAbCdEfGhIjKlMnOw",  # a real generator-shaped ID passes unchanged
    "user_kind", "4321", "8765",  # gateway_usage fields
    "strategy", "hybrid", "refuse",  # kb_query fields
    "777", "333",  # llm_usage prompt/completion tokens
    "42.5",  # host_disk
]
# Must never reach Loki.
NEVER = [
    "SECRET-Q", "引號", "薪資", "反斜線", "hr/salary",  # question text, sources
    "LEAK-NAME-FIELD",  # a field not on the allowlist
    "AliceFromMarketing", "Alice Chen",  # names used as IDs
    "UNKNOWN-MARKER", "MALFORMED-MARKER", "MALFORMED-TEXT-MARKER", "HALF-LINE-MARKER",
]


def image() -> str:
    m = re.search(r"image:\s*(grafana/alloy:\S+)", COMPOSE.read_text(encoding="utf-8"))
    if not m:
        sys.exit(f"no grafana/alloy image in {COMPOSE}")
    return m.group(1)


def logs() -> str:
    r = subprocess.run(["docker", "logs", NAME], capture_output=True, text=True, encoding="utf-8")
    return r.stdout + r.stderr


def main() -> int:
    img = image()
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)
    subprocess.run([
        "docker", "run", "-d", "--name", NAME,
        "-v", f"{ALLOY / 'lib'}:/etc/alloy/lib:ro",
        "-v", f"{HERE / 'config.alloy'}:/etc/alloy/config.alloy:ro",
        "-v", f"{ALLOY / 'testdata'}:/testdata:ro",
        img, "run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy",
    ], check=True, capture_output=True)
    try:
        deadline = time.time() + 90
        while time.time() < deadline and not all(m in logs() for m in MUST):
            time.sleep(2)
        time.sleep(5)  # let any line that should not be there show up too
        out = logs()
    finally:
        subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)

    failures = [f"missing {m!r}" for m in MUST if m not in out]
    failures += [f"leaked {m!r}" for m in NEVER if m in out]
    # A "q" key, raw or backslash-escaped inside loki.echo's quoted entry.
    if re.search(r'\\?"q\\?"\s*:', out):
        failures.append('leaked a "q" field')
    if out.count("nonstandard_id") < 3:
        failures.append(f"nonstandard_id x{out.count('nonstandard_id')}, want >= 3 (2 user_id + 1 owner_id)")
    if failures:
        print(out)
        print("FAIL (%s):" % img, *failures, sep="\n  ")
        return 1
    print(f"allowlist ok ({img})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
