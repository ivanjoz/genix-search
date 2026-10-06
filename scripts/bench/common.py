"""Shared by the scripts of PLAN_BENCH.md: the corpus, our normalization (a port
of genix-s3/internal/normalize, so that the judge and the engine read text the
same way), the inverted index the generators and the judge use, and the paths.
"""
import re
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GXS = ROOT / "genix-s3"
DATA = GXS / "testdata/bench"
DB = GXS / "bench/db"

STOPWORDS = set(
    "a al con de del desde el en es esa ese eso esos esta esto fue ha la las le les "
    "lo los me mi mis ni no nos o para por que se ser si son soy su sus tu y".split()
)

_ACCENTS = {}
for _chars, _base in (("áàäâ", "a"), ("éèëê", "e"), ("íìïî", "i"), ("óòöô", "o"), ("úùüû", "u"), ("ñ", "n")):
    for _c in _chars:
        _ACCENTS[_c] = _base
        _ACCENTS[_c.upper()] = _base


def _char(c):
    """normalizeChar: a letter or digit, lowercase and without accents; '' for a separator."""
    if "a" <= c <= "z" or "0" <= c <= "9":
        return c
    if "A" <= c <= "Z":
        return c.lower()
    return _ACCENTS.get(c, "")


def normalize(text, stopwords=False):
    """Normalizer.Normalize (stopwords=False) or Normalizer.Tokens (stopwords=True: dropped)."""
    tokens, cur = [], []
    last, digits, run = "", False, 0

    def flush():
        nonlocal last
        if cur:
            tokens.append("".join(cur))
            cur.clear()
        last = ""

    for source in text:
        c = _char(source)
        if c == "":
            flush()
        elif c.isdigit():
            if not digits:
                flush()
                digits = True
            cur.append(c)
        else:
            if digits:
                flush()
                digits = False
            if c == last:
                run += 1
            else:
                last, run = c, 1
            if run <= (2 if c == "l" else 1):
                cur.append(c)
    flush()

    if stopwords:
        tokens = [t for t in tokens if t not in STOPWORDS]
    return tokens


def tokens(text):
    """What the engine indexes and matches: Normalizer.Tokens."""
    return normalize(text, stopwords=True)


def is_number(tok):
    return tok[:1].isdigit()


def fold(text):
    """Lowercase and without accents, keeping every other character: for the raw
    text a reference engine sees."""
    return "".join(_ACCENTS.get(c, c.lower()) for c in text)


def raw_words(text):
    """Words as typed: runs of letters and digits, lowercase, accents kept."""
    return [w.lower() for w in re.findall(r"[^\W_]+", text)]


def edit1(a, b):
    """Whether a and b are one Damerau edit apart (not equal)."""
    if a == b:
        return False
    la, lb = len(a), len(b)
    if abs(la - lb) > 1:
        return False
    if la == lb:
        diff = [i for i in range(la) if a[i] != b[i]]
        if len(diff) == 1:
            return True
        return len(diff) == 2 and diff[1] == diff[0] + 1 and a[diff[0]] == b[diff[1]] and a[diff[1]] == b[diff[0]]
    if la > lb:
        a, b = b, a
    for i in range(len(b)):
        if a[:i] == b[:i] and a[i:] == b[i + 1 :]:
            return True
    return False


class Corpus:
    """The products, and the inverted index of their normalized words."""

    def __init__(self):
        rows = [l.rstrip("\n").split("\t") for l in open(DATA / "productos.tsv", encoding="utf-8")][1:]
        self.ids = [int(r[0]) for r in rows]
        self.name = {int(r[0]): r[1] for r in rows}
        self.cat = {int(r[0]): r[2] for r in rows}
        self.brand = {int(r[0]): r[3] for r in rows}
        self.ntok = {i: tokens(self.name[i]) for i in self.ids}
        self.ctok = {i: tokens(self.cat[i]) for i in self.ids}
        self.nset = {i: set(t) for i, t in self.ntok.items()}
        self.cset = {i: set(t) for i, t in self.ctok.items()}
        self.norm_name = {i: " ".join(self.ntok[i]) for i in self.ids}

        self.by_name = defaultdict(set)  # token -> docs with it in the name
        self.by_any = defaultdict(set)  # token -> docs with it in the name or the category
        for i in self.ids:
            for t in self.nset[i]:
                self.by_name[t].add(i)
                self.by_any[t].add(i)
            for t in self.cset[i]:
                self.by_any[t].add(i)
        self.vocab = sorted(self.by_name)
        self.members = defaultdict(list)
        for i in self.ids:
            self.members[self.cat[i]].append(i)

    def docs(self, tok, prefix=False, group=False):
        """The docs with the word tok (or a word that starts with it) in the name,
        or in the name or the category with group."""
        index = self.by_any if group else self.by_name
        if not prefix:
            return index.get(tok, set())
        out = set()
        for v in self.vocab if not group else index:
            if v.startswith(tok):
                out |= index[v]
        return out

    def matching(self, words, prefix_last=False, group=False):
        """The docs that have every word: complete, the last one maybe as a prefix."""
        out = None
        for n, w in enumerate(words):
            found = self.docs(w, prefix=prefix_last and n == len(words) - 1, group=group)
            out = found if out is None else out & found
            if not out:
                return set()
        return out or set()


def number_swap(a, b):
    """Whether a and b are the same word in singular and plural, as the queries
    of `plural` write it: one is the other with "s" or "es" at the end."""
    short, long = sorted((a, b), key=len)
    return len(short) >= 3 and long in (short + "s", short + "es")


def correct(corpus, target, query_tokens, related=edit1):
    """The query with its one word that is not in the target's name replaced by
    the target's word it is related to (a typo, or singular and plural), or None
    when that is not unique (the judge and the generator must agree on it)."""
    bad = [t for t in query_tokens if t not in corpus.nset[target]]
    if len(bad) != 1:
        return None
    options = [t for t in corpus.nset[target] if related(bad[0], t)]
    if len(options) != 1:
        return None
    return [options[0] if t == bad[0] else t for t in query_tokens]


def read_queries():
    rows = [l.rstrip("\n").split("\t") for l in open(DATA / "queries.tsv", encoding="utf-8")][1:]
    return [dict(id=r[0], cat=r[1], text=r[2], target=int(r[3]) if r[3] else None, bases=r[4]) for r in rows]
