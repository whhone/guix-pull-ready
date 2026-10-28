# AGENTS.md

## Project Overview

`guix-evaluated` provides tools and a public HTTP service to find the newest GNU Guix commit whose substitutes (prebuilt binaries) are ready on `https://ci.guix.gnu.org` (Cuirass), allowing users to run `guix pull` or update `channels.scm` without triggering local source builds.

The repository contains two implementations:
1. **`guix-sync-commit.scm`**: Standalone Guile Scheme CLI script (`guix repl -- guix-sync-commit.scm [OPTIONS] [SPEC...]`).
2. **`guix-sync-server`**: Zero-dependency Go HTTP service that polls Cuirass in the background, caches evaluation and job states locally, and serves instant HTTP responses (`text/plain`, `channels.scm`, and `application/json`).

---

## Repository Layout

```text
.
├── cmd/guix-sync-server/main.go   # Go service entrypoint, CLI flags, env vars
├── internal/
│   ├── api/                       # HTTP handlers (/commit, /channels.scm, /status, /evaluations, /healthz, /)
│   ├── cuirass/                   # Cuirass REST client, JobStatus enum, nixname parsing, Guix VersionGreater
│   ├── poller/                    # Incremental background poller and lazy /build/<id> version resolver
│   ├── resolver/                  # Whole-evaluation and package-list query resolution engine
│   └── store/                     # Crash-safe atomic disk store and lock-free in-memory Snapshot
├── Containerfile                  # Multi-stage static container image build
├── go.mod                         # Go module definition (100% standard library, zero external dependencies)
└── guix-sync-commit.scm           # Original Guile Scheme CLI reference implementation
```

---

## Architecture & Key Invariants

1. **No Live Cuirass Calls on the Read Hot Path**:
   - Cuirass endpoints (`/api/jobs`, `/api/jobs/history`) are slow and prone to `504 Gateway Time-out`.
   - Public HTTP requests read from `store.Snapshot()` (an `atomic.Pointer[Snapshot]`) in memory.
   - The only outbound calls that may happen on-demand are cached `/build/<id>` lookups via `poller.ResolveMissingVersions` when a user queries a multi-version package (e.g. `gcc-toolchain@14`) whose build IDs have not yet been resolved.

2. **Incremental Polling & Immutable Completed Evaluations**:
   - Once an evaluation for a target system has `PendingJobs == 0` and `TotalJobs > 0`, `SystemEvalData.Complete` is set to `true`.
   - Completed `(evalID, system)` entries are never re-fetched on subsequent poll cycles.

3. **Stuck Pending Build Reconciliation (`/build/<id>`)**:
   - Upstream Cuirass occasionally suffers from database desync where a job remains stuck with status `-3` (scheduled) or `-1` (started) in `/api/jobs`, even though the underlying build succeeded (e.g. built under another jobset like `science-team`).
   - When an evaluation has $\le \text{MaxReconcilePending}$ (default 10) pending builds, the poller cross-checks `/build/<id>`. If `finished == 1`, it overrides the job status in-place with the canonical `JobStatus(buildstatus)`, allowing evaluations to cleanly transition to `Complete`.

4. **Cuirass Job Status Codes**:
   - Pending (`IsPending()`): `-3` (`scheduled`), `-2` (`submitted`), `-1` (`started`)
   - Succeeded (`IsSucceeded()`): `0` (`succeeded`)
   - Failed (`IsFailed()`): `1` (`failed`), `2` (`failed-dependency`), `3` (`failed-other`), `4` (`canceled`)

4. **Package Spec Resolution (`internal/resolver`)**:
   - Package specs strip `:output` suffixes (`bind:utils` -> `bind`) and split `@version` (`gcc-toolchain@14` -> name `gcc-toolchain`, version prefix `14`).
   - Both `<name>.<system>` and `base.<name>.<system>` job names are matched.
   - Specs not present in any cached evaluation (`TrackedBasePackages`) are reported in `untracked_packages` and skipped so custom/local channel packages do not block resolution.
   - When a package has multiple jobs in an evaluation and no `@version` is specified, the highest version according to `cuirass.VersionGreater` (GNU `strverscmp` / Guix `version>?`) is selected.

5. **Zero External Dependencies**:
   - Keep `go.mod` free of third-party dependencies so the project compiles offline, in minimal containers, and with Guix's `go-build-system` without extra Go library packaging.

---

## Common Commands

### Build & Test (Go Service)
```bash
# Run all unit and integration tests
go test -v ./...

# Build the static binary
CGO_ENABLED=0 go build -trimpath -o guix-sync-server ./cmd/guix-sync-server

# Run directly in a single command (no build step needed)
go run ./cmd/guix-sync-server

# Run directly in-memory without writing to disk
go run ./cmd/guix-sync-server --db-path=:memory:

# Run locally with custom options
./guix-sync-server --listen=:8080 --db-path=./guix-sync.db --systems=x86_64-linux,aarch64-linux --limit=40
```

**Why `CGO_ENABLED=0` for static builds:**
- By default on Linux (`CGO_ENABLED=1`), Go's standard library `net` package links against the host's C library (`glibc` / `libc.so.6`) for `getaddrinfo()` DNS resolution, producing a dynamically linked binary with a hardcoded ELF interpreter path (such as `/lib64/ld-linux-x86-64.so.2` or `/gnu/store/...-glibc-.../lib/ld-linux-x86-64.so.2`).
- Setting `CGO_ENABLED=0` forces Go to use its built-in pure-Go DNS resolver (`netgo`), producing a self-contained static binary that runs across standard Linux distributions, GNU Guix (non-FHS), and minimal `libc`-less container images (`Containerfile`). For local development on the same host (`go build` or `go run`), `CGO_ENABLED=0` is optional.

### Run Reference Guile Script
```bash
# Whole-evaluation mode
guix repl -- guix-sync-commit.scm --max-pending=0

# Package mode
guix repl -- guix-sync-commit.scm emacs git gcc-toolchain@14
```

---

## HTTP API Endpoints & Curl Examples

The service listens on `:8080` by default. All endpoints support both GET query parameters and POST request bodies for passing package lists.

### Query Parameters

| Parameter | Default | Description |
| :--- | :--- | :--- |
| `system` | `x86_64-linux` | Target architecture (e.g. `x86_64-linux`, `aarch64-linux`) |
| `max_pending` | `0` | Whole-evaluation mode: maximum acceptable pending builds |
| `pkgs` | *(none)* | Comma-separated or space-separated package specs (e.g. `emacs,git,gcc-toolchain@14`) |
| `additional_channels` | *(none)* | Comma-separated additional channels to coordinate with Guix (e.g. `nonguix`) |
| `allow_failed` | `false` | Package mode: allow failed builds (only pending builds block) |
| `limit` | `40` | Number of recent master evaluations to scan (max 100) |

### Endpoints

#### 1. Plain-Text Commit Hash (`GET /commit`)
Returns raw 40-character Git commit hash followed by a newline (`text/plain`). Ideal for shell command substitution.

```bash
# Get latest ready commit for current architecture (whole-evaluation mode)
curl -fsSL "http://localhost:8080/commit"

# Package mode: find commit where emacs, git, and gcc-toolchain 14 are ready
curl -fsSL "http://localhost:8080/commit?pkgs=emacs,git,gcc-toolchain@14"

# POST package list from installed packages (avoids URL length limits)
guix package --list-installed | cut -f1 | curl -fsSL --data-binary @- "http://localhost:8080/commit"
```

#### 2. Channel Specification (`GET /channels.scm`)
Returns a Scheme snippet (`text/x-scheme`) with evaluation metadata in comments, ready for `guix pull -C`, `downloaded-channels`, `~/.config/guix/channels.scm`, or `guix time-machine`.

```bash
# Pull latest ready commit directly
guix pull -C <(curl -fsSL "http://localhost:8080/channels.scm")

# Pull both guix and nonguix at their coordinated ready commits
guix pull -C <(curl -fsSL "http://localhost:8080/channels.scm?additional_channels=nonguix")

# Overwrite channels.scm with latest ready commit
curl -fsSL "http://localhost:8080/channels.scm?system=x86_64-linux" > ~/.config/guix/channels.scm

# Run an ephemeral shell with guaranteed prebuilt binaries
guix time-machine -C <(curl -fsSL "http://localhost:8080/channels.scm?pkgs=emacs,git") -- shell emacs git
```

#### 3. Evaluation Status Details (`GET /status`)
Returns full JSON metadata (`application/json`) including commit, evaluation ID, lag behind master, job counts, and `guix pull` readiness.

```bash
# Inspect status for default architecture
curl -fsSL "http://localhost:8080/status?system=x86_64-linux"

# Status for package query (includes untracked package detection)
curl -fsSL "http://localhost:8080/status?pkgs=emacs,git,my-custom-pkg"
```

#### 4. Cached Evaluations List (`GET /evaluations`)
Returns recent evaluations tracked in local cache with job counts and pending build samples.

```bash
# View last 10 evaluations for aarch64-linux
curl -fsSL "http://localhost:8080/evaluations?system=aarch64-linux&limit=10"
```

#### 5. Health Check (`GET /healthz`)
Returns service and poller status for monitoring and container health checks.

```bash
curl -fsSL "http://localhost:8080/healthz"
```

#### 6. Service Overview & Usage (`GET /`)
- `curl http://localhost:8080/` returns a concise plain-text cheat sheet.
- Browser visits return an HTML dashboard table of ready commits and copyable commands.
- `curl -H "Accept: application/json" http://localhost:8080/` returns a JSON overview across all tracked systems.

---

## Style & Contribution Rules

- **Conventional Commits**: Always format commit messages following the Conventional Commits specification (e.g., `feat(api): ...`, `fix(poller): ...`, `docs: ...`).
- **No Em-Dashes**: Avoid using em-dashes in code comments, documentation, and commit messages. Use parentheses `( )` or commas `,` instead.
- **Maintain Parity**: When modifying resolution semantics in `internal/resolver` or `guix-sync-commit.scm`, keep behavior consistent between both implementations and add corresponding test cases in `internal/resolver/resolver_test.go`.
