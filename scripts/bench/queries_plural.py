#!/usr/bin/env python3
"""25 queries of the category `plural` -> appended to
genix-s3/testdata/bench/queries.tsv as q201..q225.

    python3 scripts/bench/queries_plural.py

Like `exacta` (2-3 words in a row of a product's name), with one word in the
other number: "galletas" where the name says "galleta", "bolsa" where it says
"bolsas". The 200 queries of queries.py stay as they are; running this again
rewrites only q201..q225.
"""
import random

from common import DATA, STOPWORDS, Corpus, correct, fold, normalize, number_swap, raw_words, read_queries, tokens

SEED = 20261006
FIRST = 201
N = 25

rng = random.Random(SEED)
c = Corpus()
VOCAB = set(c.vocab)


def content(i):
    """The words of a name that are one word of the index, as typed."""
    out = []
    for w in raw_words(c.name[i]):
        t = normalize(w)
        if len(t) == 1 and len(t[0]) >= 3 and t[0].isalpha() and t[0] not in STOPWORDS:
            out.append(w)
    return out


def other_number(word):
    """The word in the other number, or None: -s after a vowel, -es after l,
    n, r, d or j, and the other way. The other form must be a word of the
    corpus too, as people type it ("bolsas" for "Bolsa"): that keeps out
    "eaus" or "sines", and tells "tomates" (tomate) from "panes" (pan)."""
    f = fold(word)
    if f.endswith("s"):
        for cut in (1, 2):
            if normalize(f[:-cut]) == [f[:-cut]] and f[:-cut] in VOCAB and len(f) - cut >= 3:
                return word[:-cut]
        return None
    other = word + "s" if f[-1] in "aeiou" else word + "es" if f[-1] in "lnrdj" else None
    if other is None or tokens(other) != [fold(other)] or fold(other) not in VOCAB:
        return None
    return other


def main():
    queries = [q for q in read_queries() if int(q["id"][1:]) < FIRST]
    used = {q["target"] for q in queries if q["target"]}
    order = list(c.ids)
    rng.shuffle(order)

    made = []  # (text, target)
    plural_typed = 0
    for i in order:
        if len(made) == N:
            break
        if i in used:
            continue
        words = content(i)
        if len(words) < 3:
            continue
        k = rng.choice((2, 3))
        start = rng.randrange(len(words) - k + 1)
        w = words[start : start + k]
        j = rng.randrange(k)
        if normalize(w[j])[0] in tokens(c.brand[i]):
            continue  # nobody types a brand in plural
        swapped = other_number(w[j])
        if swapped is None:
            continue
        # Half of them type the plural of a singular, half the other way.
        typed_plural = len(fold(swapped)) > len(fold(w[j]))
        if typed_plural and plural_typed >= N // 2 + 1 or not typed_plural and len(made) - plural_typed >= N // 2 + 1:
            continue
        text = " ".join(swapped if n == j else x for n, x in enumerate(w))
        q = tokens(text)
        if len(q) != k or q[j] in c.nset[i]:
            continue
        fixed = correct(c, i, q, number_swap)
        if fixed is None or len(c.matching(fixed)) > 5:
            continue
        made.append((text, i))
        used.add(i)
        plural_typed += typed_plural

    if len(made) < N:
        raise SystemExit(f"only {len(made)} plural queries")

    with open(DATA / "queries.tsv", "w", encoding="utf-8") as f:
        f.write("id\tcategoria\tconsulta\tobjetivo\tbases\n")
        for q in queries:
            f.write(f"{q['id']}\t{q['cat']}\t{q['text']}\t{q['target'] or ''}\t{q['bases']}\n")
        for n, (text, target) in enumerate(made):
            f.write(f"q{FIRST + n:03d}\tplural\t{text}\t{target}\tAB\n")
    print(f"{len(made)} plural queries ({plural_typed} type a plural)")


main()
