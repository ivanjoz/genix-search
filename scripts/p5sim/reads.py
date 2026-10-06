# Requests and bytes READ per query (run from the repo root).
# IDs are arbitrary uint32 (random), never dense. 3000 queries of 2-3 words taken from catalogue docs.
import sys, os, random, collections, math
sys.path.insert(0, os.path.dirname(__file__))
from norm import docs
import re

D = [d for d in docs() if d]
N = len(D)
rnd = random.Random(42)
IDS = rnd.sample(range(1, 2**32), N)                      # arbitrary client ids

def toks(d): return [p for t in d.split() for p in re.findall(r'[a-z]+|[0-9]+', t)]
def key(t): return '#' + str(int(t)) if t.isdigit() else t[:8]
DT = [[key(t) for t in toks(d)] for d in D]               # document as 8-letter keys, in order
LONG = [[len(t) > 8 and not t.isdigit() for t in toks(d)] for d in D]

post = collections.defaultdict(list)                       # key -> [(id, pos)]
for i, ks in enumerate(DT):
    for p, k in enumerate(ks): post[k].append((IDS[i], min(p, 15), i))

# ---- size of one posting list (INTERNALS.md §7.1), first ID in the dictionary
def nib(v):
    n = 1
    while v >= 8: v >>= 3; n += 1
    return n
def plist_bytes(k):
    r = sorted(post[k]); n = len(r)
    if n == 1: return 0                                     # inline in the dictionary
    g = [b[0] - a[0] for a, b in zip(r, r[1:])]
    gb = min((n - 1) * max(x.bit_length() for x in g), 4 * sum(nib(x) for x in g))
    return (gb + 4 * n + 7) // 8

# ---- size of a document record embedded in a group: ID 32 + token count 5 + tokens (len unit + units) + 2-bit tails
def tok_bits(k): return 5 * (1 + (len(str(int(k[1:]))) * 0 + max(1, math.ceil(int(k[1:]).bit_length() / 5)) if k[0] == '#' else len(k)))
REC = [32 + 5 + sum(tok_bits(k) for k in ks) + 2 * sum(LONG[i]) for i, ks in enumerate(DT)]
def group_bytes(k):
    docs_ = {i for _, _, i in post[k]}
    return (sum(REC[i] for i in docs_) + 7) // 8
GB = {k: group_bytes(k) for k in post}
DF = {k: len({i for _, _, i in v}) for k, v in post.items()}

# ---- ranking: coverage desc, adjacent query pairs desc, first-position bonus desc, id asc
def score(i, q):
    ks = DT[i]; present = [t for t in q if t in ks]
    adj = sum(1 for a, b in zip(ks, ks[1:]) if a in q and b in q and a != b)
    first = 1 if ks and ks[0] in q else 0
    return (-len(present), -adj, -first, IDS[i])
def topk(cands, q, K=10): return [i for i in sorted(cands, key=lambda i: score(i, q))[:K]]

queries = []
while len(queries) < 3000:
    ks = list(dict.fromkeys(k for k in DT[rnd.randrange(N)] if k[0] != '#' and len(k) >= 3))
    if len(ks) < 2: continue
    queries.append(rnd.sample(ks, rnd.choice((2, 3)) if len(ks) >= 3 else 2))

res = collections.defaultdict(list)
for q in queries:
    exact = topk({i for k in q for _, _, i in post[k]}, q)
    # A: one posting list per term
    res['A_req'].append(sum(1 for k in q if DF[k] > 1 or len(post[k]) > 1))
    res['A_bytes'].append(sum(plist_bytes(k) for k in q))
    # B: embedded documents, only the rarest term's group;  C: the two rarest
    by = sorted(q, key=lambda k: GB[k])
    for name, r in (('B', 1), ('C', 2)):
        sel = by[:r]
        cands = {i for k in sel for _, _, i in post[k]}
        res[name + '_bytes'].append(sum(GB[k] for k in sel))
        res[name + '_req'].append(len(sel))
        res[name + '_inl'].append(sum(1 for k in sel if GB[k] > 64))    # groups <= 64 B inlined in the dictionary
        res[name + '_same'].append(topk(cands, q) == exact)

def pct(v, p): v = sorted(v); return v[min(len(v) - 1, int(p * len(v)))]
print(f"{len(queries)} queries, {N} docs, random uint32 ids\n")
print(f"{'design':58s} {'requests avg':>12s} {'bytes avg':>10s} {'p50':>7s} {'p99':>7s} {'max':>8s} {'top-10 = exact':>15s}")
for name, label in (('A', 'A  postings: one list per term'), ('B', 'B  document in the group: rarest term only'), ('C', 'C  document in the group: two rarest terms')):
    req = res[name + '_req']; b = res[name + '_bytes']
    same = f"{100*sum(res[name+'_same'])/len(queries):.1f}%" if name != 'A' else 'exact'
    print(f"{label:58s} {sum(req)/len(req):12.2f} {sum(b)/len(b):10.0f} {pct(b,.5):7d} {pct(b,.99):7d} {max(b):8d} {same:>15s}")
    if name != 'A':
        inl = res[name + '_inl']
        print(f"{'   + groups <= 64 B inlined in the dictionary':58s} {sum(inl)/len(inl):12.2f}")
print(f"\nstorage: postings {sum(plist_bytes(k) for k in post)/1024:.0f} KB, groups with documents {sum(GB.values())/1024:.0f} KB; avg document record {sum(REC)/len(REC)/8:.1f} B")

# ---- how often do full-coverage documents alone fill the top-K (B is exact for them by construction)
full = []
for q in queries:
    full.append(len(set.intersection(*[{i for _, _, i in post[k]} for k in q])))
for K in (1, 5, 10):
    print(f"queries with >= {K:2d} documents containing every term: {100*sum(f >= K for f in full)/len(full):5.1f}%")

# ---- last word typed partially (auto-prefix): A reads the prefix range, B evaluates it on the candidates
skeys = sorted(post)
import bisect
def prefix_keys(p):
    lo = bisect.bisect_left(skeys, p); hi = bisect.bisect_left(skeys, p + '{')
    return skeys[lo:hi]
ra = collections.defaultdict(list)
for q in queries:
    last = q[-1]; cut = last[:rnd.randint(3, len(last))] if len(last) > 3 else last
    done = q[:-1]
    pk = prefix_keys(cut)
    ra['A_req'].append(sum(1 for k in done if len(post[k]) > 1) + (1 if any(len(post[k]) > 1 for k in pk) else 0))
    ra['A_bytes'].append(sum(plist_bytes(k) for k in done) + sum(plist_bytes(k) for k in pk))
    anchor = min(done, key=lambda k: GB[k])
    ra['B_req'].append(1); ra['B_bytes'].append(GB[anchor])
print("\nlast word as a prefix (cut to 3+ letters):")
for name, label in (('A', 'A  postings: one list per term + prefix range'), ('B', 'B  document in the group: rarest complete term, prefix checked on candidates')):
    req = ra[name + '_req']; b = ra[name + '_bytes']
    print(f"  {label:78s} requests {sum(req)/len(req):5.2f}  bytes avg {sum(b)/len(b):7.0f}  p99 {pct(b,.99):7d}")
