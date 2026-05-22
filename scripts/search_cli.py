#!/usr/bin/env python3
"""Interactive search CLI for GenixSearch.

Connects to the server, prompts for a search term, and prints ranked
results with the actual product names resolved from the CSV.

Usage:
    python3 scripts/search_cli.py
    python3 scripts/search_cli.py --n 50000   # if you loaded 50k records
    python3 scripts/search_cli.py --limit 20  # show top 20 results

Search tips:
    aceite girasol      exact term search
    leche*              prefix search (trailing *)
    leche entera        multi-term (AND-ranked)
"""

import argparse
import csv
import itertools
import pathlib
import readline  # noqa: F401 — enables arrow-key history in input()
import socket
import sys

DEFAULT_CSV = pathlib.Path(__file__).resolve().parent / "perf" / "data" / "productos.csv"


class GenixConn:
    def __init__(self, host: str, port: int, mode: str, password: str | None = None) -> None:
        self.sock = socket.create_connection((host, port), timeout=10)
        self.buf = b""
        banner = self._readline()
        if not banner.startswith("CONNECTED"):
            raise RuntimeError(f"unexpected banner: {banner!r}")
        start_cmd = f"START {mode} {password}" if password else f"START {mode}"
        self._send(start_cmd)
        ack = self._readline()
        if not ack.startswith("STARTED"):
            raise RuntimeError(f"unexpected STARTED ack: {ack!r}")

    def _send(self, cmd: str) -> None:
        self.sock.sendall((cmd + "\n").encode("utf-8"))

    def _readline(self) -> str:
        while b"\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise EOFError("connection closed while reading")
            self.buf += chunk
        line, _, self.buf = self.buf.partition(b"\n")
        return line.decode("utf-8", errors="replace").rstrip("\r")

    def query(self, collection: str, bucket: str, terms: str, limit: int = 10) -> list[tuple[int, int]]:
        """Return list of (doc_id, score) sorted by descending score."""
        self._send(f'QUERY {collection} {bucket} "{escape_text(terms)}" LIMIT({limit})')
        pending = self._readline()
        if not pending.startswith("PENDING"):
            raise RuntimeError(f"unexpected query ack: {pending!r}")
        event = self._readline()
        parts = event.split(" ", 3)
        payload = parts[3] if len(parts) >= 4 else ""
        if not payload:
            return []
        results = []
        for token in payload.split():
            if "|" in token:
                doc_str, score_str = token.split("|", 1)
                try:
                    results.append((int(doc_str), int(score_str)))
                except ValueError:
                    pass
        return results

    def close(self) -> None:
        try:
            self._send("QUIT")
            self._readline()
        except Exception:
            pass
        try:
            self.sock.close()
        except Exception:
            pass


def escape_text(text: str) -> str:
    return text.replace("\\", "\\\\").replace('"', '\\"')


def build_index(csv_path: str, n: int | None) -> dict[int, str]:
    """Map doc_id → product name, matching the IDs assigned during load."""
    with open(csv_path, encoding="utf-8") as fh:
        rows = [
            row[0].strip()
            for row in csv.reader(fh, quoting=csv.QUOTE_ALL)
            if row and row[0].strip()
        ]
    if not rows:
        return {}
    total = n if n is not None else len(rows)
    index: dict[int, str] = {}
    for doc_id, text in enumerate(itertools.islice(itertools.cycle(rows), total), start=1):
        index[doc_id] = text
    return index


def run_search_loop(conn: GenixConn, index: dict[int, str], args: argparse.Namespace) -> None:
    print(f"\nConnected to [{args.host}]:{args.port}  collection={args.collection}/{args.bucket}")
    print(f"Index size: {len(index)} products  |  Showing top {args.limit} results")
    print('Type a search term (e.g. "aceite", "leche entera", "agua*"). Ctrl-C or empty to quit.\n')

    while True:
        try:
            query = input("search> ").strip()
        except (EOFError, KeyboardInterrupt):
            print()
            break
        if not query:
            break

        try:
            results = conn.query(args.collection, args.bucket, query, args.limit)
        except Exception as exc:
            print(f"  ERROR: {exc}")
            continue

        if not results:
            print("  No results.\n")
            continue

        max_score = results[0][1] if results else 1
        print(f"\n  {'Rank':<5} {'Score':>6}  {'Bar':<20}  Product")
        print(f"  {'-'*5} {'-'*6}  {'-'*20}  {'-'*50}")
        for rank, (doc_id, score) in enumerate(results, start=1):
            name = index.get(doc_id, f"<id={doc_id}>")
            bar_len = int(score / max_score * 20)
            bar = "█" * bar_len + "░" * (20 - bar_len)
            print(f"  {rank:<5} {score:>6}  {bar}  {name}")
        print()


def main() -> int:
    parser = argparse.ArgumentParser(description="GenixSearch interactive CLI")
    parser.add_argument("--host", default="::1")
    parser.add_argument("--port", type=int, default=1491)
    parser.add_argument("--password", default="SecretPassword")
    parser.add_argument("--csv", default=str(DEFAULT_CSV))
    parser.add_argument("--n", type=int, default=None,
                        help="number of records that were loaded (default: all CSV rows). "
                             "Must match what was used in load_products.py")
    parser.add_argument("--collection", default="products")
    parser.add_argument("--bucket", default="all")
    parser.add_argument("--limit", type=int, default=15,
                        help="max results to show per query (default: 15)")
    args = parser.parse_args()

    print(f"Loading product index from {args.csv}...")
    index = build_index(args.csv, args.n)
    if not index:
        print(f"ERROR: no rows found in {args.csv}", file=sys.stderr)
        return 1
    print(f"Loaded {len(index)} product names.")

    try:
        conn = GenixConn(args.host, args.port, "search", args.password)
    except Exception as exc:
        print(f"ERROR: could not connect to [{args.host}]:{args.port}: {exc}", file=sys.stderr)
        print("Make sure the GenixSearch server is running.", file=sys.stderr)
        return 1

    try:
        run_search_loop(conn, index, args)
    finally:
        conn.close()

    return 0


if __name__ == "__main__":
    sys.exit(main())
