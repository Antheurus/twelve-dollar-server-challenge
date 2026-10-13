# Go + raw epoll + SQLite (no framework)

| | |
|---|---|
| Language | Go 1.27.2 (official tarball, sha256-checked in `install.sh`) |
| Framework | none: an HTTP/1.1 server on raw `epoll`/`accept4`/`read`/`write` syscalls, parsing requests from bytes |
| SQLite | 3.54.0 amalgamation compiled into the binary with cgo (sha256-checked in `build.sh`), driven through a small C shim, no `database/sql` |
| JWT / JSON | hand-written: HS256 over the standard library's `crypto/hmac`, a strict RFC 8259 validator |
| Dependencies | the Go standard library and SQLite, nothing else |
| **Nginx or direct** | **Direct**: serves `0.0.0.0:80` itself |

```bash
sudo bash install.sh   # apt: build-essential, curl, unzip; Go 1.27.2 into /usr/local/go
bash build.sh          # downloads + verifies SQLite 3.54.0, builds bin/server
SQLITE_PATH=... JWT_SECRET=... HOST=0.0.0.0 PORT=80 bash start.sh
```

`test/run.sh` passes 42/42 on linux/amd64 and linux/arm64. `e2e_test.go` covers what curl in
`test/test.sh` cannot reach: pipelined requests behind a write, requests split across packets,
`Connection: close` on a write, HTTP/1.0, `Expect: 100-continue` and chunked bodies
(`E2E_ADDR=127.0.0.1:3000 JWT_SECRET=... go test -run E2E`).

## Design, and why

**One event loop per process, everything on it.** Each process is one OS thread running
level-triggered `epoll`: accept, read, parse, query SQLite, write. On one vCPU there is nothing to
gain from more threads, and a connection costs a ~60-byte struct in a table indexed by fd, not a
goroutine and its buffers. Reads, writes, accepts and closes on the non-blocking sockets use raw
syscalls, skipping the scheduler hand-off of a blocking syscall. The steady state allocates
nothing, so the Go GC never runs during a load test (verified with `GODEBUG=gctrace=1`).

**Three processes sharing port 80 (`SO_REUSEPORT`), for file descriptors, not CPU.** The service
gets `LimitNOFILE=65535`, which is per process. One process tops out near 59k open sockets, so
past that it would have to close idle keep-alive connections and k6 would reconnect on its next
request: more CPU per request, and on a box with connection tracking a full conntrack table
dropping SYNs. Three processes hold ~178k connections, so every k6 user keeps its connection for
the whole run. The first process supervises the other two (restarts them if they exit, takes them
down with it). If the fd table ever does run short, a process closes only connections idle for
more than 2 s, shorter than the 3 s minimum think time in `bench/load.js` and well past any
request in flight.

**SQLite: one connection per process, statements prepared once, one cgo call per query.** The C
shim binds, steps every row, packs the columns into a buffer and resets, so `/feed` (20 rows, 100
columns) crosses into C once. WAL with `synchronous=NORMAL`, `mmap_size` 1 GB.

**Writes: queued, then group-committed per loop pass.** A like or a new post is authenticated and
validated at once, then queued. After the loop has served every ready socket, it runs all queued
writes in one `BEGIN IMMEDIATE … COMMIT` and only then sends their responses, so a 201 always
means committed (rule 6). The write lock is held for the inserts alone, never while sockets are
served, and `BEGIN` never waits: if the lock is held (by the other processes or the
checkpointer) the writes stay queued and are retried a millisecond later while reads keep being
served. A connection's later pipelined requests wait behind its write, so responses stay in order.

**Checkpoints on their own thread, in `RESTART` mode.** Auto-checkpointing is off in the event
loops, so a checkpoint's fsync never stalls one. A `PASSIVE` checkpoint was the first version and
it was wrong: under a steady write load the WAL never rewinds (frames arrive faster than a passive
checkpoint finishes), and it grew 8.6 MB/s to 800 MB in a minute, making every page lookup slower.
`RESTART` mode takes the write lock briefly once a second so the next writer starts the WAL from
the beginning; it now stays at ~10 MB.

**Queries.** `/feed` is the reference join (its plan is an index scan plus PK and covering-index
lookups). `/posts/:id` uses scalar subqueries instead of the join: for one row the join measured
7.0 µs against 1.5 µs (a fixed per-execution cost), while for twenty rows the join wins, 11 µs
against 12.6 µs. A like is one `INSERT … SELECT … WHERE EXISTS (post) ON CONFLICT DO NOTHING`; only
when it changes nothing does a second query tell "already liked" from "no such post".

**HTTP and JSON from bytes.** Request line and headers are matched in place
(`Content-Length`, `Authorization`, `Connection`, `Expect`, `Transfer-Encoding`), responses are
appended into one buffer and sent with one `write`. Keep-alive, pipelining, HTTP/1.0 and
`Connection: close` work; chunked request bodies get 501 (out of scope). JSON output escapes only
what must be escaped; the request body goes through a strict validator before the `body` field is
extracted. `body` is trimmed with JavaScript's `String.prototype.trim` whitespace set and limited
to 500 code points, the same unit as the schema's `length(body)` check.

**JWT.** Every token is verified on every request: signature (HS256 only, constant-time compare),
`exp`, `nbf` if present, then `sub` and `username`. The HMAC object is reused between requests
(its key schedule, not any verification result).

## Measured

RESULTS_PLACEHOLDER

## Tried and rejected (measured, not guessed)

| Idea | Result |
|---|---|
| One process | Capped near 59k sockets by `LimitNOFILE`; beyond that every request reconnects |
| Group commit holding the write lock from the first write to the end of the loop pass | p99 ~470 ms: the other process waited for the whole pass |
| `PASSIVE` checkpoints | WAL never rewound, 800 MB after a minute |
| `locking_mode=EXCLUSIVE` (one process, checkpoint in the loop) | -3 points CPU, but every fsync landed in the event loop and p99 rose |
| Committing at most every 10 / 25 ms (fewer page-cache resets in the other processes) | CPU within noise (54 → 50/51 %), p95 0.8 → 6/16 ms |
| SQLite `-O3`, `SQLITE_THREADSAFE=0` | `/feed` 12.4–13.5 µs vs 12.5–12.9 µs: noise |
| gzip | k6 creates its transport with `DisableCompression: true` and sends no `Accept-Encoding` |
