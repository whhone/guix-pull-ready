# AGENTS.md

## Project Overview

`guix-pull-ready` finds the newest GNU Guix commit (and optionally coordinated `nonguix` commit) whose prebuilt binary substitutes are ready on Cuirass (`https://ci.guix.gnu.org` and `https://cuirass.nonguix.org`), so `guix pull` and package installations do not trigger local builds from source. It runs as a zero-dependency Go HTTP service (`cmd/guix-pull-ready`, `internal/`) that polls Cuirass in the background, caches state locally, and serves instant responses.

---

## Repository Layout

```text
.
├── cmd/guix-pull-ready/main.go    # Go service entrypoint and CLI flags
├── internal/
│   ├── api/                       # HTTP handlers (/, /channels.scm, /favicon.ico) and HTML dashboard
│   ├── cuirass/                   # Cuirass REST client, JobStatus, nixname parsing, Guix VersionGreater
│   ├── poller/                    # Incremental background poller, /build/<id> reconciler, lazy version resolver
│   ├── resolver/                  # Whole-evaluation, package-list, and multi-channel resolution engine
│   └── store/                     # Gob atomic disk store and lock-free in-memory Snapshot
├── Containerfile                  # Multi-stage static container image build
└── docker-compose.yml             # Container deployment with optional Cloudflare Tunnel
```

---

## Architecture & Key Invariants

1. **No Live Cuirass Calls on the Read Hot Path**:
   - Cuirass endpoints (`/api/jobs`, `/api/evaluations`) are slow and prone to `504 Gateway Time-out`.
   - HTTP handlers read exclusively from `store.Snapshot()` (`atomic.Pointer[Snapshot]`).
   - The only outbound calls allowed during a request are cached `/build/<id>` lookups via `poller.ResolveMissingVersions` when a query specifies a multi-version package (e.g. `gcc-toolchain@14`) whose build nixnames are not yet cached.

2. **Incremental Polling & Immutable Completed Evaluations**:
   - Once a `(channel, evalID, system)` entry has `PendingJobs == 0` and `TotalJobs > 0`, `SystemEvalData.Complete` is set to `true` and it is never re-fetched on subsequent poll cycles.
   - For systems supported by the `guix` jobset (`x86_64-linux`, `i686-linux`), a commit requires both the `master` evaluation and the `guix` jobset evaluation (`PullReady`) to succeed.
   - Additional channels (`nonguix`) are correlated by matching the `guix` checkout commit recorded in the channel evaluation against a ready `guix` evaluation for that same commit.

3. **Stuck Pending Build Reconciliation (`/build/<id>`)**:
   - Upstream Cuirass can leave jobs stuck in pending status (`-3` or `-1`) in `/api/jobs` even after the underlying build finishes.
   - When an evaluation is near completion, the poller cross-checks `/build/<id>` for pending jobs (up to `--max-reconcile-pending`, default 40) and overrides finished statuses in-place so evaluations can transition to `Complete`.

4. **Compact Job Storage (`internal/store`)**:
   - A Guix evaluation has ~35,000 jobs per system. `SystemEvalData` stores single-job packages as 1-byte statuses in a pointer-free `DenseSingleJobs` (`[]int8` indexed by dense package ID) and only allocates slices in `JobsByBase` for multi-version packages (e.g. `gcc-toolchain`, `linux-libre`). Preserve this split when modifying store structures.

5. **Package Spec Resolution (`internal/resolver`)**:
   - Specs strip `:output` suffixes (`bind:utils` -> `bind`) and split `@version` (`gcc-toolchain@14` -> `gcc-toolchain`, version prefix `14`), matching `<name>.<system>` and `base.<name>.<system>`.
   - Packages absent from `Snapshot.IsPackageTracked` are reported in `untracked_packages` and skipped so local/custom packages do not block resolution.
   - Multi-version packages without `@version` select the highest version using `cuirass.VersionGreater` (GNU `strverscmp` / Guix `version>?`).

6. **Zero External Dependencies**:
   - Keep `go.mod` limited to the Go standard library so the binary compiles offline, in minimal containers, and under Guix's `go-build-system`.

---

## HTTP API & Query Parameters

Routes: `GET /` (HTML dashboard), `GET /channels.scm` (`text/x-scheme` channel snippet), `GET /favicon.ico`.

| Parameter | Default | Description |
| :--- | :--- | :--- |
| `system` | `x86_64-linux` | Target architecture (`x86_64-linux`, `aarch64-linux`, `powerpc64le-linux`, `riscv64-linux`) |
| `max_pending` | `0` | Whole-evaluation mode: maximum acceptable pending builds |
| `pkgs` | *(none)* | Comma- or space-separated package specs (e.g. `emacs,git,gcc-toolchain@14`) |
| `additional_channels` | *(none)* | Additional channels to coordinate with Guix (`nonguix`) |
| `allow_failed` | `false` | Package mode: allow failed builds (only pending builds block) |
| `limit` | `40` | Number of recent evaluations to scan (max 100) |

---

## Development & Testing

```bash
# Run all unit and integration tests
go test -v ./...

# Run service locally in-memory (preferred for testing)
go run ./cmd/guix-pull-ready --db-path=:memory:

# Build portable static binary (forces pure-Go netgo DNS resolver instead of glibc)
CGO_ENABLED=0 go build -trimpath -o guix-pull-ready ./cmd/guix-pull-ready

# Smoke-test HTTP endpoints locally
curl -fsSL "http://localhost:8080/channels.scm?pkgs=emacs,git"
```

### Rules
- **Go Builds**: Always use `CGO_ENABLED=0 go build -trimpath` when compiling Go binaries intended for portable execution or container images, ensuring the static pure-Go resolver (`netgo`) is linked instead of glibc `getaddrinfo`.
- **Ephemeral Testing**: When developing or testing the service locally, prefer in-memory storage (`--db-path=:memory:`) instead of repeatedly wiping database files on disk.
