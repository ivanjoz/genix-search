#!/usr/bin/env python3
"""The xlsx of products -> genix-s3/testdata/bench/productos.tsv.

    python3 scripts/bench/prepare.py ~/Downloads/download6/productos.xlsx

Columns: id (the row number, 1..N), nombre, categoria, marca. The category is
the whole text of the column (some join several with commas): it is the group.
"""
import re
import sys
from pathlib import Path

import openpyxl

OUT = Path(__file__).resolve().parents[2] / "genix-s3/testdata/bench/productos.tsv"


def clean(v):
    return re.sub(r"\s+", " ", str(v)).strip() if v is not None else ""


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else str(Path.home() / "Downloads/download6/productos.xlsx")
    ws = openpyxl.load_workbook(src, read_only=True, data_only=True)["Productos"]

    rows = list(ws.iter_rows(values_only=True))
    header = [clean(c) for c in rows[1]]
    col = {name: i for i, name in enumerate(header)}

    out = []
    for r in rows[2:]:
        name = clean(r[col["Producto"]])
        if not name:
            continue
        out.append((len(out) + 1, name, clean(r[col["Categorías"]]), clean(r[col["Marca"]])))

    OUT.parent.mkdir(parents=True, exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as f:
        f.write("id\tnombre\tcategoria\tmarca\n")
        for row in out:
            f.write("\t".join(map(str, row)) + "\n")

    cats = {r[2] for r in out}
    brands = {r[3] for r in out if r[3]}
    print(f"{len(out)} products, {len(cats)} categories, {len(brands)} brands -> {OUT}")


main()
