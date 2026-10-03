# Postings and dictionary sizes for PLAN_5BIT.md section 5.1-6.1 (run from the repo root).
import sys, json, random; sys.argv = ['x']
g = {'__file__': __import__('os').path.join(__import__('os').path.dirname(__file__), 'search.py')}
exec(open(__import__('os').path.join(__import__('os').path.dirname(__file__), 'search.py')).read().replace('print(', '(lambda *a, **k: None)('), g)
by_slot, key_post, nb, slot_units = g['by_slot'], g['key_post'], g['nb'], g['slot_units']
words = g['words']; tail = g['tail']
# rows per key: (id, pos, tail)
rows = {}
for k in key_post:
    if k.startswith('#'): continue
for w in words:
    k = w[:8]; rows.setdefault(k, []).extend((d, min(p, 15), tail(w)) for d, p in g['post'][w])
for s in g['nums']: rows['#' + s] = [(d, min(p, 15), 0) for d, p in g['post'][s]]
def nib(v):
    n = 1
    while v >= 8: v >>= 3; n += 1
    return n
def measure(m, label):
    p_nib = p_fixed = 0; kb = 0
    slots = {}
    for k, r in rows.items():
        sk = ('#' + str(len(k))) if k.startswith('#') else (k[:2] if len(k) >= 2 else k + '_')
        slots.setdefault(sk, []).append(k)
    for sk, ks in slots.items():
        firsts, cnts, gn, tails = [], [], [], []
        for k in ks:
            r = sorted((m[d], p, t) for d, p, t in rows[k]); ids = [x[0] for x in r]
            gaps = [b - a for a, b in zip(ids, ids[1:])]
            T = any(x[2] for x in r); n = len(r)
            firsts.append(ids[0]); cnts.append(n); tails.append(T)
            G = sum(nib(x) for x in gaps); gn.append(G)
            if n > 1:
                p_nib += (4*G + 4*n + (2*n if T else 0) + 7) // 8
                w = max([x.bit_length() for x in gaps] or [0])
                p_fixed += ((n-1)*w + 4*n + (2*n if T else 0) + 7) // 8
        base = min(firsts)
        wc = max(c.bit_length() for c in cnts); wf = max((f - base).bit_length() for f in firsts)
        wl = max(5, max(x.bit_length() for x in gn))
        n = len(ks)
        if sk.startswith('#'):
            keyb = (sum(int(sk[1:]) for _ in ks) * 0)  # number keys measured separately below
        fields = (n * (wc + 1 + wl + wf) + 7) // 8
        kb += 8 + (n - 1) // 16 * 2 + fields
    keys = sum(((slot_units(r, 16)*5 + 7)//8) for r in by_slot.values()) + 512   # + ~0.5 KB number keys
    kt = 8264 + kb + keys + 4 * ((p_nib + 4095) // 4096)
    print(f"{label:16s} p: varint nibbles {p_nib/1024:6.1f} KB | ancho único por lista {p_fixed/1024:6.1f} KB    k: {kt/1024:5.1f} KB (campos {kb/1024:.1f} KB, claves {keys/1024:.1f} KB)")
measure(list(range(10010)), "orden de nombre")
perm = list(range(10010)); random.Random(7).shuffle(perm)
measure(perm, "mezclados")

def per_list(m, label):
    tot = {'fixed': 0, 'nib': 0, 'best': 0}; picked = 0; n_lists = 0
    for k, r0 in rows.items():
        r = sorted((m[d], p, t) for d, p, t in r0); n = len(r)
        if n < 2: continue
        ids = [x[0] for x in r]; gaps = [b - a for a, b in zip(ids, ids[1:])]
        T = any(x[2] for x in r); rest = 4*n + (2*n if T else 0)
        f = ((n-1) * max(x.bit_length() for x in gaps) + rest + 7) // 8
        v = (4 * sum(nib(x) for x in gaps) + rest + 7) // 8
        tot['fixed'] += f; tot['nib'] += v; tot['best'] += min(f, v); picked += v < f; n_lists += 1
    print(f"{label:16s} ancho único {tot['fixed']/1024:6.1f} KB | nibbles {tot['nib']/1024:6.1f} KB | el menor por lista {tot['best']/1024:6.1f} KB  (nibbles elegido en {picked} de {n_lists} listas)")
print()
per_list(list(range(10010)), "orden de nombre")
per_list(perm, "mezclados")
