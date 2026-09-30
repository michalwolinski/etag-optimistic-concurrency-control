# ETag: the concurrency guard hiding in a cache header

Reproducible demo for the article of the same name by **Michał Woliński**.

- **Article:** https://wolinski.com/blog/etag-optimistic-concurrency-control.html
- **Author:** https://wolinski.com

Every code snippet in the article is a real file in this repo. It builds
optimistic concurrency control end to end — a version column in SQL, a
compare-and-set in PHP, a precondition handler in Go — and then runs it: two
real clients racing for one resource, over a real socket, with the actual
wire-level output captured below.

## Layout

```
php/
  Order.php                  immutable value object; withStatus() keeps the SAME version
  OrderRepository.php        the compare-and-set: UPDATE ... WHERE id = ? AND version = ?
  OptimisticLockException.php  carries both the expected and the actual version
  demo.php                   two writers on SQLite, plus the hash-validator ABA scenario
go/
  handler.go                 entityTag, splitEntityTags, strongCompare/weakCompare,
                              evaluateIfMatch, handleGet/handlePut
  handler_test.go            TestConcurrentWritersExactlyOneWins — two goroutines,
                              one sync.WaitGroup, a channel as a starting gun
  race.sh                    drives the same race over a real HTTP socket
  race.log                   captured output of the run the article quotes
```

## Run it

```bash
# The PHP side: two writers against SQLite, then the hash-vs-version comparison
php php/demo.php

# The Go side: the concurrent-writer test, under the race detector
cd go && go test -race -v ./...

# The Go side, over the wire: builds the binary, starts it, drives the race,
# tears it down
cd go && ./race.sh
```

## What it shows

1. **The lost update, with no malice required.** `php/demo.php` section 1: two
   writers read version 1, both write, and the database enforces that exactly
   one of them wins — `rowCount() !== 1` on the loser, `OptimisticLockException`
   carrying both the version it expected and the version that actually won.

2. **A content hash is not a strong validator.** `php/demo.php` section 2: the
   same row moves `new → shipped → new`, restoring the hash it had at the
   start. A stale writer holding that first hash is accepted — the ABA
   problem RFC 9110 §8.8.1 rules out for a strong validator ("unique across
   all versions... over time"). The version-guarded statement rejects the
   same write, because version 1 never recurs.

3. **The two conditional headers are not interchangeable.** `go/handler.go`
   implements both comparison functions from RFC 9110 §8.8.3.2: `weakCompare`
   for `If-None-Match` (cache validation), `strongCompare` for `If-Match`
   (the precondition). `splitEntityTags` handles the case a naive
   `strings.Split` gets wrong — a comma is legal *inside* a quoted tag.

4. **Exactly one writer moves the version, and the race detector agrees.**
   `handler_test.go`'s `TestConcurrentWritersExactlyOneWins` releases two
   goroutines from the same channel close and asserts one `204` and one
   `412` — never which one, because the scheduler decides that and the
   mechanism does not care.

5. **The same race, on the wire, with headers you can read:**

   ```
   ### 1. Read the resource once - this validator is what both clients will hold
   GET  /orders/1                                  -> 200  ETag: "order-1-v1"     {"id":1,"status":"new","version":1}

   ### 2. The same header, the other half of its job: cache validation
   GET  /orders/1  If-None-Match: "order-1-v1"     -> 304  ETag: "order-1-v1"
   GET  /orders/1  If-None-Match: W/"order-1-v1"   -> 304  ETag: "order-1-v1"

   ### 3. A write that carries no precondition at all
   PUT  /orders/1  (no If-Match)                   -> 428  ETag: "order-1-v1"     conditional request required: send If-Match

   ### 4. The race: both clients released together, both holding the same validator
   PUT  /orders/1  If-Match: "order-1-v1"  paid    -> 204  ETag: "order-1-v2"
   PUT  /orders/1  If-Match: "order-1-v1"  shipped -> 412  ETag: "order-1-v2"     precondition failed

   ### 5. The resource afterwards: written once, version advanced once
   GET  /orders/1                                  -> 200  ETag: "order-1-v2"     {"id":1,"status":"paid","version":2}

   ### 6. The rejected client retries with the validator it was handed
   PUT  /orders/1  If-Match: "order-1-v2"  shipped -> 204  ETag: "order-1-v3"
   ```

   `go/race.log` is that exact capture. Step 2 and step 4 are the cleanest
   argument in the repo for why `If-None-Match` and `If-Match` are not the
   same mechanism: `W/"order-1-v1"` earns a `304` on the read path and would
   earn a permanent `412` on the write path.

## Versions

| Component | Version |
|---|---|
| PHP | 8.5, `pdo_sqlite` |
| Go | 1.24+ (`go.mod` pins `go 1.24`) |

## About

Written by [Michał Woliński](https://wolinski.com) — a systems architect
working on backend infrastructure, distributed systems and production AI.
The article this accompanies is
[ETag: The Concurrency Guard Hiding in a Cache Header](https://wolinski.com/blog/etag-optimistic-concurrency-control.html).

If something here has drifted from the article, or reproduces differently on
your versions, an issue on this repository is the right place for it.
