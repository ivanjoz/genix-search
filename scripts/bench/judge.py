#!/usr/bin/env python3
"""PLAN_BENCH.md 5.2: grade every document for every query with a rule over the
whole text (no 8-letter cut, no engine) -> genix-s3/testdata/bench/qrels.tsv.

    python3 scripts/bench/judge.py

3  the target, or a document with the same normalized name
2  the name has every word of the query (the last one as a prefix in
   `escribiendo`; the corrected word in `typo`; in `plural`, the word as typed
   or as the target has it, singular or plural); with a group (nombre+grupo) the
   words may be in the category too; in `grupo`, the document is a member
1  it has some word of the query (a word of 2 letters or more, not a number)
   but not all
0  none (not written)

The grade does not depend on the base: the queries of a group are judged with
the category, wherever they run.
"""
import sys

from common import DATA, Corpus, correct, edit1, is_number, number_swap, read_queries, tokens

GROUPED = ("nombre+grupo", "grupo")


def forms(c, w):
    """A word and its singular or plural, those the corpus has: the number does
    not change what a product is ("filetes" and "Filete de panga")."""
    if is_number(w):
        return [w]
    out = {w}
    for v in (w + "s", w + "es", w[:-1], w[:-2]):
        if v in c.by_any and number_swap(w, v):
            out.add(v)
    return sorted(out)


def query_words(c, q):
    """The words the rule looks for, each as the list of forms it may take: the
    normalized query, with the typo corrected, in singular and plural."""
    words = tokens(q["text"])
    if q["cat"] in ("typo", "plural"):
        fixed = correct(c, q["target"], words, edit1 if q["cat"] == "typo" else number_swap)
        if fixed is None:
            raise SystemExit(f"{q['id']}: the corrected word of {q['text']!r} is not unique")
        words = fixed
    if q["cat"] == "grupo":
        return [[w] for w in words]  # a category is matched as written
    out = [forms(c, w) for w in words]
    if q["cat"] == "escribiendo":
        out[-1] = [words[-1]]  # a cut word is already a prefix
    return out


def judge(c, q):
    """The grades of the query: {doc id: 1..3}."""
    words = query_words(c, q)
    group = q["cat"] in GROUPED
    prefix = q["cat"] == "escribiendo"
    grades = {}

    if not words:
        return grades

    def docs(n, forms):
        out = set()
        for w in forms:
            out |= c.docs(w, prefix=prefix and n == len(words) - 1, group=group)
        return out

    if q["cat"] == "grupo":
        for i in c.ids:
            if c.ctok[i] == [w for w, in words]:
                grades[i] = 2
    else:
        every = None
        for n, forms in enumerate(words):
            every = docs(n, forms) if every is None else every & docs(n, forms)
        for i in every:
            grades[i] = 2

    # Some word, but not all.
    partial = set()
    for n, forms in enumerate(words):
        if len(forms[0]) < 2 or is_number(forms[0]):
            continue
        partial |= docs(n, forms)
    for i in partial:
        grades.setdefault(i, 1)

    if q["target"] is not None:
        for i in c.ids:
            if c.norm_name[i] == c.norm_name[q["target"]]:
                grades[i] = 3

    return grades


def main():
    c = Corpus()
    queries = read_queries()
    total = 0
    with open(DATA / "qrels.tsv", "w", encoding="utf-8") as f:
        f.write("consulta\tid\tnota\n")
        for q in queries:
            grades = judge(c, q)
            if q["cat"] == "negativa" and grades:
                sys.exit(f"{q['id']} {q['text']!r}: a negative query has relevant documents")
            if q["target"] is not None and grades.get(q["target"]) != 3:
                sys.exit(f"{q['id']}: the target does not get a 3")
            for i in sorted(grades):
                f.write(f"{q['id']}\t{i}\t{grades[i]}\n")
            total += len(grades)
    print(f"{len(queries)} queries, {total} judged documents")


if __name__ == "__main__":
    main()
