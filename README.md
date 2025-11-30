# Go DAG Scheduler

**Distributed DAG Scheduler with WAL-based Recovery and Failure Propagation**

A concurrent task orchestrator that resolves dependency graphs, executes tasks through a priority-aware worker pool, and survives process crashes through write-ahead logging. Built in Go with zero external orchestration dependencies.

---

## Why This Problem Is Hard

Building a task scheduler is easy. Building one that is **correct under failure** is not.

The moment you introduce concurrency, dependencies, and crash recovery into a single system, you get a collision of invariants that fight each other:

- A worker dequeues a task. While it executes, the scheduler cascade-fails that task. Who wins? If the worker calls `Complete()`, it unlocks dependents that should be dead. If the scheduler overwrites the status, you have a **data race**.
- The process crashes mid-execution. On restart, how do you know which tasks were in-flight? If you re-execute them, you might double-run. If you skip them, you lose work.
- The scheduler resolves dependencies, manages retries, and dispatches tasks — all from multiple goroutines calling into shared state. A single mutex creates **deadlocks under load**. No mutex creates races.

This project solves all three. Not in theory — in practice, through bugs found, debugged, and fixed.

---

## Architecture

![Architecture](./assets/architecture.png)

**Why a single-goroutine event loop?** The scheduler owns all mutable DAG state — task map, in-degree counters, dependency graph, ready queue. Instead of protecting this with a mutex (which caused deadlocks — see [Challenge #1](#1-system-deadlock--the-mutex-that-broke-everything)), all mutations flow through channels into a single `runLoop` goroutine. This is the same pattern used by Redis and Node.js. Zero lock contention. Zero data races on the hot path.

---

## Task Lifecycle

![Task Lifecycle](./assets/lifecycle.png)

| State | Meaning | Triggered By |
|---|---|---|
| `pending` | Ingested, blocked on dependencies | API submission |
| `ready` | All deps satisfied, in priority queue | In-degree reaches 0 |
| `running` | Picked up by a worker | Dequeued from heap |
| `completed` | Execution succeeded | Worker reports success |
| `failed` | Permanently failed or cascade-killed | Retries exhausted / upstream failure |

**Retry loop**: On failure, tasks re-enter `pending` with exponential backoff (`2ˢ`, capped at 30s). Default: 3 retries. Only after exhausting all retries does permanent failure + cascade trigger.

---

## Failure Handling

![Failure Cascade](./assets/failure.png)

### Why This Matters

Without failure propagation, downstream tasks execute against broken prerequisites — wasting compute and producing corrupt results. This scheduler treats **failure as a first-class citizen**.

### Cascade Mechanics

When a task exhausts retries, `propagateFailure()` runs a **BFS traversal** through all transitive dependents:

```
A (failed) ──► B (cascade-failed) ──► C (cascade-failed)
                                  ──► D (cascade-failed)
```

Each cascaded task is: marked `StatusFailed`, flagged atomically (`Cancelled.Store(1)`), persisted to WAL, and skipped by any worker that already dequeued it.

### The Dual-Guard Pattern

The scheduler owns `task.Status`. Workers **cannot read it** without a data race. Instead, workers check an `atomic.Int32` cancellation flag at two points:

```
GUARD 1 (pre-execution):   task.Cancelled.Load() == 1 → skip entirely
         ↓
     [ execute task ]
         ↓
GUARD 2 (post-execution):  task.Cancelled.Load() == 1 → do NOT call Complete()
```

**Why two guards?** Guard 1 catches tasks cancelled while buffered in the channel. Guard 2 catches tasks cancelled *during* execution — without it, a "completed" task would unlock dependents that the cascade already killed. This was a real bug (see [Challenge #2](#2-execution-after-failure--a-dag-correctness-violation)).

---

## Crash Recovery

![WAL Recovery](./assets/recovery.png)

### WAL Design

Every mutation is serialized to an append-only NDJSON file and `fsync`'d **before** the scheduler processes it:

```json
{"type":"INGEST","tasks":[{"id":"A","payload":"build","priority":1,"dependencies":[]}]}
{"type":"START","task_id":"A"}
{"type":"COMPLETE","task_id":"A"}
{"type":"FAIL","task_id":"B"}
{"type":"REQUEST","idempotency_key":"build-v1"}
```

### Replay Sequence

On restart:

1. **Parse WAL** line-by-line, tracking byte offsets
2. **Truncate corruption** — if a partial write is found (crash mid-`fsync`), truncate at the last valid offset. No valid data is lost
3. **Replay events**:
   - `INGEST` → reconstruct DAG topology, dependency graph, in-degree map
   - `START` → mark running, update metrics
   - `COMPLETE` → mark completed, decrement dependent in-degrees, enqueue newly-ready tasks
   - `FAIL` → increment retry counter, cascade if exhausted
4. **Requeue orphans** — tasks with `START` but no terminal event were in-flight at crash time → re-enqueue into the ready queue with correct metrics
5. **Rebuild idempotency store** — `REQUEST` entries restore processed keys so duplicates are rejected post-crash

---

## Key Engineering Challenges

These are real bugs discovered, root-caused, and fixed during development. Each one taught a lesson about concurrent systems design.

### 1. System Deadlock — The Mutex That Broke Everything

**Commit**: [`13cfaa5`](../../commit/13cfaa5) — *replace global mutex with single-owner event loop*

**Symptom**: Under concurrent load, the system would hang indefinitely. No crash, no error, no log — just silence.

**Root cause**: The scheduler used `sync.Mutex` to protect shared state. Workers called `Complete()`, which locked the mutex and triggered enqueuing of dependent tasks. If the queue was full, it blocked — while holding the lock. Meanwhile, the scheduler's dispatch loop needed the same lock to dequeue tasks. **Classic lock-ordering deadlock.**

**Fix**: Eliminated the mutex entirely. Replaced with a single-goroutine event loop (`runLoop`) where all state transitions happen via channel sends. Workers send task IDs through `completeChan`/`failChan` — they never touch shared state. Deadlock is structurally impossible.

**Lesson**: Mutexes compose poorly in systems with bidirectional data flow. The actor model (channels + single owner) eliminates the entire class of problems.

---

### 2. Execution After Failure — A DAG Correctness Violation

**Commit**: [`7062eed`](../../commit/7062eed) — *prevent execution of tasks marked failed*

**Symptom**: Task A fails and cascades to B and C. But B still completes successfully and unlocks its dependents. The DAG produces results from a broken pipeline.

**Root cause**: The worker had already dequeued B from `readyChan` before the cascade ran. By the time the scheduler marked B as failed, the worker was mid-execution. On completion, it called `Complete()`, which unlocked B's dependents — violating the invariant that **nothing downstream of a permanent failure should ever execute**.

**Fix**: Introduced the `atomic.Int32` cancellation flag with dual-guard pattern. The scheduler sets `Cancelled.Store(1)` during BFS cascade. Workers check `.Load()` before execution (Guard 1) and after execution (Guard 2). This is lock-free, race-free, and zero-cost on the happy path.

**Lesson**: In concurrent systems, "mark as failed" and "stop execution" are two different operations that happen at different times. You need a cross-goroutine signal that doesn't require synchronization.

---

### 3. Event Loop Starvation — Metrics That Killed Throughput

**Commit**: [`6ba2bf2`](../../commit/6ba2bf2) — *eliminate blocking QueueSize calls*

**Symptom**: Under load testing (~100 concurrent requests), task completion rate dropped to near-zero despite workers being idle. The scheduler appeared frozen.

**Root cause**: The API handler called `scheduler.QueueSize()` for admission control. This was implemented as a channel round-trip into `runLoop` — send request, wait for response. Under load, dozens of HTTP goroutines queued up on `QueueSize()`, flooding the event loop's select with read requests. Real work (ingests, completions, failures) couldn't get a slot. **The monitoring system starved the system it was monitoring.**

**Fix**: Replaced channel-based `QueueSize()` with `atomic.Int64`. The scheduler increments/decrements it inline. The API reads it with `.Load()` — zero event loop involvement, zero blocking.

**Lesson**: Never funnel read-only queries through the same channel as write operations. Atomic counters exist for exactly this reason.

---

### 4. Data Race on Task Status — The `-race` Detector Win

**Commit**: [`35de8ea`](../../commit/35de8ea) — *make metrics non-blocking, remove task status data race*

**Symptom**: `go test -race` flagged concurrent read/write on `task.Status`.

**Root cause**: Two separate issues. First: the `Metrics()` function iterated over all tasks and read `task.Status` from the HTTP goroutine while `runLoop` was writing it — textbook data race. Second: workers read `task.Status` (a string field) to check for cancellation while the scheduler wrote to it during cascade — another race.

**Fix**: Two-part fix. (1) Replaced per-task status counting with **pre-aggregated atomic counters** (`atomic.Int64` for pending/running/completed/failed/retried). Updated inside `runLoop`, read lock-free from any goroutine. (2) Added `Cancelled atomic.Int32` field to the Task struct — workers read this instead of `Status`. Clean separation: `Status` is owned by `runLoop`, `Cancelled` is the cross-goroutine signal.

**Lesson**: The Go race detector is non-negotiable. "It works in practice" is not the same as "it is correct."

---

### 5. WAL Replay Inconsistency — The Ghost Pending Tasks

**Commit**: [`34241b4`](../../commit/34241b4) — *fix WAL recovery metrics*

**Symptom**: After crash-restart, `/api/v1/status` reported `pending: 2` for tasks that had completed before the crash. New tasks submitted post-recovery worked fine — but the metrics were permanently wrong.

**Root cause**: The WAL replay functions (`handleReplayStart`, `handleReplayComplete`, `handleReplayFail`) correctly set task statuses but **never updated atomic metrics**. The `Ingest` replay added +2 to pending. The `Complete` replay set status to completed but didn't decrement pending or increment completed. Additionally, `handleRequeueOrphans` set orphan tasks to `StatusReady` but never enqueued them into the `readyQueue` — they were invisible to the dispatch loop.

**Fix**: All four replay handlers now maintain full metric consistency. `handleRequeueOrphans` properly enqueues into the priority queue. Post-recovery metrics exactly match the replayed state.

**Lesson**: Recovery code needs the same invariant maintenance as normal-path code. "Set the status" is not the same as "transition the state."

---

### 6. WAL Corruption Handling

**Commit**: [`f6579e2`](../../commit/f6579e2) — *make WAL crash-safe with exact byte offset truncation*

**Symptom**: If the process crashed mid-write, recovery would fail with a JSON parse error and halt — losing all events after the corrupt line, including valid ones before it.

**Root cause**: The recovery function used a simple line reader. A partial JSON line (from a crash during `write()` before `fsync()`) caused `json.Unmarshal` to fail, and the function returned an error that stopped the entire replay.

**Fix**: Track byte offsets during reading. When an unparseable line is hit, stop replay at the last valid offset and truncate the file there. Seek the append handle to the new EOF. All valid entries before the corruption are preserved. The trade-off: at most one in-flight write is lost — acceptable for a single-node system where that task will be retried anyway.

---

## System Guarantees

| Guarantee | How It's Enforced |
|---|---|
| **No task is lost** | WAL `fsync` completes before scheduler processes any mutation |
| **No execution after upstream failure** | BFS cascade + atomic dual-guard on workers |
| **No duplicate processing** | Idempotency keys stored in-memory, rebuilt from WAL on crash recovery |
| **Deterministic DAG execution** | Single-owner event loop — all ordering decisions happen in one goroutine |
| **Crash-safe recovery** | WAL replay reconstructs full state: DAG topology, retry counts, metrics, orphans |
| **No data races** | Zero shared mutable state between goroutines — channels + atomics only |

---

## Design Tradeoffs

| Decision | Why | Consequence |
|---|---|---|
| **Single-goroutine scheduler** | Eliminates all lock contention and ordering bugs. Structurally prevents deadlocks. | Throughput is bounded by single-core speed. Currently handles ~10K tasks/sec — sufficient for orchestration workloads. |
| **fsync per WAL write** | Guarantees no acknowledged task is ever lost, even on power failure. | Higher write latency (~1ms per operation). Acceptable because task execution time dominates (50-200ms). |
| **In-memory DAG state** | Fast lookups, simple implementation, no serialization overhead. | State is lost on crash — rebuilt from WAL replay. Replay time grows with WAL size (no compaction yet). |
| **Single-node only** | Keeps consistency trivial — no consensus, no split-brain, no network partition handling. | Cannot horizontally scale. Suitable for CI/CD pipelines, build systems, ETL — not for planet-scale scheduling. |
| **WAL grows unbounded** | Append-only is crash-safe and simple. Truncation risks data loss if done incorrectly. | Disk usage grows linearly. Compaction/snapshotting is planned but not yet implemented. |
| **Simulated execution** | Allows correctness testing without real workloads. | Workers simulate work with random sleep — production would need real execution backends. |

---

## Production Features

The API layer isn't a thin wrapper — it implements defense-in-depth:

| Feature | Implementation | Why |
|---|---|---|
| **Idempotency** | `Idempotency-Key` header required. Keys stored in-memory, rebuilt from WAL on restart. Duplicate → cached response. | Prevents double-submission from retrying clients |
| **Per-client rate limiting** | Token bucket per IP (`5 req/s`, burst 10). Localhost is exempt for load testing. | Prevents any single client from flooding the scheduler |
| **Backpressure** | If queue depth > 50, API handler spins with 10ms sleep. If > 10K, returns `429`. | Prevents unbounded memory growth from faster ingestion than execution |
| **Payload limits** | 1MB max body, 1000 tasks/request, 50 deps/task. `DisallowUnknownFields()` for strict parsing. | Prevents JSON bombs and malformed payloads |
| **Health probes** | `GET /healthz` (liveness, always 200), `GET /readyz` (readiness, checks scheduler + WAL) | Kubernetes-native deployment readiness |
| **Observability** | Prometheus counters, gauges, histograms at `/metrics`. Structured logging with `slog`. | Production monitoring without custom tooling |

---

## API

### Submit a DAG

```bash
curl -X POST http://localhost:8080/api/v1/dag \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: pipeline-v1" \
  -d '[
    {"id": "build",  "payload": "go build", "priority": 1, "dependencies": []},
    {"id": "test",   "payload": "go test",  "priority": 2, "dependencies": ["build"]},
    {"id": "deploy", "payload": "deploy.sh","priority": 3, "dependencies": ["test"]}
  ]'
# → 201 {"ingested": 3, "status": "accepted"}
```

### System Status
```bash
curl http://localhost:8080/api/v1/status
# → {"pending":0, "running":1, "completed":2, "failed":0, "retried":0}
```

### Health & Metrics
```bash
curl http://localhost:8080/healthz    # → 200 {"status":"alive"}
curl http://localhost:8080/readyz     # → 200 {"status":"ready"} | 503
curl http://localhost:8080/metrics    # → Prometheus scrape format
```

---

## Running the System

### Docker
```bash
docker build -t dag-scheduler .
docker run -d -p 8080:8080 -v wal-data:/app/data dag-scheduler
```

### Docker Compose
```bash
docker compose up -d
```

### Local
```bash
go run ./cmd/server
```

---

## Project Structure

```
go-enterprise-scheduler/
├── cmd/server/              Entry point, wiring, graceful shutdown
├── internal/
│   ├── api/                 Handlers, idempotency, rate limiting
│   ├── engine/
│   │   ├── scheduler.go     Event loop, failure cascade, retry logic
│   │   ├── heap.go          Min-heap priority queue
│   │   └── worker.go        Worker pool, dual-guard cancellation
│   └── storage/
│       └── wal.go           Append-only WAL, replay, corruption handling
├── pkg/
│   ├── models/task.go       Task struct, lifecycle constants
│   └── telemetry/metrics.go Prometheus metric definitions
├── Dockerfile               Multi-stage, non-root, <15MB runtime image
└── docker-compose.yml       One-command deployment with named volume
```

---

## Future Work

- **WAL compaction** — periodic snapshotting to bound replay time
- **Multi-node coordination** — distributed scheduling with leader election
- **Task sharding** — partition DAGs across scheduler instances
- **Real execution backends** — shell commands, containers, gRPC dispatch
- **Persistent store** — replace in-memory maps with embedded KV (BadgerDB)
