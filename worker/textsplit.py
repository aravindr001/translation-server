"""Small, model-free sentence utilities for the translation worker."""

from __future__ import annotations

import re
from typing import Iterable, List

# Split after Hindi danda (U+0964), question mark, exclamation mark, or full
# stop. ASCII punctuation requires following whitespace so decimals such as
# "3.5" are not split; a danda is a strong boundary even without a space. The
# delimiter is retained on the preceding sentence.
_BOUNDARY = re.compile(r"(?<=[\u0964?!.])\s+|(?<=\u0964)(?=\S)")


def split_sentences(text: str) -> List[str]:
    """Split text into ordered sentences, preserving sentence punctuation."""
    stripped = text.strip()
    if not stripped:
        return []
    return [part for part in _BOUNDARY.split(stripped) if part]


def chunked(items: Iterable[str], size: int) -> Iterable[List[str]]:
    """Yield lists of at most size items."""
    if size < 1:
        raise ValueError("size must be >= 1")
    batch: List[str] = []
    for item in items:
        batch.append(item)
        if len(batch) == size:
            yield batch
            batch = []
    if batch:
        yield batch


def join_sentences(parts: Iterable[str]) -> str:
    """Rejoin translated sentences in order with a single space."""
    cleaned = [part.strip() for part in parts if part and part.strip()]
    return " ".join(cleaned)


def split_long_sentence(sentence: str, max_chars: int) -> List[str]:
    """Split an over-long sentence on word boundaries.

    The tokenizer truncates at MAX_LENGTH tokens. Splitting very long sentences
    first avoids silently dropping the tail of a 2000-rune input. A single word
    longer than max_chars is split as a last resort.
    """
    if max_chars < 1:
        raise ValueError("max_chars must be >= 1")
    sentence = sentence.strip()
    if not sentence or len(sentence) <= max_chars:
        return [sentence] if sentence else []

    chunks: List[str] = []
    current = ""
    for word in sentence.split():
        if len(word) > max_chars:
            if current:
                chunks.append(current)
                current = ""
            for start in range(0, len(word), max_chars):
                chunks.append(word[start : start + max_chars])
            continue
        candidate = f"{current} {word}" if current else word
        if len(candidate) <= max_chars:
            current = candidate
        else:
            chunks.append(current)
            current = word
    if current:
        chunks.append(current)
    return chunks


def prepare_sentences(text: str, max_chars: int = 0) -> List[str]:
    """Split text into sentences and optionally cap each chunk's length."""
    sentences = split_sentences(text)
    if max_chars <= 0:
        return sentences
    prepared: List[str] = []
    for sentence in sentences:
        prepared.extend(split_long_sentence(sentence, max_chars))
    return prepared
