#!/usr/bin/env python3
"""The 200 queries -> genix-s3/testdata/bench/queries.tsv.

    python3 scripts/bench/queries.py

Fixed seed. The result is reviewed by hand once and then frozen: the file is
versioned and a query is never regenerated (if it changes, it gets a new id),
so run this only to start a new set.

Columns: id, categoria, consulta, objetivo (a product id, empty when there is
none), bases (A = names, B = names with groups).
"""
import random
import re

from common import DATA, STOPWORDS, Corpus, correct, edit1, fold, normalize, raw_words, tokens
from judge import judge

SEED = 20261005
rng = random.Random(SEED)
c = Corpus()

# Product ids in a fixed random order, and the targets already used.
order = list(c.ids)
rng.shuffle(order)
used = set()
queries = []  # (categoria, consulta, objetivo, bases)


def content(i):
    """The words of a name that stand for one word of the index, as typed:
    (raw, normalized), without stopwords, numbers or one-letter words."""
    out = []
    for w in raw_words(c.name[i]):
        t = normalize(w)
        if len(t) == 1 and len(t[0]) >= 2 and not t[0].isdigit() and t[0] not in STOPWORDS:
            out.append((w, t[0]))
    return out


def window(words, k):
    start = rng.randrange(len(words) - k + 1)
    return words[start : start + k]


def take(category, n, bases, build, objective=True):
    """Calls build(product id) over the products in order until n queries pass."""
    got = 0
    for i in order:
        if i in used and objective:
            continue
        q = build(i)
        if q is None:
            continue
        text, target = q
        if any(text == x[1] for x in queries):
            continue
        queries.append((category, text, target, bases))
        if objective:
            used.add(i)
        got += 1
        if got == n:
            return
    raise SystemExit(f"{category}: only {got} of {n} queries")


def exacta(i):
    words = content(i)
    if len(words) < 3:
        return None
    w = window(words, rng.choice((2, 3)))
    if len(c.matching([t for _, t in w])) > 5:
        return None
    return " ".join(r for r, _ in w), i


def parcial(i, skipped):
    words = content(i)
    if len(words) < 4:
        return None
    k = rng.choice((2, 3))
    idx = sorted(rng.sample(range(len(words)), k))
    if skipped and idx[-1] - idx[0] == k - 1:
        return None  # it must skip a word
    w = [words[j] for j in idx]
    if not skipped:
        rng.shuffle(w)
        if w == [words[j] for j in idx]:
            return None  # it must change the order
    if len(c.matching([t for _, t in w])) > 5:
        return None
    return " ".join(r for r, _ in w), i


def escribiendo(i):
    words = content(i)
    if len(words) < 3:
        return None
    w = window(words, rng.choice((2, 3)))
    raw, tok = w[-1]
    if len(raw) < 5 or not raw.isalpha():
        return None
    cut = raw[: rng.randint(3, min(5, len(raw) - 1))]
    text = " ".join([r for r, _ in w[:-1]] + [cut])
    words_q = tokens(text)
    if len(words_q) != len(w) or len(c.matching(words_q, prefix_last=True)) > 10:
        return None
    return text, i


def typo_of(word, pos):
    """One edit of the word (ASCII, no accents) at position pos."""
    word = fold(word)
    kind = rng.choice(("swap", "delete", "insert", "substitute"))
    if kind == "swap" and pos + 1 < len(word):
        return word[:pos] + word[pos + 1] + word[pos] + word[pos + 2 :]
    if kind == "delete":
        return word[:pos] + word[pos + 1 :]
    letter = rng.choice("abcdefghijklmnopqrstuvwxyz")
    if kind == "insert":
        return word[:pos] + letter + word[pos:]
    return word[:pos] + letter + word[pos + 1 :]


def typo(i, early):
    words = content(i)
    if len(words) < 3:
        return None
    w = window(words, rng.choice((2, 3)))
    j = rng.randrange(len(w))
    raw, tok = w[j]
    if len(raw) < 5 or not raw.isalpha():
        return None
    pos = rng.choice((0, 1)) if early else rng.randrange(2, len(raw) - 1)
    bad = typo_of(raw, pos)
    badt = tokens(bad)
    if len(badt) != 1 or badt[0] == tok or badt[0] in c.by_name or any(badt[0] == t for _, t in w):
        return None
    if not edit1(badt[0], tok):
        return None  # the cut of repeated letters moved it away
    text = " ".join(bad if n == j else r for n, (r, _) in enumerate(w))
    fixed = correct(c, i, tokens(text))
    if fixed is None or len(c.matching(fixed)) > 5:
        return None
    return text, i


def accents(word):
    return fold(word) != word


def tildes(i, kind):
    words = content(i)
    if len(words) < 3:
        return None
    w = window(words, rng.choice((2, 3)))
    raws = [r for r, _ in w]
    if kind in ("strip", "upper") and not any(accents(r) for r in raws):
        return None
    if kind == "strip":
        text = " ".join(fold(r) for r in raws)
    elif kind == "upper":
        text = " ".join(fold(r) for r in raws).upper() if rng.random() < 0.6 else " ".join(r.capitalize() for r in raws)
    else:  # an accent that is not there
        plain = [n for n, r in enumerate(raws) if not accents(r) and re.search("[aeiou]", r)]
        if not plain:
            return None
        n = rng.choice(plain)
        spots = [m.start() for m in re.finditer("[aeiou]", raws[n])]
        p = rng.choice(spots)
        raws[n] = raws[n][:p] + "áéíóú"["aeiou".index(raws[n][p])] + raws[n][p + 1 :]
        text = " ".join(raws)
    if len(c.matching(tokens(text))) > 5:
        return None
    return text, i


def marca(i):
    brand = raw_words(c.brand[i])
    btok = tokens(c.brand[i])
    if not btok or not btok[0].isalpha() or not set(btok) <= c.nset[i]:
        return None
    kind = [t for _, t in content(i) if t not in btok]
    if len(kind) < 2:
        return None
    k = rng.choice((1, 2))
    words = brand + kind[:k]
    n = len(c.matching(tokens(" ".join(words))))
    if not 3 <= n <= 60:
        return None
    return " ".join(words), None


NUMBER = re.compile(r"(\d+(?:[.,]\d+)?)\s*(kg|gr|g|ml|cl|l|lt|ud|uds|mg|cm|mm|m)\b\.?", re.I)
DECIMAL = re.compile(r"\b(\d+,\d+)\s*º?")


def numeros(i, decimal):
    words = [r for r, _ in content(i)][:2]
    if len(words) < 2:
        return None
    if decimal:
        m = DECIMAL.search(c.name[i])
        if not m:
            return None
        tail = m.group(1)
    else:
        m = NUMBER.search(c.name[i])
        if not m:
            return None
        number, unit = m.group(1), m.group(2).lower()
        tail = f"{number}{unit}" if rng.random() < 0.25 else f"{number} {unit}"
    text = " ".join(words + [tail])
    if len(c.matching(tokens(text))) > 6:
        return None
    return text, i


def nombre_grupo(i, overlap):
    cat = tokens(c.cat[i])
    if not 1 <= len(cat) <= 4:
        return None
    if overlap != bool(set(cat) & c.nset[i]):
        return None
    words = [(r, t) for r, t in content(i) if t not in cat and len(t) >= 3]
    if not words:
        return None
    w = rng.sample(words, min(len(words), rng.choice((1, 2))))
    if len(c.matching([t for _, t in w])) <= 10:
        return None  # alone, the name words must be ambiguous
    text = " ".join([r for r, _ in w] + raw_words(c.cat[i]))
    q = dict(id="", cat="nombre+grupo", text=text, target=i)
    relevant = [d for d, g in judge(c, q).items() if g >= 2]
    if not 1 <= len(relevant) <= 4:
        return None
    return text, i


def grupos():
    """15 categories of up to 4 words: 5 small (2-4 members), 5 medium (5-25), 5 big."""
    pools = {"small": [], "medium": [], "big": []}
    for cat, members in c.members.items():
        words = tokens(cat)
        if not 1 <= len(words) <= 4 or sum(1 for k in c.members if tokens(k) == words) != 1:
            continue
        n = len(members)
        if 2 <= n <= 4:
            pools["small"].append(cat)
        elif 5 <= n <= 25:
            pools["medium"].append(cat)
        elif n > 25:
            pools["big"].append(cat)
    for size in ("small", "medium", "big"):
        for cat in rng.sample(sorted(pools[size]), 5):
            queries.append(("grupo", " ".join(raw_words(cat)), None, "B"))


NEGATIVES = [
    "producto premium",
    "smartphone",
    "bateria litio",
    "zapatillas running",
    "cuaderno universitario",
    "quirofano",
    "xilofono",
]


def negativas():
    """Words that are not in the corpus, in any way a query could reach them (a
    word, or a word that starts with it), and made-up words that start like a
    category."""
    cats = sorted({tokens(k)[0] for k in c.members if tokens(k)})
    made = []
    for cat in rng.sample(cats, 40):
        word = cat[:2] + rng.choice(("xq", "zk", "qw", "vx")) + cat[2:5]
        made.append(word)
    for text in NEGATIVES + made:
        words = tokens(text)
        if any(c.docs(w, prefix=True, group=True) for w in words):
            continue
        queries.append(("negativa", text, None, "AB"))
        if sum(1 for q in queries if q[0] == "negativa") == 10:
            return
    raise SystemExit("negativa: not enough")


def main():
    take("exacta", 35, "AB", exacta)
    take("parcial", 10, "AB", lambda i: parcial(i, True))
    take("parcial", 10, "AB", lambda i: parcial(i, False))
    take("escribiendo", 25, "AB", escribiendo)
    take("typo", 6, "AB", lambda i: typo(i, True))
    take("typo", 19, "AB", lambda i: typo(i, False))
    take("tildes", 6, "AB", lambda i: tildes(i, "strip"))
    take("tildes", 4, "AB", lambda i: tildes(i, "upper"))
    take("tildes", 5, "AB", lambda i: tildes(i, "extra"))
    take("marca", 15, "AB", marca, objective=False)
    take("numeros", 11, "AB", lambda i: numeros(i, False))
    take("numeros", 4, "AB", lambda i: numeros(i, True))
    take("nombre+grupo", 5, "AB", lambda i: nombre_grupo(i, True))
    take("nombre+grupo", 20, "AB", lambda i: nombre_grupo(i, False))
    grupos()
    negativas()

    queries.sort(key=lambda q: ["exacta", "parcial", "escribiendo", "typo", "tildes", "marca", "numeros", "nombre+grupo", "grupo", "negativa"].index(q[0]))
    with open(DATA / "queries.tsv", "w", encoding="utf-8") as f:
        f.write("id\tcategoria\tconsulta\tobjetivo\tbases\n")
        for n, (cat, text, target, bases) in enumerate(queries, 1):
            f.write(f"q{n:03d}\t{cat}\t{text}\t{target if target else ''}\t{bases}\n")
    print(len(queries), "queries")


main()
