import csv, unicodedata, collections, math
STOP=set("a al con de del desde el en es esa ese eso esos esta esto fue ha la las le les lo los me mi mis ni no nos o para por que se ser si son soy su sus tu y".split())
FOLD={}
for s,t in [("áàäâÁÀÄÂ","a"),("éèëêÉÈËÊ","e"),("íìïîÍÌÏÎ","i"),("óòöôÓÒÖÔ","o"),("úùüûÚÙÜÛ","u"),("ñÑ","n")]:
    for c in s: FOLD[c]=t
def nc(c):
    if 'a'<=c<='z' or '0'<=c<='9': return c
    if 'A'<=c<='Z': return c.lower()
    return FOLD.get(c,'')
def normalize(s):
    toks=[];cur=[];last='';run=0
    for ch in s:
        n=nc(ch)
        if n and n.isalpha():
            if n==last: run+=1
            else: last,run=n,1
            if run <= (2 if n=='l' else 1): cur.append(n)
        elif n:
            cur.append(n); last=''
        else:
            if cur: toks.append(''.join(cur)); cur=[]
            last=''
    if cur: toks.append(''.join(cur))
    return [t for t in toks if t not in STOP]
def docs():
    out=[]
    with open(__import__('os').path.join(__import__('os').path.dirname(__file__),'..','perf','data','productos.csv'),newline='') as f:
        for row in csv.reader(f):
            if row: out.append(' '.join(normalize(row[0])))
    return out
