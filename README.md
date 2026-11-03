# Hindi → Malayalam Translation Service

A small, CPU-only service that translates Hindi (`hin_Deva`) to Malayalam
(`mal_Mlym`) on demand. The model is not kept in memory when nobody is using
the service.

**Public URL:** <https://server.tailda589d.ts.net:10000>

> The public URL is protected by bearer keys. Funnel provides TLS but does not
> authenticate callers.

## At a glance

| Item | Value |
|---|---|
| Translation endpoint | `POST /translate` |
| Transcription endpoint | `POST /transcribe` (Hindi audio to text via Whisper small) |
| Health check | `GET /healthz` (no authentication) |
| Status | `GET /status` (authentication required) |
| Warm-up | `POST /warmup` (authentication required) |
| Maximum input | 2000 Unicode characters |
| Maximum audio upload | 25 MB by default |
| Idle shutdown | Worker stops after 10 minutes without a request |
| First request after idle | Roughly 10–15 seconds while the model loads |
| Normal request | Well under a second for short text |
| Gateway | `translator.service`, listening on `127.0.0.1:18080` |
| Worker | Python child process, started by the gateway on demand |
| Model | `ai4bharat/indictrans2-indic-indic-dist-320M` |
| Model revision | `ffb7582b6d43791f1fb26b2153fc065f2e9ea575` |
| Model cache | `/var/lib/translator/hf` |
| Configuration | `/etc/translator/translator.env` |
| API keys | `/etc/translator/keys.json` |
| Logs | `journalctl -u translator` |

The service was installed under `/opt/translator`.

## How it works

```text
Client
  │  HTTPS :10000
  ▼
Tailscale Funnel
  │  HTTP to loopback only
  ▼
Go gateway  (127.0.0.1:18080)          always running
  │  Unix socket /run/translator/worker.sock
  ▼
Python worker (FastAPI + uvicorn)      started only when needed
  │
  ▼
IndicTrans2 model (int8 quantized, CPU)
```

There are two processes by design:

- The **gateway** is small, always running, and handles authentication,
  limits, queueing, and lifecycle.
- The **worker** owns the model. It is disposable, handles one translation or
  transcription at a time, and exits after 10 minutes of inactivity.

The transcription path shares the same gateway lifecycle and safety checks as
`/translate`, but uses a separate Whisper model load that is delayed until the
first audio request.

### Worker lifecycle

```text
stopped ──request──▶ starting ──ready──▶ ready
   ▲                                      │
   │                                      │ 10 min idle
   └──────────── stopping ◀───────────────┘
```

- `stopped`: no Python process and no model in memory.
- `starting`: one worker is loading and quantizing the model.
- `ready`: translations are accepted, one at a time.
- `stopping`: the worker is shutting down.

Concurrent cold requests share one startup attempt. A crash or startup failure
enters a 30-second cooldown so a broken model cannot create a restart loop.

### Request pipeline

The order matters: unauthenticated or malformed traffic must never start the
model.

1. Body size cap (16 KB).
2. Bearer authentication.
3. Per-key rate limit.
4. JSON, UTF-8, text, and length validation.
5. Queue admission.
6. Cold-start wait or worker startup.
7. Single-flight translation.

After repeated failed authentication from one IP, that IP is temporarily
blocked. The block uses `X-Forwarded-For`, supplied by local `tailscaled`.

## Using the API

Set the raw key in your shell (see [Managing keys](#managing-keys)):

```sh
export TRANSLATOR_KEY='<raw key>'
export BASE_URL='https://server.tailda589d.ts.net:10000'
```

### Health check

```sh
curl -sS "$BASE_URL/healthz"
# {"ok":true}
```

This does not touch or start the worker.

### Status

```sh
curl -sS "$BASE_URL/status" \
  -H "Authorization: Bearer $TRANSLATOR_KEY"

# {"state":"ready","seconds_until_idle_shutdown":412}
```

`state` is `stopped`, `starting`, `ready`, or `stopping`.

### Warm-up

Start the worker before expected traffic:

```sh
curl -sS -X POST "$BASE_URL/warmup" \
  -H "Authorization: Bearer $TRANSLATOR_KEY"

# {"state":"starting"}
```

There is no scheduled warm-up; the service is designed to idle at zero.

### Translate

```sh
curl -sS "$BASE_URL/translate" \
  -H "Authorization: Bearer $TRANSLATOR_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"text":"नमस्ते, आप कैसे हैं?"}'

# {"translation":"ഹലോ, നിങ്ങൾ എങ്ങനെയുണ്ട്?"}
```

### Transcribe

```sh
curl -sS -X POST "$BASE_URL/transcribe" \
  -H "Authorization: ******" \
  -F "audio=@/path/to/hindi-audio.wav"

# {"text":"नमस्ते, आप कैसे हैं?"}
```

The transcription endpoint accepts multipart audio, validates the size at the
gateway before the upload is forwarded, and returns the transcribed Hindi text
as JSON.

Errors:

| Condition | Status |
|---|---|
| Request body or audio payload exceeds the configured limit | `413` |
| Missing, malformed, or wrong key | `401` with `{"error":"unauthorized"}` |
| Rate limit, queue full, or failed-auth IP block | `429` + `Retry-After` |
| Invalid JSON/UTF-8, empty text, or more than 2000 characters | `400` |
| Invalid multipart, missing `audio`, empty payload, or worker-side transcription failure | `400` / `500` |
| Cold start failed, cooldown, crash, or translation/transcription timeout | `503` + `Retry-After` |
| Funnel cannot reach the gateway | `502` |

The worker is killed after a translation or transcription timeout; the next
request cold-starts a fresh worker after the cooldown.

## Managing keys

The key file contains only SHA-256 hashes. Raw keys are printed once and are
never written by the service.

Current format:

```json
{"keys":[{"name":"alice","sha256_hash":"..."}]}
```

An array is also accepted:

```json
[{"name":"alice","sha256_hash":"..."}]
```

Generate a new key:

```sh
/opt/translator/bin/gateway genkey alice
```

The output contains `raw_key=...` followed by the JSON entry. Save the raw key
in a password manager. Add the JSON object to the array and install the file
atomically:

```sh
sudo install -m 0640 -o root -g translator keys.json.new /etc/translator/keys.json
sudo systemctl reload translator
```

`reload` re-reads the key file without dropping the worker. If the new file is
invalid, the old keys stay active. Removing an entry revokes that key on the
next reload.

The raw key created during installation cannot be recovered from
`/etc/translator/keys.json`; only its hash is stored there.

## Operations

### Service and logs

```sh
sudo systemctl status translator --no-pager
sudo journalctl -u translator -f
sudo journalctl -u translator -n 100 --no-pager
```

### Start, stop, restart, reload

```sh
sudo systemctl start translator      # start the gateway
sudo systemctl stop translator       # stop the gateway and reap the worker
sudo systemctl restart translator    # restart everything
sudo systemctl reload translator     # reload keys only (SIGHUP)
```

The unit is `Type=notify` with a 60-second watchdog. `reload` does not interrupt
the worker.

### Memory and swap

```sh
free -h
swapon --show
systemctl show translator -p MemoryCurrent -p MemoryPeak -p MemoryMax
```

The unit caps the gateway plus worker at 3.2 GB. The worker is the preferred
OOM victim; the small gateway should stay alive to return `503` responses.

### Listener

```sh
ss -ltnp | grep 18080
```

It must show only `127.0.0.1:18080`.

### Funnel

```sh
sudo tailscale funnel status
```

Expected mapping:

```text
https://server.tailda589d.ts.net:10000
└── / proxy http://127.0.0.1:18080
```

The existing Funnel handlers on ports `443` and `8443` must remain in the
output.

Do **not** run `tailscale funnel reset`; that removes all Funnel mappings,
including the existing `443` and `8443` handlers. If the `10000` mapping is
lost, add it back idempotently with:

```sh
sudo tailscale funnel --bg --https=10000 18080
```

### Selfcheck

```sh
TRANSLATOR_KEY='<raw key>' \
  /opt/translator/scripts/selfcheck.sh https://server.tailda589d.ts.net:10000
```

The first translation may take 10–15 seconds while the model loads.

## Configuration

Settings are environment variables in `/etc/translator/translator.env`. The
file is installed from `deploy/translator.env.example` and contains comments
for every option.

Common settings:

| Variable | Default | Purpose |
|---|---|---|
| `TRANSLATOR_PORT` | `18080` | Loopback HTTP port |
| `TRANSLATOR_IDLE_TIMEOUT` | `10m` | Stop the worker after this idle time |
| `TRANSLATOR_STARTUP_TIMEOUT` | `120s` | Maximum model load time |
| `TRANSLATOR_COLD_START_WAIT` | `60s` | How long a request waits for a cold start |
| `TRANSLATOR_TRANSLATE_TIMEOUT` | `60s` | Queue wait plus worker request timeout |
| `TRANSLATOR_STARTUP_COOLDOWN` | `30s` | Cooldown after a failure or crash |
| `TRANSLATOR_MAX_BODY_BYTES` | `16384` | Maximum JSON request body size |
| `TRANSLATOR_MAX_AUDIO_BYTES` | `25 MB` | Maximum multipart audio upload size |
| `TRANSLATOR_MAX_TEXT_RUNES` | `2000` | Maximum input length |
| `TRANSLATOR_WHISPER_MODEL` | `openai/whisper-small` | Speech-to-text model for `/transcribe` |
| `TRANSLATOR_RATE_LIMIT_PER_MINUTE` | `30` | Per-key sustained rate |
| `TRANSLATOR_RATE_LIMIT_BURST` | `30` | Per-key burst |
| `TRANSLATOR_QUEUE_DEPTH` | `8` | Waiting requests |
| `TRANSLATOR_MODEL_CACHE` | `/var/lib/translator/hf` | Model cache root |
| `TRANSLATOR_BEAMS` | `4` | Generation beams |
| `TRANSLATOR_BATCH_SIZE` | `4` | Sentences per generation batch |
| `TRANSLATOR_MAX_LENGTH` | `256` | Token limit per batch |
| `TRANSLATOR_MAX_CHUNK_CHARS` | `400` | Split long sentences before tokenizing |
| `TRANSLATOR_CLIENT_IP_HEADER` | empty | Set to `X-Forwarded-For` behind Funnel |

The example environment raises `TRANSLATOR_COLD_START_WAIT` to `90s` for this
4-core CPU host. Lower `BEAMS` or `BATCH_SIZE` if long requests approach the
translation timeout.

## Planned improvements

The current service is stable and simple, but the following upgrades would make
it easier to operate and harder to regress over time:

- Add focused integration tests for `/translate`, `/transcribe`, auth failures,
  rate limiting, queue saturation, timeout handling, and cold starts.
- Add audio validation before Whisper inference: reject unsupported or
  corrupt payloads early, validate duration and sample rate, and catch empty
  or malformed audio sooner.
- Add richer observability for per-route latency, queue waits, worker startup
  failures, and timeout counts without logging request text or model output.
- Consider separate fair queues for text and audio traffic if transcription
  volume grows and long recordings begin to starve translation requests.
- Add request IDs in the gateway logs and headers to simplify debugging while
  keeping payload content out of logs.

These changes are intentionally small and operationally focused; they do not
change the current architecture or the public API contract.

## Repository layout

```text
cmd/gateway/                 CLI: serve + genkey, signals, sd_notify
internal/auth/               key parsing, SHA-256 hashes, genkey
internal/config/             TRANSLATOR_* environment settings
internal/ratelimit/          per-key token bucket, failed-auth IP limiter
internal/sdnotify/           READY/STOPPING/WATCHDOG notifications
internal/server/             HTTP API, pipeline, queue, logging
internal/worker/             worker process state machine and socket client
worker/worker.py             FastAPI/uvicorn worker, model load and quantization
worker/textsplit.py          sentence splitting and long-chunk splitting
worker/tests/                splitter unit tests
tests/fake_worker.py         controllable stub for Go integration tests
deploy/                      systemd unit, env example, keys example, journald cap
scripts/install.sh           target-host installer
scripts/download_model.py    pinned, gated model download
scripts/selfcheck.sh         public end-to-end check
scripts/setup_swap.sh        dedicated swap file
bin/gateway                  built static binary
```

Local-only directories, not part of the source:

- `.tools/go/` — the local Go toolchain.
- `worker/.venv/` — optional local Python environment for worker development.
  It is not needed to run the unit tests or the installed service.

`__pycache__/` directories and `nenv/` are generated and safe to delete.
`bin/gateway` is kept because `scripts/install.sh` uses it when Go is not
available on the target. Remove `.tools/go/` only if Go is installed
system-wide.

## Build and test

Go is available here at `.tools/go/bin/go`. Python unit tests need only the
standard library.

```sh
cd /home/admin/aravinddrive/dev

make GO=.tools/go/bin/go check
```

This runs `go vet`, `go test -race`, and the Python unit tests. The Go
integration tests use `python3` for the fake worker.

Build the static binary:

```sh
CGO_ENABLED=0 .tools/go/bin/go build -trimpath -ldflags='-s -w' \
  -o bin/gateway ./cmd/gateway
```

To run the worker locally with the real model, create a virtual environment
and install `worker/requirements.txt`; the deployed runtime uses
`/opt/translator/venv`.

## Installing or rebuilding the server

The service is already installed, but the full sequence is:

```sh
# 1. Build and test
make GO=.tools/go/bin/go check

# 2. Install files, service account, venv, swap, and systemd unit
sudo scripts/install.sh --swap

# 3. Download the gated model (token and accepted license required)
sudo -u translator env HF_TOKEN='hf_...' \
  /opt/translator/venv/bin/python /opt/translator/scripts/download_model.py \
  --cache-dir /var/lib/translator/hf \
  --revision ffb7582b6d43791f1fb26b2153fc065f2e9ea575

# 4. Start the gateway
sudo systemctl start translator

# 5. Add Funnel without touching existing handlers
sudo tailscale funnel --bg --https=10000 18080

# 6. End-to-end check
TRANSLATOR_KEY='<raw key>' \
  scripts/selfcheck.sh https://server.tailda589d.ts.net:10000
```

The installer accepts `--hf-token-file /root/hf_token` instead of exporting
`HF_TOKEN`. The download script never writes the token.

### Tailnet prerequisites

In the Tailscale admin console:

1. Enable MagicDNS.
2. Enable HTTPS certificates.
3. Add the `funnel` node attribute.
4. Disable node-key expiry for the server.

`tailscaled.service` must be enabled at boot. The `--bg` Funnel mapping is
stored by `tailscaled` and survives reboots.

## Troubleshooting

**`/healthz` works but translation returns `503 worker unavailable`.**
Check `journalctl -u translator`. Common causes are a missing model cache, wrong
revision, missing Python packages, insufficient memory, or a startup still in
progress. Startup failures enter a 30-second cooldown.

**The model download returns 401 or 403.**
The model is gated. Sign in, accept the license on the model page, and verify
the token. The runtime is fully offline and never downloads by itself.

**Every public request appears to come from `127.0.0.1`.**
Set `TRANSLATOR_CLIENT_IP_HEADER=X-Forwarded-For` in
`/etc/translator/translator.env` and restart. The gateway listens only on
loopback, so only local `tailscaled` can supply this header.

**Funnel returns 502.**
Confirm the gateway is active and listening:

```sh
sudo systemctl status translator --no-pager
ss -ltnp | grep 18080
sudo tailscale funnel status
sudo journalctl -u tailscaled -n 50 --no-pager
```

**Translation times out and the worker is killed.**
Check RSS and swap first. Then lower `TRANSLATOR_BEAMS` or
`TRANSLATOR_BATCH_SIZE`, or reduce `TRANSLATOR_MAX_TEXT_RUNES`.

**Key rotation appears to do nothing.**
Check the reload log line. A parse error intentionally keeps the old key set
active. Verify that the hash is SHA-256 of the raw bearer secret, not the raw
key itself.

**The host runs out of memory.**
Confirm swap is active, keep `MemoryMax=3200M`, and avoid raising concurrency.
One translation at a time is enforced deliberately.

## Security notes

- The gateway binds only to `127.0.0.1`. Funnel is the only public path.
- Funnel terminates TLS but does not authenticate users; bearer keys are the
  only access control.
- The key file stores only SHA-256 hashes. Raw keys are printed once.
- All authentication failures return the same `401` body.
- `X-Forwarded-For` is trusted only because the listener is loopback-only.
- The model revision is pinned and runtime downloads are disabled
  (`HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`).
- `trust_remote_code=True` is required by this model. Review model code before
  changing the revision.
- Do not enable `TRANSLATOR_DEBUG_HEADERS` permanently on a public deployment.

## Verification status

Build host: Go build, `go vet`, `go test -race`, Python unit tests, and
fake-worker integration tests pass. The tests cover cold start, idle
stop/restart, crash and startup-failure cooldowns, translation timeout, queue
limits, SIGHUP key rotation, authentication, and rate limiting.

Target host (`server`, 2026-11-03): the pinned model was downloaded (1.21 GiB).
The worker loads and quantizes it in about 5–7 seconds and returns real
Hindi-to-Malayalam translations. `translator.service` is enabled and active,
the listener is loopback-only, and cgroup memory peaked at about 1.7 GB against
the 3.2 GB cap with no OOM messages. The public selfcheck passed over
`https://server.tailda589d.ts.net:10000`. A real idle stop, an unexpected
worker kill (`503` with `Retry-After: 30`), and cold-start recovery after the
cooldown were also verified.

A full reboot test was not performed, to avoid interrupting other services on
the host. The unit is enabled and the Funnel mapping uses `--bg`, so both are
configured to survive a reboot.

## Compatibility note

Transformers 4.57 defaults to a dynamic KV cache, but this model's custom
decoder still expects the legacy tuple cache. The worker forces the legacy
cache path before generation; without this, inference fails with:

```text
AttributeError: 'NoneType' object has no attribute 'shape'
```
