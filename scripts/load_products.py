#!/usr/bin/env python3
"""Push product records into GenixSearch via PUSHI.

Reads the CSV and sends records one-by-one over the TCP ingest channel.
If --n is larger than the CSV row count, records cycle (same names, new IDs).

Usage:
    python3 scripts/load_products.py               # push all CSV rows (~10k)
    python3 scripts/load_products.py --n 50000     # push 50k (cycles CSV)
    python3 scripts/load_products.py --flush-first # clear collection then ingest
"""

import argparse
import csv
import itertools
import pathlib
import socket
import sys
import time

DEFAULT_CSV = pathlib.Path(__file__).resolve().parent / "perf" / "data" / "productos.csv"


class GenixConn:
    def __init__(self, host: str, port: int, mode: str, password: str | None = None) -> None:
        self.sock = socket.create_connection((host, port), timeout=60)
        self.buf = b""
        banner = self._readline()
        if not banner.startswith("CONNECTED"):
            raise RuntimeError(f"unexpected banner: {banner!r}")
        start_cmd = f"START {mode} {password}" if password else f"START {mode}"
        self._send(start_cmd)
        ack = self._readline()
        if not ack.startswith("STARTED"):
            if "authentication_required" in ack:
                raise RuntimeError("authentication failed — check the password in --password or the default in the script")
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

    def cmd(self, command: str) -> str:
        self._send(command)
        return self._readline()

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


def load_csv_rows(csv_path: str) -> list[str]:
    rows = []
    with open(csv_path, encoding="utf-8") as fh:
        reader = csv.reader(fh, quoting=csv.QUOTE_ALL)
        for row in reader:
            if not row:
                continue
            text = (row[0] or "").strip()
            if text:
                rows.append(text)
    return rows


def main() -> int:
    parser = argparse.ArgumentParser(description="Load products into GenixSearch")
    parser.add_argument("--host", default="::1")
    parser.add_argument("--port", type=int, default=1491)
    parser.add_argument("--password", default="SecretPassword")
    parser.add_argument("--csv", default=str(DEFAULT_CSV))
    parser.add_argument("--n", type=int, default=None,
                        help="number of records to push (default: all CSV rows)")
    parser.add_argument("--collection", default="products")
    parser.add_argument("--bucket", default="all")
    parser.add_argument("--report-every", type=int, default=1000)
    parser.add_argument("--flush-first", action="store_true",
                        help="flush the collection before ingesting")
    args = parser.parse_args()

    rows = load_csv_rows(args.csv)
    if not rows:
        print(f"ERROR: no rows found in {args.csv}", file=sys.stderr)
        return 1

    n = args.n if args.n is not None else len(rows)
    print(f"CSV rows available : {len(rows)}")
    print(f"Records to push    : {n}")
    print(f"Target             : {args.collection}/{args.bucket} @ [{args.host}]:{args.port}")
    if n > len(rows):
        print(f"Note: cycling CSV ({len(rows)} rows) to reach {n}")

    if args.flush_first:
        print(f"\nFlushing collection '{args.collection}' first...")
        conn = GenixConn(args.host, args.port, "ingest", args.password)
        resp = conn.cmd(f"FLUSHC {args.collection}")
        conn.close()
        print(f"  -> {resp}")

    print("\nIngesting...")
    conn = GenixConn(args.host, args.port, "ingest", args.password)
    started = time.perf_counter()
    last_report = started
    pushed = 0
    failed = 0

    try:
        record_stream = itertools.islice(itertools.cycle(rows), n)
        for doc_id, text in enumerate(record_stream, start=1):
            cmd = f'PUSHI {args.collection} {args.bucket} {doc_id} "{escape_text(text)}"'
            resp = conn.cmd(cmd)
            if resp.startswith("OK"):
                pushed += 1
            else:
                failed += 1
                if failed <= 5:
                    print(f"  row {doc_id} unexpected response: {resp!r}")

            if doc_id % args.report_every == 0:
                now = time.perf_counter()
                window = now - last_report
                rate_window = args.report_every / window if window > 0 else 0
                rate_total = doc_id / (now - started) if now > started else 0
                pct = doc_id / n * 100
                print(
                    f"  {doc_id:>6}/{n} ({pct:5.1f}%) | "
                    f"window {window:5.2f}s ({rate_window:6.0f} rps) | "
                    f"overall {rate_total:6.0f} rps"
                )
                last_report = now
    finally:
        conn.close()

    elapsed = time.perf_counter() - started
    rate = pushed / elapsed if elapsed > 0 else 0
    print(f"\nDone: pushed={pushed}  failed={failed}  elapsed={elapsed:.1f}s  avg={rate:.0f} rps")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
