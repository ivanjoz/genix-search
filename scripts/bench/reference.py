#!/usr/bin/env python3
"""PLAN_BENCH.md 3 and 5.1: the SQLite FTS5 reference databases and their top 10
-> genix-s3/bench/db/{names,groups}.sqlite and genix-s3/testdata/bench/reference.tsv

    python3 scripts/bench/reference.py

R1  BM25: unicode61 tokenizer without diacritics, the words of the query in OR,
    the last one also as a prefix (like ours), stopwords of the query dropped.
    In the groups base the category weighs 0.3 of the name.
R2  BM25 over trigrams: the trigrams of every word of the query in OR. It
    tolerates typos and cut words, and it matches a lot of noise.

Ties are broken by product id. The Go test does not need SQLite: it reads
reference.tsv.
"""
import re
import sqlite3

from common import DB, DATA, STOPWORDS, Corpus, fold, read_queries


def build(c, path, grouped):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.unlink(missing_ok=True)
    db = sqlite3.connect(path)
    cols = "nombre, categoria" if grouped else "nombre"
    db.execute(f"CREATE VIRTUAL TABLE fts USING fts5({cols}, tokenize='unicode61 remove_diacritics 2')")
    db.execute(f"CREATE VIRTUAL TABLE tri USING fts5({cols}, tokenize='trigram remove_diacritics 1')")
    for i in c.ids:
        row = (i, c.name[i], c.cat[i]) if grouped else (i, c.name[i])
        marks = "?, ?, ?" if grouped else "?, ?"
        for table in ("fts", "tri"):
            db.execute(f"INSERT INTO {table}(rowid, {cols}) VALUES ({marks})", row)
    db.commit()
    return db


def words(text):
    """The words of a query as FTS5 sees them, without stopwords."""
    return [w for w in re.findall(r"[^\W_]+", fold(text)) if w not in STOPWORDS]


def top(db, table, match, grouped):
    if not match:
        return []
    rank = f"bm25({table}, 1.0, 0.3)" if grouped else f"bm25({table})"
    rows = db.execute(f"SELECT rowid FROM {table} WHERE {table} MATCH ? ORDER BY {rank}, rowid LIMIT 10", (match,))
    return [r[0] for r in rows]


def r1(text):
    w = words(text)
    return " OR ".join(f'"{x}"' + ("*" if n == len(w) - 1 else "") for n, x in enumerate(w))


def r2(text):
    grams = []
    for w in words(text):
        for n in range(len(w) - 2):
            if w[n : n + 3] not in grams:
                grams.append(w[n : n + 3])
    return " OR ".join(f'"{g}"' for g in grams)


def main():
    c = Corpus()
    queries = read_queries()
    dbs = {"A": build(c, DB / "names.sqlite", False), "B": build(c, DB / "groups.sqlite", True)}

    n = 0
    with open(DATA / "reference.tsv", "w", encoding="utf-8") as f:
        f.write("sistema\tbase\tconsulta\ttop10\n")
        for base in ("A", "B"):
            for q in queries:
                if base not in q["bases"]:
                    continue
                for system, build_match, table in (("R1", r1, "fts"), ("R2", r2, "tri")):
                    ids = top(dbs[base], table, build_match(q["text"]), base == "B")
                    f.write(f"{system}\t{base}\t{q['id']}\t{' '.join(map(str, ids))}\n")
                    n += 1
    print(n, "rankings")


main()
