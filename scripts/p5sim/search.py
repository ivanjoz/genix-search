# Measurements for the search-oriented 5-bit layout (PLAN_5BIT.md).
import sys, os, re, math, collections
sys.path.insert(0, os.path.dirname(__file__))
from norm import docs
D = [d for d in docs() if d]
N = len(D)

def split_mixed(tok):  # "50ml" -> ["50","ml"]: letters and digits never share a token
    return re.findall(r'[a-z]+|[0-9]+', tok)

docs_toks = [[p for t in d.split() for p in split_mixed(t)] for d in D]
words = collections.Counter(); nums = collections.Counter()
post = collections.defaultdict(list)  # token -> [(doc, pos)]
for i, ts in enumerate(docs_toks):
    for p, t in enumerate(ts):
        (nums if t.isdigit() else words)[t] += 1
        post[t].append((i, p))
print(f"docs {N}  tokens {sum(map(len,docs_toks))}  letter tokens {sum(words.values())}  number tokens {sum(nums.values())}")
print(f"vocab: words {len(words)}  numbers {len(nums)}")

# word length (in letters) vs what fits in a u64 key
L = collections.Counter(len(w) for w in words.elements())
tot = sum(L.values())
for k in (8, 10, 12, 14):
    print(f"  tokens with <= {k:2d} letters: {100*sum(v for l,v in L.items() if l<=k)/tot:6.2f}%   distinct words: {100*sum(1 for w in words if len(w)<=k)/len(words):6.2f}%")
print("  longest:", sorted(words, key=len)[-5:])

# numbers: decimal nibbles vs binary nibbles, 4 bits + continue bit per unit
def units_dec(s): return len(s)
def units_bin(s):
    v = int(s); u = 1
    while v >= 16: v >>= 4; u += 1
    return u
ud = sum(units_dec(s)*c for s,c in nums.items()); ub = sum(units_bin(s)*c for s,c in nums.items())
lz = sum(c for s,c in nums.items() if len(s)>1 and s[0]=='0')
print(f"number units: decimal {ud}  binary {ub}  ({100*(ub-ud)/ud:+.1f}%)   leading-zero tokens {lz}")
print("  numbers > 4 digits:", sum(c for s,c in nums.items() if len(s)>4))

# initial-bigram slots and posting sizes
def est_bytes(n):  # postings: delta-coded id + position, bitpacked (estimate)
    gap = max(1, N/n)
    return n*(math.log2(gap)+2+4)/8
slots = collections.defaultdict(lambda: [0,0,0])  # words, postings, bytes
for w in words:
    s = slots[w[:2] if len(w)>=2 else w+'_']
    n = len(post[w]); s[0]+=1; s[1]+=n; s[2]+=est_bytes(n)+10
print(f"letter slots used: {len(slots)} of 26*27={26*27}")
b = sorted(v[2] for v in slots.values())
print(f"  slot size est. (KB) median {b[len(b)//2]/1024:.2f}  p90 {b[int(len(b)*.9)]/1024:.2f}  p99 {b[int(len(b)*.99)]/1024:.2f}  max {b[-1]/1024:.2f}  total {sum(b)/1024:.0f}")
big = sorted(slots.items(), key=lambda x:-x[1][2])[:6]
print("  biggest:", [(k, v[0], v[1], round(v[2]/1024,1)) for k,v in big])
w_per = sorted(v[0] for v in slots.values())
print(f"  words per slot median {w_per[len(w_per)//2]}  max {w_per[-1]}")
nb = sum(est_bytes(len(post[n]))+6 for n in nums)
print(f"number postings est. {nb/1024:.1f} KB in {len(nums)} values")
# position stats: how often a token's position is > 3, > 15
pos = [p for ts in docs_toks for p in range(len(ts))]
print(f"positions: <4 {100*sum(p<4 for p in pos)/len(pos):.1f}%  <16 {100*sum(p<16 for p in pos)/len(pos):.1f}%  max {max(pos)}")

# --- numbers: user's varint (NUM marker + 4-bit nibble + continue bit) vs length-prefixed base 32
def units_b32(s):
    v = int(s); u = 1
    while v >= 32: v >>= 5; u += 1
    return u
tot_var = sum((1+units_bin(s))*c for s,c in nums.items())
tot_b32 = sum((1+units_b32(s))*c for s,c in nums.items())   # NUMk code + k base-32 digits
tot_dec = sum((1+len(s))*c for s,c in nums.items())
print(f"number token units (incl. marker): decimal {tot_dec}  varint-nibble {tot_var}  len-prefixed base32 {tot_b32}")
print("  base32 digit count dist:", collections.Counter(units_b32(s) for s in nums.elements()))
print("  max value", max(int(s) for s in nums))

# --- u64 keys: 12 units of 5 bits; collisions when a word is cut at 12 letters
long = [w for w in words if len(w) > 12]
cut = collections.defaultdict(set)
for w in long: cut[w[:12]].add(w)
coll = {k:v for k,v in cut.items() if len(v)>1}
print(f"words > 12 letters: {len(long)}; groups that collide when cut: {len(coll)} ({sum(len(v) for v in coll.values())} words)")
print("  e.g.", list(coll.values())[:4])
import zlib
coll4 = 0
for k,v in coll.items():
    hs = collections.Counter(1 + zlib.crc32(w[12:].encode()) % 15 for w in v)
    coll4 += sum(c for c in hs.values() if c>1)
print(f"  still colliding with a 4-bit tail hash in the low bits: {coll4} words")

# --- postings: fixed u32 (id 28 + pos 4) vs bitpacked estimate
P = sum(len(v) for v in post.values())
print(f"postings {P}: fixed u32 {P*4/1024:.0f} KB; bitpacked est. {sum(est_bytes(len(v)) for v in post.values())/1024:.0f} KB")
# --- dictionary heads: sorted u64 keys, delta + bitpack per slot (estimate) + off/df columns
def key(w):
    k = 0
    for i in range(12):
        c = (ord(w[i]) - 96) if i < len(w) else 0
        k = (k << 5) | c
    return k << 4
head = 0
for s in set(w[:2] for w in words):
    ks = sorted(key(w) for w in words if w[:2] == s)
    d = [b-a for a,b in zip(ks, ks[1:])] or [0]
    head += len(ks)*(max(d).bit_length() + 2*12)/8 + 16   # key delta + off/df at ~12 bits each
print(f"dictionary heads (all slots) est. {head/1024:.1f} KB for {len(words)} words; raw u64 keys would be {len(words)*8/1024:.0f} KB")

# --- 8-letter keys: 8 units x 5 bits = 40 bits = 5 bytes
cut8 = collections.defaultdict(set)
for w in words: cut8[w[:8]].add(w)
merged = {k: v for k, v in cut8.items() if len(v) > 1}
nm = sum(len(v) for v in merged.values())
print(f"\n8-letter keys: {len(cut8)} distinct keys from {len(words)} words; {len(merged)} keys merge {nm} words")
# merges that are only inflection (same stem + typical Spanish endings) vs different words
ends = ('', 's', 'es', 'a', 'as', 'o', 'os', 'e', 'es')
def same_family(ws):
    ws = sorted(ws, key=len); base = ws[0]
    return all(w[:len(base)] == base or w[:-1] == base[:-1] or w[:-2] == base[:-2] for w in ws)
fam = [v for v in merged.values() if all(len(w) <= 10 for w in v) and same_family(v)]
print(f"  merges that look like inflection (stem + short ending): {len(fam)} of {len(merged)}")
docs_aff = collections.Counter()
for k, v in merged.items():
    docs_aff[k] = len({d for w in v for d, _ in post[w]})
print("  biggest merges:", [(k, sorted(merged[k])[:5], docs_aff[k]) for k, _ in docs_aff.most_common(8)])
other = [sorted(v) for v in merged.values() if not same_family(v)]
print("  sample of non-inflection merges:", other[:12])
toks_long = sum(c for w, c in words.items() if len(w) > 8)
print(f"  tokens longer than 8 letters: {toks_long} ({100*toks_long/sum(words.values()):.1f}%)")
print(f"  dictionary: {len(cut8)+len(nums)} keys x 5 B = {(len(cut8)+len(nums))*5/1024:.0f} KB keys; refs x 12 B = {(len(cut8)+len(nums))*12/1024:.0f} KB")
def stem(w): return re.sub(r'(es|as|os|s|a|o|e)$', '', w)
real = {k: sorted(v) for k, v in merged.items() if len({stem(w) for w in v}) > 1}
print(f"  merges with more than one stem (after dropping a/o/e/s/es endings): {len(real)} of {len(merged)}; docs touched {sum(docs_aff[k] for k in real)}")
print("  examples:", [v[:4] for v in list(real.values())[:20]])

# --- dictionary keys: 8 letters max, front-coded inside each slot on the 5-bit grid.
# One header unit encodes (shared letters after the slot, total letters after the slot):
# pairs s <= L <= 6 are 28 combinations, so they fit in one 5-bit unit.
keys8 = sorted({w[:8] for w in words})
by_slot = collections.defaultdict(list)
for k in keys8: by_slot[k[:2] if len(k) >= 2 else k + '_'].append(k[2:])
def slot_units(rest, restart):
    u = 0; prev = None
    for i, r in enumerate(rest):
        s = 0
        if prev is not None and not (restart and i % restart == 0):
            while s < min(len(r), len(prev)) and r[s] == prev[s]: s += 1
        u += 1 + len(r) - s; prev = r
    return u
for restart in (0, 16):
    bits = sum(((slot_units(r, restart)*5 + 7)//8)*8 for r in by_slot.values())
    print(f"keys front-coded per slot, restart {restart or 'none'}: {bits/8/1024:.1f} KB  {bits/len(keys8):.1f} bits/key")
print(f"  fixed 40-bit for comparison: {len(keys8)*5/1024:.1f} KB")
# numbers: slot (NUM, k) with k base-32 digits; entries front-coded the same way
nb = collections.defaultdict(list)
for s in nums:
    v = int(s); d = []
    while True:
        d.append(v & 31); v >>= 5
        if not v: break
    nb[len(d)].append(tuple(reversed(d)))
nu = sum(slot_units(sorted(v), 16) for v in nb.values())
print(f"number keys: {len(nums)} in {len(nb)} slots, {nu*5/8/1024:.2f} KB")

# --- 2-bit tail per posting: 0 = the word has <= 8 letters, 1..3 = hash of the rest
def tail(w): return 0 if len(w) <= 8 else 1 + zlib.crc32(w[8:].encode()) % 3
pairs = same = 0
for k, ws in real.items():
    ws = list(ws)
    for i in range(len(ws)):
        for j in range(i+1, len(ws)):
            pairs += 1; same += tail(ws[i]) == tail(ws[j])
print(f"different-stem words sharing an 8-letter key: {pairs} pairs, {same} still tied after the 2-bit tail ({100*(pairs-same)/pairs:.0f}% told apart)")

# --- refs per slot: Off is the slot base + prefix sum of Len, so only Len and DF are stored,
# each bitpacked at the widest value of the slot
key_df = collections.Counter(); key_len = collections.Counter()
for w in words:
    key_df[w[:8]] += len({d for d, _ in post[w]}); key_len[w[:8]] += est_bytes(len(post[w]))
rb = 0
for s, rest in by_slot.items():
    ks = [s.rstrip('_') + r for r in rest]
    wl = max(int(key_len[k]).bit_length() for k in ks); wd = max(key_df[k].bit_length() for k in ks)
    rb += len(ks)*(wl + wd) + 32
print(f"refs bitpacked per slot (Len + DF): {rb/8/1024:.1f} KB  vs fixed 12 B: {len(keys8)*12/1024:.1f} KB")

# --- how much the IDs cost: one list per 8-letter key, sorted by ID, delta + bitpack in blocks of 128
import random
key_post = collections.defaultdict(list)
for w in words:
    key_post[w[:8]].extend(post[w])
for s in nums: key_post['#' + s] = post[s]

def list_bits(ids):
    ids = sorted(ids); first = ids[0]
    bits = max(1, first.bit_length()) + 8           # first ID + 1 byte header (bit width)
    gaps = [b - a for a, b in zip(ids, ids[1:])]
    for i in range(0, len(gaps), 128):
        blk = gaps[i:i+128]
        bits += len(blk) * max(g.bit_length() for g in blk) + (8 if i else 0)
    return bits
def total_bits(idmap):
    return sum(list_bits([idmap[d] for d, _ in p]) for p in key_post.values())
P = sum(len(p) for p in key_post.values())
dense = list(range(N))
rnd = random.Random(1); sparse = sorted(rnd.sample(range(1, 2**31), N))     # external ids spread over int32
sparse30 = sorted(rnd.sample(range(1, 2**20), N))                          # ids from a counter with gaps
for name, m in [("dense 0..N-1 (internal docnum)", dense), ("external, gaps (N in 2^20)", sparse30), ("external, spread over int32", sparse)]:
    b = total_bits(m)
    print(f"IDs {name:32s} {b/8/1024:6.1f} KB  {b/P:5.1f} bits/posting")
df1 = sum(1 for p in key_post.values() if len(p) == 1)
print(f"lists {len(key_post)}  postings {P}  lists with 1 posting {df1} ({100*df1/len(key_post):.0f}%)  postings in lists >= 128: {sum(len(p) for p in key_post.values() if len(p)>=128)}")

# --- first ID of each list moved into the SlotBlock (cached), p keeps only the gaps
def gap_bits(ids):
    ids = sorted(ids); gaps = [b - a for a, b in zip(ids, ids[1:])]; bits = 0
    for i in range(0, len(gaps), 128):
        blk = gaps[i:i+128]; bits += len(blk) * max(g.bit_length() for g in blk) + (8 if i else 0)
    return bits
for name, m in [("dense", dense), ("external spread over int32", sparse)]:
    first_k = sum(max(1, min(m[d] for d, _ in p).bit_length()) + 5 for p in key_post.values())  # first ID + width
    gaps_p = sum(gap_bits([m[d] for d, _ in p]) for p in key_post.values())
    print(f"first ID in k ({name:26s}): k +{first_k/8/1024:5.1f} KB, p IDs {gaps_p/8/1024:5.1f} KB; lists that need no p read: {df1} ({100*df1/len(key_post):.0f}%)")

# --- raw-bytes SlotBlock: header 8 B + 5-bit key stream + per-key fields bitpacked
# per key: Count (wCount bits), GapW (5), Tail (1), FirstID - base (wFirst bits)
kb = 0; restarts = 0
for s, rest in by_slot.items():
    ks = [s.rstrip('_') + r for r in rest]
    cnt = [len(key_post[k]) for k in ks]
    first = [min(d for d, _ in key_post[k]) for k in ks]
    base = min(first)
    wc = max(c.bit_length() for c in cnt); wf = max((f - base).bit_length() for f in first)
    n = len(ks)
    rs = (n - 1) // 16 * 2                      # uint16 unit offset per restart point
    keyb = (slot_units(rest, 16) * 5 + 7) // 8
    fields = (n * (wc + 5 + 1 + wf) + 7) // 8
    kb += 8 + rs + keyb + fields; restarts += rs
for k, v in nb.items():                         # number slots
    ks = ['#' + str(sum(d << (5*(len(t)-1-i)) for i, d in enumerate(t))) for t in v]
    kb += 8 + (slot_units(sorted(v), 16)*5 + 7)//8 + (len(v) * (11 + 5 + 1 + 14) + 7)//8
print(f"\nraw k.<gen>: SlotDir 8.0 KB + slot blocks {kb/1024:.1f} KB (restart tables {restarts} B) = {(kb+8200)/1024:.1f} KB")

# --- numbers as NUM_n + n units of a binary value (uint5 / uint10 / uint15); bigger values overflow
def num_n(v):
    return 1 if v < 32 else 2 if v < 1024 else 3
over = collections.Counter(); kinds = collections.Counter(); keys15 = set(); slots15 = set()
for s, c in nums.items():
    v = int(s)
    if v >= 1 << 15: over[s] += c
    w = v & 0x7FFF                                   # wrap: keep the low 15 bits
    n = num_n(w); kinds[n] += c; keys15.add(w)
    units = [(w >> (5 * (n - 1 - i))) & 31 for i in range(n)]
    slots15.add((n, units[0]))
tot = sum(kinds.values())
print(f"\nNUM_n: tokens NUM_1 {kinds[1]} ({100*kinds[1]/tot:.1f}%), NUM_2 {kinds[2]} ({100*kinds[2]/tot:.1f}%), NUM_3 {kinds[3]} ({100*kinds[3]/tot:.1f}%)")
print(f"  overflow (> 32767): {sum(over.values())} tokens, values {sorted(over)}")
print(f"  distinct keys {len(keys15)} in {len(slots15)} slots; units incl. NUM_n code: {sum((1 + num_n(int(s) & 0x7FFF)) * c for s, c in nums.items())}")
