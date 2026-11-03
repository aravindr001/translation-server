#!/usr/bin/env bash
# Install the gateway, worker, venv, units, and first key on an Ubuntu host.
# Run from a checkout that contains a built ./bin/gateway, or with Go installed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_DIR="${APP_DIR:-/opt/translator}"
VENV="$APP_DIR/venv"
ETC_DIR="${ETC_DIR:-/etc/translator}"
STATE_DIR="${STATE_DIR:-/var/lib/translator}"
MODEL_CACHE="${MODEL_CACHE:-$STATE_DIR/hf}"
MODEL_REVISION="${TRANSLATOR_MODEL_REVISION:-ffb7582b6d43791f1fb26b2153fc065f2e9ea575}"
RUN_USER="${RUN_USER:-translator}"
RUN_GROUP="${RUN_GROUP:-translator}"
KEY_FILE="$ETC_DIR/keys.json"

DO_APT=1
DO_SWAP=0
DO_START=0
DO_MODEL=1
HF_TOKEN_FILE=""

usage() {
  cat >&2 <<'EOF'
usage: sudo scripts/install.sh [options]

Options:
  --no-apt              do not install apt packages
  --swap                create a 2G swap file (calls setup_swap.sh)
  --start               enable and start translator.service at the end
  --skip-model          do not download the model even if an HF token is present
  --hf-token-file PATH  read a Hugging Face token from a root-only file
  --help                show this help

Environment:
  HF_TOKEN              Hugging Face token (alternative to --hf-token-file)
  APP_DIR, ETC_DIR, STATE_DIR, MODEL_CACHE, RUN_USER, RUN_GROUP

The model is gated. The Hugging Face account must have accepted the license at
https://huggingface.co/ai4bharat/indictrans2-indic-indic-dist-320M first.
The token is never written to a file by this script.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-apt) DO_APT=0 ;;
    --swap) DO_SWAP=1 ;;
    --start) DO_START=1 ;;
    --skip-model) DO_MODEL=0 ;;
    --hf-token-file)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      HF_TOKEN_FILE="$2"; shift ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
  shift
done

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "ERROR: run this script as root (sudo)." >&2
  exit 1
fi

if [[ -n "$HF_TOKEN_FILE" ]]; then
  [[ -r "$HF_TOKEN_FILE" ]] || { echo "ERROR: cannot read $HF_TOKEN_FILE" >&2; exit 1; }
  HF_TOKEN="$(tr -d '\r\n' < "$HF_TOKEN_FILE")"
fi

if [[ "$DO_APT" -eq 1 ]]; then
  echo "== installing apt packages =="
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates curl python3 python3-pip python3-venv
fi

if [[ "$DO_SWAP" -eq 1 ]]; then
  echo "== setting up swap =="
  "$ROOT/scripts/setup_swap.sh"
fi

echo "== creating service account and directories =="
if ! getent group "$RUN_GROUP" >/dev/null; then
  groupadd --system "$RUN_GROUP"
fi
if ! id -u "$RUN_USER" >/dev/null 2>&1; then
  useradd --system --gid "$RUN_GROUP" --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$RUN_USER"
fi
install -d -m 0755 "$APP_DIR" "$APP_DIR/bin" "$APP_DIR/worker" "$APP_DIR/scripts"
install -d -o root -g "$RUN_GROUP" -m 0750 "$ETC_DIR"
install -d -o "$RUN_USER" -g "$RUN_GROUP" -m 0700 "$STATE_DIR" "$MODEL_CACHE"

echo "== installing gateway binary =="
TMP_BIN="$(mktemp)"
trap 'rm -f "$TMP_BIN"' EXIT
if [[ -x "$ROOT/bin/gateway" ]]; then
  cp "$ROOT/bin/gateway" "$TMP_BIN"
elif command -v go >/dev/null 2>&1; then
  ( cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$TMP_BIN" ./cmd/gateway )
else
  echo "ERROR: no ./bin/gateway and no Go toolchain found." >&2
  echo "Build on another machine with: CGO_ENABLED=0 go build -o bin/gateway ./cmd/gateway" >&2
  echo "then copy bin/gateway here and re-run." >&2
  exit 1
fi
install -m 0755 "$TMP_BIN" "$APP_DIR/bin/gateway"

echo "== installing worker files =="
install -m 0644 "$ROOT/worker/worker.py" "$APP_DIR/worker/worker.py"
install -m 0644 "$ROOT/worker/textsplit.py" "$APP_DIR/worker/textsplit.py"
install -m 0644 "$ROOT/worker/requirements.txt" "$APP_DIR/worker/requirements.txt"
install -m 0644 "$ROOT/README.md" "$APP_DIR/README.md"
install -m 0755 "$ROOT/scripts/download_model.py" "$APP_DIR/scripts/download_model.py"
install -m 0755 "$ROOT/scripts/selfcheck.sh" "$APP_DIR/scripts/selfcheck.sh"

echo "== installing Python venv (this downloads torch CPU) =="
if [[ ! -x "$VENV/bin/python" ]]; then
  python3 -m venv "$VENV"
fi
"$VENV/bin/pip" install --no-cache-dir --upgrade pip
"$VENV/bin/pip" install --no-cache-dir -r "$APP_DIR/worker/requirements.txt"

echo "== installing configuration =="
if [[ ! -f "$ETC_DIR/translator.env" ]]; then
  install -m 0640 -o root -g "$RUN_GROUP" "$ROOT/deploy/translator.env.example" "$ETC_DIR/translator.env"
  echo "Installed $ETC_DIR/translator.env from the example."
else
  echo "Keeping existing $ETC_DIR/translator.env."
fi

if [[ ! -s "$KEY_FILE" ]]; then
  echo "== generating the first API key =="
  key_output="$("$APP_DIR/bin/gateway" genkey default)"
  raw_key="$(printf '%s\n' "$key_output" | sed -n 's/^raw_key=//p')"
  entry_json="$(printf '%s\n' "$key_output" | sed -n '2p')"
  if [[ -z "$raw_key" || -z "$entry_json" ]]; then
    echo "ERROR: gateway genkey produced unexpected output." >&2
    exit 1
  fi
  printf '{"keys":[%s]}\n' "$entry_json" > "$KEY_FILE"
  chown root:"$RUN_GROUP" "$KEY_FILE"
  chmod 0640 "$KEY_FILE"
  cat >&2 <<EOF

##############################################################################
# SAVE THIS RAW API KEY NOW. IT IS NOT STORED ANYWHERE AND CANNOT BE SHOWN
# AGAIN. Only its SHA-256 hash is in $KEY_FILE.
#
#   $raw_key
#
##############################################################################

EOF
else
  echo "Keeping existing $KEY_FILE (raw keys cannot be recovered from it)."
fi

echo "== installing systemd units and journald cap =="
install -m 0644 "$ROOT/deploy/translator.service" /etc/systemd/system/translator.service
install -d -m 0755 /etc/systemd/journald.conf.d
install -m 0644 "$ROOT/deploy/journald-translator.conf" /etc/systemd/journald.conf.d/translator.conf
systemctl daemon-reload
systemctl enable translator.service
systemctl try-restart systemd-journald || true

echo "== model download =="
if [[ "$DO_MODEL" -eq 0 ]]; then
  echo "Skipped by --skip-model."
elif [[ -n "${HF_TOKEN:-}" ]]; then
  echo "Downloading the pinned gated model. The token is passed via the environment."
  runuser -u "$RUN_USER" -- env \
    HF_TOKEN="$HF_TOKEN" \
    HF_HOME="$MODEL_CACHE" \
    "$VENV/bin/python" "$APP_DIR/scripts/download_model.py" \
    --cache-dir "$MODEL_CACHE" \
    --revision "$MODEL_REVISION"
else
  cat >&2 <<EOF
HF_TOKEN is not set; the gated model was NOT downloaded.
After accepting the model license, run as the service user:

  sudo -u $RUN_USER env HF_TOKEN='<token>' \\
    $VENV/bin/python $APP_DIR/scripts/download_model.py \\
    --cache-dir $MODEL_CACHE --revision $MODEL_REVISION
EOF
fi

if [[ "$DO_START" -eq 1 ]]; then
  systemctl restart translator.service
  echo "translator.service started."
else
  echo "translator.service is enabled but not started. After the model and"
  echo "configuration are ready: sudo systemctl start translator.service"
fi

cat <<'EOF'

Remaining one-time host steps (see README.md):
  1. Configure Tailscale MagicDNS, HTTPS certificates, and the funnel node
     attribute in the tailnet policy.
  2. sudo tailscale funnel --bg --https=10000 18080
  3. sudo tailscale funnel status
  4. Run scripts/selfcheck.sh against https://<host>:10000

Existing Funnel handlers on other ports are not changed by this script.
EOF
