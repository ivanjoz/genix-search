#!/usr/bin/env python3
"""GenixSearch Channel performance client.

Drives PUSHI ingestion and QUERY scoring over the raw TCP protocol, then
prints timing + rank traces. Intended to be invoked from ./run_perf.sh.
"""

import argparse
import csv
import pathlib
import socket
import sys
import time

DEFAULT_CSV = pathlib.Path(__file__).resolve().parent / "data" / "productos.csv"


class GenixConn:
    def __init__(self, host: str, port: int, mode: str, password: str | None) -> None:
        self.sock = socket.create_connection((host, port), timeout=30)
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

    def cmd(self, command: str) -> str:
        self._send(command)
        return self._readline()

    def query(self, collection: str, bucket: str, terms: str, limit: int = 10) -> tuple[float, str]:
        start = time.perf_counter()
        self._send(f'QUERY {collection} {bucket} "{terms}" LIMIT({limit})')
        pending = self._readline()
        if not pending.startswith("PENDING"):
            raise RuntimeError(f"unexpected query ack: {pending!r}")
        event = self._readline()
        elapsed_ms = (time.perf_counter() - start) * 1000.0
        parts = event.split(" ", 3)
        payload = parts[3] if len(parts) >= 4 else ""
        return elapsed_ms, payload

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


def iter_rows(csv_path: str):
    with open(csv_path, encoding="utf-8") as handle:
        reader = csv.reader(handle, quoting=csv.QUOTE_ALL)
        for row in reader:
            if not row:
                continue
            text = (row[0] or "").strip()
            if text:
                yield text


def phase_ingest(args, log) -> None:
    conn = GenixConn(args.host, args.port, "ingest", args.password)
    log(f"[ingest] target={args.n} rows | collection={args.collection} bucket={args.bucket}")
    started = time.perf_counter()
    last_report = started
    pushed = 0
    failed = 0
    try:
        for index, text in enumerate(iter_rows(args.csv), start=1):
            if index > args.n:
                break
            doc_id = index
            cmd = f'PUSHI {args.collection} {args.bucket} {doc_id} "{escape_text(text)}"'
            resp = conn.cmd(cmd)
            if resp.startswith("OK"):
                pushed += 1
            else:
                failed += 1
                if failed <= 5:
                    log(f"  row {index} unexpected: {resp!r} (text={text!r})")

            if (index % args.report_every) == 0:
                now = time.perf_counter()
                window = now - last_report
                rate_window = args.report_every / window if window > 0 else 0
                rate_total = index / (now - started) if now > started else 0
                log(
                    f"  progress: {index}/{args.n} | window {window:6.2f}s "
                    f"({rate_window:7.0f} rps) | overall {rate_total:7.0f} rps"
                )
                last_report = now
    finally:
        conn.close()

    total = time.perf_counter() - started
    rate = pushed / total if total > 0 else 0
    log(
        f"[ingest] done | pushed={pushed} failed={failed} | "
        f"elapsed={total:.2f}s avg={rate:.0f} rps"
    )


DEFAULT_QUERIES = [
    "aceite girasol",
    "aceite oliva",
    "leche",
    "leche entera",
    "agua mineral",
    "pan integral",
    "arroz",
    "queso",
    "jamon",
    "yogur natural",
    "atun",
    "cerveza",
    "vino tinto",
    "tomate frito",
    "aceit*",
    "olive 1l*",
    "agua min*",
    "leche desn*",
]


def phase_query(args, log) -> None:
    queries = args.queries.split("||") if args.queries else DEFAULT_QUERIES
    conn = GenixConn(args.host, args.port, "search", args.password)
    log(f"[query] queries={len(queries)} | collection={args.collection} bucket={args.bucket}")
    log(f"[query] index size at this phase: {args.n} rows")
    log("=" * 80)
    elapsed_total = 0.0
    try:
        for q in queries:
            elapsed_ms, payload = conn.query(args.collection, args.bucket, q, args.limit)
            elapsed_total += elapsed_ms
            results = payload.split(" ") if payload else []
            preview = ", ".join(results[:5]) + (" ..." if len(results) > 5 else "")
            log(
                f"  Q={q!r:30s} | {elapsed_ms:7.2f} ms | hits={len(results):3d} | "
                f"top={preview}"
            )
    finally:
        conn.close()

    if queries:
        log(f"[query] done | avg={elapsed_total/len(queries):.2f} ms | total={elapsed_total:.2f} ms")


def phase_flushc(args, log) -> None:
    conn = GenixConn(args.host, args.port, "ingest", args.password)
    log(f"[flushc] collection={args.collection}")
    try:
        resp = conn.cmd(f"FLUSHC {args.collection}")
        log(f"[flushc] response: {resp!r}")
    finally:
        conn.close()


def main() -> int:
    parser = argparse.ArgumentParser(description="GenixSearch Channel perf client")
    parser.add_argument("--host", default="::1")
    parser.add_argument("--port", type=int, default=1491)
    parser.add_argument("--password", default=None)
    parser.add_argument("--csv", default=str(DEFAULT_CSV))
    parser.add_argument("--n", type=int, default=1000)
    parser.add_argument("--collection", default="products")
    parser.add_argument("--bucket", default="all")
    parser.add_argument("--limit", type=int, default=10)
    parser.add_argument("--report-every", type=int, default=500)
    parser.add_argument("--queries", default="", help="separate with || ; otherwise uses defaults")
    parser.add_argument("phase", choices=["ingest", "query", "flushc"])
    args = parser.parse_args()

    def log(message: str) -> None:
        print(message, flush=True)

    if args.phase == "ingest":
        phase_ingest(args, log)
    elif args.phase == "query":
        phase_query(args, log)
    elif args.phase == "flushc":
        phase_flushc(args, log)
    else:
        parser.error(f"unknown phase: {args.phase}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
