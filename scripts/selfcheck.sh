#!/usr/bin/env bash
# End-to-end check against a running gateway.
#
# Usage:
#   TRANSLATOR_KEY='<raw bearer key>' scripts/selfcheck.sh https://host:10000
#
# The key is read from the environment so it does not appear in `ps`. The
# optional SAMPLE_TEXT environment variable overrides the Hindi test sentence.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: TRANSLATOR_KEY='<raw key>' scripts/selfcheck.sh <base-url>

  <base-url>  e.g. https://server.tailda589d.ts.net:10000
              (no trailing slash required)

The raw API key is read only from TRANSLATOR_KEY.
EOF
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi
if [[ -z ${TRANSLATOR_KEY:-} ]]; then
  usage
  echo "ERROR: TRANSLATOR_KEY is not set." >&2
  exit 2
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: curl is required." >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required for JSON parsing." >&2
  exit 2
fi

BASE_URL="${1%/}"
SAMPLE_TEXT="${SAMPLE_TEXT:-नमस्ते, यह सेवा कैसी है?}"
TMPDIR_CHECK="$(mktemp -d "${TMPDIR:-/tmp}/translator-selfcheck.XXXXXX")"
trap 'rm -rf "$TMPDIR_CHECK"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# request METHOD PATH AUTH MAX_TIME -> prints HTTP status, leaves body in $BODY_FILE
request() {
  local method="$1" path="$2" auth="$3" max_time="$4"
  local args=(-sS -o "$BODY_FILE" -w '%{http_code}' --max-time "$max_time")
  if [[ "$auth" == "yes" ]]; then
    args+=(-H "Authorization: Bearer $TRANSLATOR_KEY")
  fi
  if [[ "$method" == "POST" ]]; then
    args+=(-X POST -H 'Content-Type: application/json' --data-binary @"$REQUEST_FILE")
  fi
  curl "${args[@]}" "$BASE_URL$path"
}

BODY_FILE="$TMPDIR_CHECK/body.json"
REQUEST_FILE="$TMPDIR_CHECK/request.json"

echo "== healthz (no authentication) =="
status=$(request GET /healthz no 10)
[[ "$status" == "200" ]] || fail "/healthz returned $status: $(cat "$BODY_FILE")"
grep -q '"ok":true' "$BODY_FILE" || fail "/healthz body: $(cat "$BODY_FILE")"
echo "ok: 200"

echo "== unauthenticated /status =="
status=$(request GET /status no 10)
[[ "$status" == "401" ]] || fail "unauthenticated /status returned $status: $(cat "$BODY_FILE")"
[[ "$(cat "$BODY_FILE")" == '{"error":"unauthorized"}' ]] || fail "unauthorized body is not the fixed 401 body"
echo "ok: 401 with the standard body"

echo "== authenticated /status =="
status=$(request GET /status yes 10)
[[ "$status" == "200" ]] || fail "authenticated /status returned $status: $(cat "$BODY_FILE")"
python3 - "$BODY_FILE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
if data.get("state") not in {"stopped", "starting", "ready", "stopping"}:
    raise SystemExit(f"unexpected state: {data!r}")
print(f"ok: state={data['state']} idle_stop_in={data.get('seconds_until_idle_shutdown')}s")
PY

echo "== translation (may cold-start the model) =="
python3 - "$REQUEST_FILE" "$SAMPLE_TEXT" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as f:
    json.dump({"text": sys.argv[2]}, f, ensure_ascii=False)
PY
started=$(date +%s)
status=$(request POST /translate yes 180)
elapsed=$(( $(date +%s) - started ))
[[ "$status" == "200" ]] || fail "/translate returned $status after ${elapsed}s: $(cat "$BODY_FILE")"
python3 - "$BODY_FILE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
translation = data.get("translation")
if not isinstance(translation, str) or not translation.strip():
    raise SystemExit(f"missing translation: {data!r}")
print(f"ok: {len(translation)} Malayalam characters returned")
PY
echo "ok: 200 in ${elapsed}s"

echo "== /status after translation =="
status=$(request GET /status yes 10)
[[ "$status" == "200" ]] || fail "post-translation /status returned $status"
python3 - "$BODY_FILE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
if data.get("state") != "ready":
    raise SystemExit(f"expected ready after translation, got {data!r}")
print(f"ok: ready, idle shutdown in {data.get('seconds_until_idle_shutdown')}s")
PY

echo
echo "SELFCHECK PASSED: $BASE_URL"
