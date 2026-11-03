#!/usr/bin/env python3
"""Disposable Hindi -> Malayalam translation worker.

The process binds a private Unix socket, reports 503 on /ready until the model
is loaded and dynamically quantized, and serializes generation. It never logs
input or output text.
"""

from __future__ import annotations

import asyncio
import gc
import json
import logging
import os
import re
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from typing import Any

# --- environment and early safety setup -------------------------------------

MODEL_ID = "ai4bharat/indictrans2-indic-indic-dist-320M"  # fixed by design
DEFAULT_MODEL_REVISION = "ffb7582b6d43791f1fb26b2153fc065f2e9ea575"
MODEL_CACHE = os.environ.get("TRANSLATOR_MODEL_CACHE", "/var/lib/translator/hf")
MODEL_REVISION = os.environ.get("TRANSLATOR_MODEL_REVISION", DEFAULT_MODEL_REVISION).strip()
SOCKET_PATH = os.environ.get("TRANSLATOR_SOCKET", "/run/translator/worker.sock")
BEAMS = int(os.environ.get("TRANSLATOR_BEAMS", "4"))
BATCH_SIZE = int(os.environ.get("TRANSLATOR_BATCH_SIZE", "4"))
MAX_LENGTH = int(os.environ.get("TRANSLATOR_MAX_LENGTH", "256"))
MAX_CHUNK_CHARS = int(os.environ.get("TRANSLATOR_MAX_CHUNK_CHARS", "400"))
MAX_TEXT_RUNES = int(os.environ.get("TRANSLATOR_MAX_TEXT_RUNES", "2000"))
MAX_BODY_BYTES = int(os.environ.get("TRANSLATOR_MAX_BODY_BYTES", "16384"))
OOM_SCORE_ADJ = int(os.environ.get("TRANSLATOR_OOM_SCORE_ADJ", "500"))

os.umask(0o077)
# These must be set before transformers/huggingface_hub are imported. Runtime is
# strictly offline; model files were downloaded by scripts/download_model.py.
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"
os.environ["HF_HOME"] = MODEL_CACHE
os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "timestamp": datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
            "level": record.levelname.lower(),
            "logger": record.name,
            "pid": os.getpid(),
            "msg": record.getMessage(),
        }
        for key in (
            "endpoint",
            "status",
            "latency_ms",
            "input_chars",
            "duration_ms",
            "rss_mb",
            "revision",
            "threads",
            "reason",
            "error_type",
        ):
            value = getattr(record, key, None)
            if value is not None:
                payload[key] = value
        return json.dumps(payload, ensure_ascii=False, separators=(",", ":"))


def configure_logging() -> None:
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    for h in list(root.handlers):
        root.removeHandler(h)
    handler = logging.StreamHandler(stream=sys.stdout)
    handler.setFormatter(JSONFormatter())
    root.addHandler(handler)
    # Uvicorn is configured with log_config=None. Let its named loggers
    # propagate to the JSON root handler instead of installing plain handlers.
    for name in ("uvicorn", "uvicorn.error", "uvicorn.access", "uvicorn.asgi"):
        lg = logging.getLogger(name)
        lg.handlers = []
        lg.propagate = True


configure_logging()
log = logging.getLogger("worker")

REVISION_RE = re.compile(r"^[0-9a-f]{40}$")
if not REVISION_RE.match(MODEL_REVISION):
    log.error("TRANSLATOR_MODEL_REVISION must be a full 40-character commit hash", extra={"error_type": "configuration"})
    sys.exit(2)


def raise_oom_score() -> None:
    try:
        with open("/proc/self/oom_score_adj", "w", encoding="ascii") as f:
            f.write(f"{OOM_SCORE_ADJ}\n")
        with open("/proc/self/oom_score_adj", "r", encoding="ascii") as f:
            actual = f.read().strip()
        log.info("oom_score_adj configured", extra={"reason": f"value={actual}"})
    except OSError as exc:
        log.warning("could not set oom_score_adj", extra={"error_type": type(exc).__name__})


raise_oom_score()

# Heavy imports follow the environment and OOM setup.
import torch  # noqa: E402
import uvicorn  # noqa: E402
from fastapi import FastAPI, Request  # noqa: E402
from fastapi.responses import JSONResponse  # noqa: E402
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer  # noqa: E402

try:
    # Current IndicTransToolkit documentation recommends the top-level import.
    from IndicTransToolkit import IndicProcessor  # noqa: E402
except ImportError:  # pragma: no cover - fallback for alternate releases
    from IndicTransToolkit.processor import IndicProcessor  # noqa: E402

from textsplit import chunked, join_sentences, prepare_sentences  # noqa: E402


def available_cores() -> int:
    try:
        return max(1, len(os.sched_getaffinity(0)))
    except (AttributeError, OSError):
        return max(1, os.cpu_count() or 1)


THREADS = available_cores()
torch.set_num_threads(THREADS)
try:
    torch.set_num_interop_threads(1)
except RuntimeError:
    pass


def read_rss_mb() -> int:
    try:
        with open("/proc/self/status", "r", encoding="ascii") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) // 1024
    except OSError:
        pass
    return 0


class ModelHolder:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._ready = False
        self._model = None
        self._tokenizer = None
        self._processor = None

    @property
    def ready(self) -> bool:
        with self._lock:
            return self._ready

    def set(self, model, tokenizer, processor) -> None:
        with self._lock:
            self._model = model
            self._tokenizer = tokenizer
            self._processor = processor
            self._ready = True

    def get(self):
        with self._lock:
            return self._ready, self._model, self._tokenizer, self._processor


HOLDER = ModelHolder()
EXECUTOR = ThreadPoolExecutor(max_workers=1, thread_name_prefix="translate")
APP = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)


def load_model() -> None:
    started = time.monotonic()
    log.info(
        "model load starting",
        extra={
            "revision": MODEL_REVISION,
            "threads": THREADS,
            "duration_ms": 0,
        },
    )
    try:
        if not os.path.isdir(MODEL_CACHE):
            raise RuntimeError("model cache directory is missing")
        if not MODEL_REVISION:
            raise RuntimeError("model revision is not pinned")

        tokenizer = AutoTokenizer.from_pretrained(
            MODEL_ID,
            trust_remote_code=True,
            revision=MODEL_REVISION,
            cache_dir=MODEL_CACHE,
            local_files_only=True,
        )
        model = AutoModelForSeq2SeqLM.from_pretrained(
            MODEL_ID,
            trust_remote_code=True,
            revision=MODEL_REVISION,
            cache_dir=MODEL_CACHE,
            low_cpu_mem_usage=True,
            torch_dtype=torch.float32,
            local_files_only=True,
        )
        model.eval()

        quant_started = time.monotonic()
        model = torch.quantization.quantize_dynamic(
            model,
            {torch.nn.Linear},
            dtype=torch.qint8,
            inplace=True,
        )
        model.eval()
        # transformers 4.57 initializes a DynamicCache for generation, but this
        # model's custom decoder still indexes the legacy tuple cache. Force
        # the legacy path so the first generation does not fail with
        # "AttributeError: 'NoneType' object has no attribute 'shape'".
        model._supports_default_dynamic_cache = lambda: False
        quant_ms = int((time.monotonic() - quant_started) * 1000)
        gc.collect()

        processor = IndicProcessor(inference=True)
        HOLDER.set(model, tokenizer, processor)
        total_ms = int((time.monotonic() - started) * 1000)
        log.info(
            "model load complete",
            extra={"duration_ms": total_ms, "rss_mb": read_rss_mb(), "threads": THREADS},
        )
        log.info("quantization complete", extra={"duration_ms": quant_ms, "rss_mb": read_rss_mb()})
    except BaseException as exc:  # pragma: no cover - requires real model/cache
        # Never include request text here: this path is model/cache related. Make
        # the process exit so the gateway observes startup failure immediately
        # and applies its cooldown.
        log.error(
            "model load failed",
            extra={"duration_ms": int((time.monotonic() - started) * 1000), "error_type": type(exc).__name__},
        )
        os._exit(1)


def translate_text(text: str) -> str:
    ready, model, tokenizer, processor = HOLDER.get()
    if not ready or model is None or tokenizer is None or processor is None:
        raise RuntimeError("model is not ready")

    sentences = prepare_sentences(text, MAX_CHUNK_CHARS)
    if not sentences:
        return ""

    translated: list[str] = []
    # One generation at a time is enforced by the single-thread executor and by
    # the gateway's semaphore.
    with torch.inference_mode():
        for batch in chunked(sentences, BATCH_SIZE):
            processed = processor.preprocess_batch(
                batch,
                src_lang="hin_Deva",
                tgt_lang="mal_Mlym",
            )
            inputs = tokenizer(
                processed,
                truncation=True,
                padding="longest",
                max_length=MAX_LENGTH,
                return_tensors="pt",
                return_attention_mask=True,
            )
            generated = model.generate(
                **inputs,
                use_cache=True,
                min_length=0,
                max_length=MAX_LENGTH,
                num_beams=BEAMS,
                num_return_sequences=1,
            )
            decoded = tokenizer.batch_decode(
                generated,
                skip_special_tokens=True,
                clean_up_tokenization_spaces=True,
            )
            postprocessed = processor.postprocess_batch(decoded, lang="mal_Mlym")
            if len(postprocessed) != len(batch):
                raise RuntimeError("postprocessing returned the wrong number of sentences")
            translated.extend(postprocessed)
            del inputs, generated, decoded, postprocessed, processed

    return join_sentences(translated)


@APP.middleware("http")
async def request_logging(request: Request, call_next):
    started = time.monotonic()
    response = await call_next(request)
    if request.url.path != "/ready":
        log.info(
            "worker request",
            extra={
                "endpoint": request.url.path,
                "status": response.status_code,
                "latency_ms": int((time.monotonic() - started) * 1000),
                "input_chars": getattr(request.state, "input_chars", None),
            },
        )
    return response


@APP.exception_handler(Exception)
async def unhandled_exception(_request: Request, exc: Exception):
    log.error("translation request failed", extra={"error_type": type(exc).__name__})
    return JSONResponse(status_code=500, content={"error": "translation failed"})


@APP.get("/ready")
async def ready():
    if HOLDER.ready:
        return JSONResponse(status_code=200, content={"ready": True})
    return JSONResponse(status_code=503, content={"ready": False})


@APP.post("/translate")
async def translate(request: Request):
    if not HOLDER.ready:
        return JSONResponse(status_code=503, content={"error": "not ready"})

    raw = await request.body()
    if len(raw) > MAX_BODY_BYTES:
        return JSONResponse(status_code=413, content={"error": "request body too large"})
    try:
        body = raw.decode("utf-8")
        payload = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return JSONResponse(status_code=400, content={"error": "invalid json"})
    if not isinstance(payload, dict) or not isinstance(payload.get("text"), str):
        return JSONResponse(status_code=400, content={"error": "text is required"})

    text = payload["text"].strip()
    if not text:
        return JSONResponse(status_code=400, content={"error": "text is required"})
    chars = len(text)
    request.state.input_chars = chars
    if chars > MAX_TEXT_RUNES:
        return JSONResponse(status_code=400, content={"error": "text too long"})

    loop = asyncio.get_running_loop()
    try:
        translation = await loop.run_in_executor(EXECUTOR, translate_text, text)
    except Exception as exc:
        log.error("generation failed", extra={"error_type": type(exc).__name__})
        return JSONResponse(status_code=500, content={"error": "translation failed"})
    return JSONResponse(status_code=200, content={"translation": translation})


def main() -> None:
    parent = os.path.dirname(SOCKET_PATH)
    if parent:
        os.makedirs(parent, mode=0o700, exist_ok=True)
    try:
        os.unlink(SOCKET_PATH)
    except FileNotFoundError:
        pass
    except OSError as exc:
        log.error("could not remove stale worker socket", extra={"error_type": type(exc).__name__})
        sys.exit(1)

    log.info(
        "worker starting",
        extra={"rss_mb": read_rss_mb(), "threads": THREADS, "revision": MODEL_REVISION},
    )
    threading.Thread(target=load_model, name="model-loader", daemon=True).start()

    config = uvicorn.Config(
        APP,
        uds=SOCKET_PATH,
        log_config=None,
        log_level="info",
        access_log=False,
        workers=1,
        timeout_graceful_shutdown=5,
    )
    server = uvicorn.Server(config)
    server.run()
    log.info("worker exiting")


if __name__ == "__main__":
    main()
