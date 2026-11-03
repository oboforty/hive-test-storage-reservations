#!/usr/bin/env python3
"""Fire concurrent reservation requests and check the account is never oversubscribed.

Usage:
    python concurrency_client.py --calls 10

See argparse code for optional parameters.

Notes:
  * The account's max_parallel_chunks (default 5) also limits active reservations.
    If it is lower than the number of reservations that fit into the free space, it
    is reached before the storage limit.
  * The API rate-limits each client IP (RATE_LIMIT, default 10/s), so some requests
    may get a 429. Run the server with RATE_LIMIT=0 to avoid that.
"""

import argparse
import json
import random
import sys
import threading
import urllib.error
import urllib.request
import uuid

GB = 1 << 30


def request(method, url, body=None, dedup=False):
    """Send a request and return (status, parsed JSON body or None)."""
    headers = {"Accept": "application/json"}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if dedup:
        headers["X-Dedup-ID"] = str(uuid.uuid4())
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    try:
        return status, json.loads(raw) if raw else None
    except ValueError:
        return status, raw.decode(errors="replace")


def find_account(base, account):
    status, accounts = request("GET", f"{base}/accounts")
    if status != 200:
        sys.exit(f"GET /accounts failed with {status}: {accounts}")
    for acc in accounts:
        if account in (acc["id"], acc["username"]):
            return acc["id"]
    sys.exit(f"account {account!r} not found")


def summary(base, account_id):
    status, body = request("GET", f"{base}/accounts/{account_id}?full=1")
    if status != 200:
        sys.exit(f"GET account summary failed with {status}: {body}")
    return body


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--url", default="http://127.0.0.1:8080", help="API base URL")
    p.add_argument("--account", default="hive", help="account id or username")
    p.add_argument("--calls", type=int, default=10, help="number of concurrent requests")
    p.add_argument("--min-gb", type=float, default=25, help="minimum reservation size in GiB")
    p.add_argument("--max-gb", type=float, default=30, help="maximum reservation size in GiB")
    p.add_argument("--reset", action="store_false", default=True, help="release all active reservations of the account before the run")
    args = p.parse_args()
    base = args.url.rstrip("/")

    account_id = find_account(base, args.account)
    if args.reset:
        status, body = request("DELETE", f"{base}/accounts/{account_id}/reservations", dedup=True)
        if status != 200:
            sys.exit(f"reset failed with {status}: {body}")
        print(f"reset: released {body['released']} reservation(s)")

    before = summary(base, account_id)
    free_before = before["free_storage"]
    print(f"account {before['username']} ({account_id}): "
          f"max {before['max_storage'] / GB:.2f} GiB, free {free_before / GB:.2f} GiB, "
          f"max_parallel_chunks {before['max_parallel_chunks']}, "
          f"active reservations {len(before['active_reservations'])}")

    sizes = [random.randint(int(args.min_gb * GB), int(args.max_gb * GB)) for _ in range(args.calls)]
    results = [None] * args.calls
    barrier = threading.Barrier(args.calls)
    url = f"{base}/accounts/{account_id}/reservations"

    def worker(i):
        barrier.wait()  # release all threads at once
        try:
            results[i] = request("POST", url, {"size": sizes[i]}, dedup=True)
        except Exception as e:  # connection errors etc.
            results[i] = (None, repr(e))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(args.calls)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    accepted, no_space, too_many, rate_limited, unexpected = [], [], [], [], []
    for size, (status, body) in zip(sizes, results):
        msg = body.get("error") if isinstance(body, dict) else None
        if status == 201:
            accepted.append(size)
        elif status == 409 and msg == "insufficient storage":
            no_space.append((size, body.get("details", {})))
        elif status == 409 and msg == "too many active reservations":
            too_many.append(size)
        elif status == 429:
            rate_limited.append(size)
        else:
            unexpected.append((size, status, body))

    after = summary(base, account_id)
    accepted_total = sum(accepted)

    print(f"\nsent {args.calls} requests, total {sum(sizes) / GB:.2f} GiB")
    print(f"  201 accepted:                  {len(accepted):3d}  ({accepted_total / GB:.2f} GiB)")
    print(f"  409 insufficient storage:      {len(no_space):3d}")
    print(f"  409 too many reservations:     {len(too_many):3d}")
    print(f"  429 rate limited:              {len(rate_limited):3d}")
    print(f"  other:                         {len(unexpected):3d}")
    print(f"reserved {after['reserved_storage'] / GB:.2f} / {after['max_storage'] / GB:.2f} GiB, "
          f"free {after['free_storage'] / GB:.2f} GiB")

    failures = []
    if accepted_total > free_before:
        failures.append(f"accepted {accepted_total} bytes but only {free_before} were free")
    if after["reserved_storage"] - before["reserved_storage"] != accepted_total:
        failures.append(f"reserved storage grew by {after['reserved_storage'] - before['reserved_storage']} "
                        f"bytes, accepted total is {accepted_total}")
    if after["reserved_storage"] > after["max_storage"]:
        failures.append("reserved storage exceeds max storage")
    for size, details in no_space:
        # Free space only shrinks during the run, so a rejected size must exceed the
        # final free space and the free space the server reported at rejection time.
        if size <= after["free_storage"] or size <= details.get("free_bytes", -1):
            failures.append(f"{size} bytes rejected as insufficient storage, but it fits "
                            f"(reported free {details.get('free_bytes')}, final free {after['free_storage']})")
    for size, status, body in unexpected:
        failures.append(f"unexpected response for {size} bytes: {status} {body}")

    if too_many:
        print(f"\nnote: max_parallel_chunks ({after['max_parallel_chunks']}) was reached before the storage limit")
    if rate_limited:
        print("note: some requests were rate limited; run the server with RATE_LIMIT=0 to avoid that")

    if failures:
        print("\nFAIL")
        for f in failures:
            print(f"  - {f}")
        sys.exit(1)
    print("\nOK: concurrent reservations never exceeded the account's storage")


if __name__ == "__main__":
    main()
