from __future__ import annotations

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from textsplit import (  # noqa: E402
    chunked,
    join_sentences,
    prepare_sentences,
    split_long_sentence,
    split_sentences,
)


class SplitSentencesTests(unittest.TestCase):
    def test_empty_and_whitespace(self) -> None:
        self.assertEqual(split_sentences(""), [])
        self.assertEqual(split_sentences("   \n\t "), [])

    def test_danda_question_and_exclamation(self) -> None:
        text = "पहला वाक्य। दूसरा वाक्य? तीसरा वाक्य!"
        self.assertEqual(
            split_sentences(text),
            ["पहला वाक्य।", "दूसरा वाक्य?", "तीसरा वाक्य!"],
        )

    def test_danda_without_following_space_still_splits(self) -> None:
        self.assertEqual(split_sentences("पहला।दूसरा।"), ["पहला।", "दूसरा।"])

    def test_ascii_decimal_is_not_split(self) -> None:
        self.assertEqual(split_sentences("यह 3.5 किलो है।"), ["यह 3.5 किलो है।"])

    def test_newline_is_a_boundary(self) -> None:
        self.assertEqual(split_sentences("पहला।\nदूसरा।"), ["पहला।", "दूसरा।"])

    def test_order_and_punctuation_are_preserved(self) -> None:
        text = "  एक। दो?  "
        self.assertEqual(split_sentences(text), ["एक।", "दो?"])

    def test_no_punctuation_returns_single_sentence(self) -> None:
        self.assertEqual(split_sentences("बिना विराम"), ["बिना विराम"])


class ChunkedTests(unittest.TestCase):
    def test_chunks(self) -> None:
        self.assertEqual(list(chunked(["a", "b", "c", "d", "e"], 2)), [["a", "b"], ["c", "d"], ["e"]])

    def test_exact_multiple_has_no_empty_tail(self) -> None:
        self.assertEqual(list(chunked(["a", "b", "c", "d"], 2)), [["a", "b"], ["c", "d"]])

    def test_empty_input(self) -> None:
        self.assertEqual(list(chunked([], 4)), [])

    def test_invalid_size(self) -> None:
        with self.assertRaises(ValueError):
            list(chunked(["a"], 0))

    def test_does_not_mutate_input(self) -> None:
        items = ["a", "b", "c"]
        list(chunked(items, 2))
        self.assertEqual(items, ["a", "b", "c"])


class JoinSentencesTests(unittest.TestCase):
    def test_join_with_single_space(self) -> None:
        self.assertEqual(join_sentences(["एक।", "दो।"]), "एक। दो।")

    def test_empty_parts_are_skipped(self) -> None:
        self.assertEqual(join_sentences(["", "एक।", "  ", "दो।"]), "एक। दो।")

    def test_empty(self) -> None:
        self.assertEqual(join_sentences([]), "")


class SplitLongSentenceTests(unittest.TestCase):
    def test_short_sentence_is_unchanged(self) -> None:
        self.assertEqual(split_long_sentence("छोटा वाक्य।", 100), ["छोटा वाक्य।"])

    def test_word_boundaries_and_order(self) -> None:
        text = "एक दो तीन चार पाँच छह"
        chunks = split_long_sentence(text, 12)
        self.assertGreater(len(chunks), 1)
        self.assertTrue(all(len(chunk) <= 12 for chunk in chunks))
        self.assertEqual(" ".join(chunks), text)

    def test_overlong_word_is_hard_split(self) -> None:
        chunks = split_long_sentence("abcdefghij", 4)
        self.assertEqual(chunks, ["abcd", "efgh", "ij"])

    def test_empty(self) -> None:
        self.assertEqual(split_long_sentence("   ", 10), [])

    def test_invalid_size(self) -> None:
        with self.assertRaises(ValueError):
            split_long_sentence("text", 0)


class PrepareSentencesTests(unittest.TestCase):
    def test_sentence_and_length_splitting(self) -> None:
        text = "पहला वाक्य। दूसरा बहुत लंबा वाक्य है जिसे बाँटना है।"
        prepared = prepare_sentences(text, 12)
        self.assertGreater(len(prepared), 2)
        self.assertTrue(all(len(item) <= 12 for item in prepared))
        self.assertEqual(" ".join(prepared), text)

    def test_zero_limit_disables_length_splitting(self) -> None:
        self.assertEqual(prepare_sentences("एक। दो।", 0), ["एक।", "दो।"])


if __name__ == "__main__":
    unittest.main()
