# Guix Pull Ready

Find the newest GNU Guix master commit whose prebuilt binaries (substitutes) are 100% ready on [Cuirass](https://ci.guix.gnu.org), allowing you to run `guix pull` or reconfigure systems without triggering long local builds from source.

> **Note:** This serves as a temporary community solution until official support lands (work on similar functionality upstream in GNU Guix is currently underway).

Public instance: **https://guix-pull-ready.whhone.com**

![Guix Pull Ready](screenshots/guix-pull-ready.png)

## Quick Start

### 1. Update Guix directly to the newest ready commit

```bash
guix pull --commit=$(curl -fsSL https://guix-pull-ready.whhone.com/commit)
```

### 2. Generate a channels.scm specification

```bash
curl -fsSL https://guix-pull-ready.whhone.com/channels.scm > ~/.config/guix/channels.scm
```

### 3. Query for specific packages

Find a commit where your required packages are built (without waiting for the entire evaluation to finish):

```bash
curl -fsSL "https://guix-pull-ready.whhone.com/commit?pkgs=emacs,git"
```

## Self-Hosting

### Run with Podman / Docker Compose

A ready-to-use [`docker-compose.yml`](docker-compose.yml) with Cloudflare Tunnel support is included:

```bash
cp .env.example .env   # Set your TUNNEL_TOKEN if using a Cloudflare named tunnel
podman compose up -d --build
```

### Build from Source (Zero Dependencies)

The server requires only the standard Go toolchain (zero third-party dependencies):

```bash
# Build static binary
CGO_ENABLED=0 go build -trimpath -o guix-sync-server ./cmd/guix-sync-server

# Run locally on :8080
./guix-sync-server --db-path=./guix-sync.db
```

## License

GPL-3.0-or-later
