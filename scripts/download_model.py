#!/usr/bin/env python3
"""One-time, pinned model download into the fixed Hugging Face cache.

Run this with the worker venv. The model is gated on Hugging Face: the account
associated with HF_TOKEN must have accepted the model license. The token is read
from the environment only and is never printed or written by this script.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
from pathlib import Path

MODEL_ID = "ai4bharat/indictrans2-indic-indic-dist-320M"
DEFAULT_REVISION = "ffb7582b6d43791f1fb26b2153fc065f2e9ea575"
DEFAULT_CACHE = "/var/lib/translator/hf"
REVISION_RE = re.compile(r"^[0-9a-f]{40}$")


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--model", default=MODEL_ID)
    p.add_argument(
        "--revision",
        default=os.environ.get("TRANSLATOR_MODEL_REVISION", DEFAULT_REVISION),
        help="40-character Hugging Face commit hash (not a branch name)",
    )
    p.add_argument(
        "--cache-dir",
        default=os.environ.get("TRANSLATOR_MODEL_CACHE", DEFAULT_CACHE),
    )
    return p.parse_args()


def directory_size(path: Path) -> int:
    total = 0
    for root, _dirs, files in os.walk(path):
        for name in files:
            try:
                total += (Path(root) / name).stat().st_size
            except OSError:
                pass
    return total


def main() -> int:
    args = parse_args()
    if not REVISION_RE.match(args.revision):
        print("ERROR: --revision must be a full 40-character lowercase commit hash", file=sys.stderr)
        return 2

    # This is the one-time download step; make sure an inherited service env
    # cannot accidentally force offline mode.
    os.environ.pop("HF_HUB_OFFLINE", None)
    os.environ.pop("TRANSFORMERS_OFFLINE", None)

    try:
        from huggingface_hub import snapshot_download
    except ImportError:
        print("ERROR: huggingface_hub is not installed; use the worker venv", file=sys.stderr)
        return 2

    token = os.environ.get("HF_TOKEN") or None
    cache = Path(args.cache_dir)
    cache.mkdir(parents=True, exist_ok=True)
    print(f"Downloading {args.model} at revision {args.revision}")
    print(f"Cache directory: {cache}")
    if token is None:
        print("HF_TOKEN is not set; this will fail if the gated model has not already been accepted.")

    try:
        path = snapshot_download(
            repo_id=args.model,
            revision=args.revision,
            cache_dir=str(cache),
            token=token,
            # The repository offers safetensors; skip the duplicate .bin weight
            # file to keep the fixed cache as small as possible.
            ignore_patterns=["*.bin"],
        )
    except Exception as exc:  # message may include request URLs, never the token
        print(f"ERROR: model download failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        print(
            "Check that HF_TOKEN is valid and that the account has accepted the model "
            "license at https://huggingface.co/ai4bharat/indictrans2-indic-indic-dist-320M",
            file=sys.stderr,
        )
        return 1

    snapshot = Path(path)
    required = ["config.json", "model.safetensors", "tokenizer_config.json"]
    missing = [name for name in required if not (snapshot / name).exists()]
    if missing:
        print(f"ERROR: snapshot is incomplete; missing: {', '.join(missing)}", file=sys.stderr)
        return 1

    print(f"Snapshot: {snapshot}")
    print(f"Size: {directory_size(snapshot) / (1024**3):.2f} GiB")
    print("Done. Runtime is offline and pinned to this revision.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
