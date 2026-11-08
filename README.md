# Guix Pull Ready

`guix pull` to the newest commit where prebuilt binaries are 100% ready on substitutes servers, so updating your system never triggers slow local builds from source. Optionally coordinates with [Nonguix](https://gitlab.com/nonguix/nonguix).

> **Note:** This serves as a temporary community solution until official support lands (work on similar functionality upstream in GNU Guix is currently underway).

Public instance: **https://guix-pull-ready.whhone.com**

## How It Works

1. **Evaluations and Jobs**: When commits are pushed to GNU Guix, the build farm ([Cuirass](https://ci.guix.gnu.org)) creates an **evaluation** with build **jobs** for all packages on each architecture (e.g. `x86_64-linux`).
2. **Background Sync**: This service polls Cuirass in the background and caches job statuses locally, ensuring instant responses without upstream CI timeouts.
3. **Ready Commit Lookup**:
   - **Whole system (default)**: Finds the newest commit whose evaluation has 0 pending builds remaining (and where the `guix` jobset for `guix pull` is ready).
   - **Package mode (`?pkgs=...`)**: Finds the newest commit where your selected packages are already built, without waiting for the rest of the distribution to finish.
   - **Coordinated Nonguix channel (`?additional_channels=nonguix`)**: Matches evaluations on [`cuirass.nonguix.org`](https://cuirass.nonguix.org/jobset/nonguix) with the exact `guix` commit they were evaluated against on [`ci.guix.gnu.org`](https://ci.guix.gnu.org), ensuring substitutes are ready across both channels simultaneously.

## Quick Start

### 1. Update Guix directly to the newest ready commit

```bash
guix pull -C <(curl -fsSL "https://guix-pull-ready.whhone.com/channels.scm")
```

### 2. Track automatically via channels.scm

Configure `~/.config/guix/channels.scm` to automatically fetch the newest ready commit on every `guix pull` using Guix's built-in `downloaded-channels`:

```scheme
(downloaded-channels
  "https://guix-pull-ready.whhone.com/channels.scm")
```

To learn more, see the GNU Guix manual section on [Channels with Substitutes](https://guix.gnu.org/manual/devel/en/html_node/Channels-with-Substitutes.html).

### 3. Include the Nonguix channel (`?additional_channels=nonguix`)

> **IMPORTANT:**
> 1. `nonguix` substitutes require manual authorization. Follow [Substitutes for Nonguix](https://gitlab.com/nonguix/nonguix/#substitutes-for-nonguix) to authorize the Nonguix substitute server signing key.
> 2. When tracking channels automatically via `downloaded-channels`, GNU Guix verifies downloaded channels against your trusted channels list. If you encounter an untrusted channel error, see [Untrusted Channel Errors with downloaded-channels](#untrusted-channel-errors-with-downloaded-channels-configguixtrusted-channelsscm) below.

Pull both `guix` and `nonguix` at their newest coordinated ready commits:

```bash
guix pull -C <(curl -fsSL "https://guix-pull-ready.whhone.com/channels.scm?additional_channels=nonguix")
```

Or track both channels automatically in `~/.config/guix/channels.scm`:

```scheme
(downloaded-channels
  "https://guix-pull-ready.whhone.com/channels.scm?additional_channels=nonguix")
```

### 4. Query for specific packages

Find a commit where your required packages are built (without waiting for the entire evaluation to finish):

```bash
guix pull -C <(curl -fsSL "https://guix-pull-ready.whhone.com/channels.scm?pkgs=emacs,git")

# With nonguix packages (e.g. linux, nvidia-driver):
guix pull -C <(curl -fsSL "https://guix-pull-ready.whhone.com/channels.scm?additional_channels=nonguix&pkgs=emacs,linux")
```

## Self-Hosting

### Run with Podman / Docker Compose

A ready-to-use [`docker-compose.yml`](docker-compose.yml) with Cloudflare Tunnel support is included. By default, it runs the prebuilt image from GitHub Container Registry (`ghcr.io/whhone/guix-pull-ready:latest`):

```bash
cp .env.example .env   # Set your TUNNEL_TOKEN if using a Cloudflare named tunnel

# Run prebuilt image from GHCR (no local build needed)
podman compose pull && podman compose up -d
# or with docker compose:
docker compose pull && docker compose up -d
```

To build locally from source instead of pulling from GHCR, pass `--build`:

```bash
podman compose up -d --build
```

### Build from Source (Zero Dependencies)

The server requires only the standard Go toolchain (zero third-party dependencies):

```bash
# Build static binary
CGO_ENABLED=0 go build -trimpath -o guix-pull-ready ./cmd/guix-pull-ready

# Run locally on :8080
./guix-pull-ready --db-path=./guix-pull-ready.db
```

## Known Issues & Automatic Reconciliation

### Still Building Locally

After pulling from `guix-pull-ready`, you may occasionally find that a specific package (such as `github-cli`) still builds locally during `guix system reconfigure`, `guix home reconfigure`, or `guix install`, and `guix weather <pkg>` reports `0.0% substitutes available` with `0.0% of the missing items are queued`.

- **Root Cause**: `guix-pull-ready` selects the newest evaluation with `0` **pending** builds. It does not require `0` **failed** builds, because across ~35,000 packages in a full Guix evaluation, hundreds or thousands of jobs always fail on CI (`Failed`, `Failed (dependency)`, or `Canceled`). Once all scheduled builds finish, the evaluation has `0` pending jobs even though individual packages failed and produced no substitutes.
- **Will the Local Build Succeed or Fail?**:
  - **Local build succeeds (fixed-output cutoff or alternate substitute server)**: If Cuirass aborted the package with `Failed (dependency)` because a tool needed by a fixed-output derivation failed (for example, `github-cli` -> `go-github-com-charmbracelet-huh` -> `go-github-com-charmbracelet-huh-checkout` -> `git-lfs` -> `ronn-ng` -> ... -> `ruby-mysql2`), the fixed-output checkout store path (`r:sha256`) is content-addressed and often already available as a substitute from an earlier evaluation or `bordeaux.guix.gnu.org`. Your local `guix-daemon` substitutes the intermediate dependency, skips the broken upstream chain, and only compiles the top-level package locally.
  - **Local build fails (direct deterministic failure)**: If the package itself (or a required dependency with no available substitute) failed to compile or pass tests (`Failed`), the local isolated build will fail identically.

### Cuirass Upstream `Jobs` Cache Desynchronization

Occasionally, an upstream evaluation may appear stuck with a small number of builds (e.g. 1 to 4 packages) marked as pending indefinitely, even though the underlying builds have already succeeded.

- **Root Cause**: An upstream database race condition in Cuirass (`ci.guix.gnu.org`). Cuirass caches build statuses in a denormalized `Jobs` table populated during evaluation registration, kept in sync with the canonical `Builds` table via a PostgreSQL trigger (`update_job_status`). When an existing build completes under another jobset (such as `science-team`) or concurrently while a new evaluation is being registered, the completion trigger updates existing evaluation rows but can miss the newly inserted row. Once a build completes (`status = 0`), Cuirass never issues another `UPDATE` on `Builds` for that derivation, leaving `/api/jobs` permanently reporting `status = -3` (scheduled) or `status = -1` (started) for that evaluation in upstream CI.
- **How Reconciliation Works**: `guix-pull-ready` automatically reconciles stuck pending jobs. When an evaluation is near completion, the poller queries Cuirass's canonical `/build/<id>` endpoint for pending builds (up to 40 builds per cycle, configurable via `--max-reconcile-pending`). If the build record indicates completion (`finished == 1`), the poller overrides the desynchronized job status with the canonical build status (e.g. succeeded), allowing the evaluation to reach 0 pending builds and be marked complete.
- **Fallback**: Package-mode queries (`?pkgs=...`) for packages not affected by the desynchronized jobs are unaffected even before reconciliation occurs.

### Untrusted Channel Errors with `downloaded-channels` (`~/.config/guix/trusted-channels.scm`)

When using `downloaded-channels` to fetch channels over HTTP, GNU Guix executes the downloaded Scheme file in an isolated environment and verifies all specified channels against your trusted channels list. This safeguards against arbitrary code execution in case a remote server is compromised or returns malicious channel code.

#### How Channel Authentication Works

1. **Identification by Cryptographic Introduction:** Guix does not match trusted channels solely by name or repository URL. Instead, it identifies them by their cryptographic **channel introduction** (`make-channel-introduction`), which defines the initial commit and developer OpenPGP signing key.
2. **Default Fallback:** If `~/.config/guix/trusted-channels.scm` does not exist, Guix automatically trusts only the channels currently active on your system (the output of `guix describe`).
3. **Why Nonguix Can Fail on First Use:** If you add `?additional_channels=nonguix` via `downloaded-channels` on a machine that has not previously pulled or configured `nonguix`, Guix rejects the downloaded channel specification as untrusted.

#### Solution

Define your trusted channels explicitly in `~/.config/guix/trusted-channels.scm`:

```scheme
;; ~/.config/guix/trusted-channels.scm
(list
  ;; Official GNU Guix channel
  (channel
    (name 'guix)
    (url "https://codeberg.org/guix/guix.git")
    (introduction
      (make-channel-introduction
        "9edb3f66fd807b096b48283debdcddccfea34bad"
        (openpgp-fingerprint
          "BBB0 2DDF 2CEA F6A8 0D1D  E643 A2A0 6DF2 A33A 54FA"))))

  ;; Nonguix channel
  (channel
    (name 'nonguix)
    (url "https://gitlab.com/nonguix/nonguix")
    (introduction
      (make-channel-introduction
        "897c1a470da759236cc11798f4e0a5f7d4d59fbc"
        (openpgp-fingerprint
          "2A39 3FFF 68F4 EF7A 3D29  12AF 6F51 20A0 22FB B2D5")))))
```

Once saved, `guix pull` will authenticate both `guix` and `nonguix` from `downloaded-channels` without security warnings or aborts.

## License

GPL-3.0-or-later
