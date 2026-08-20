# Local Model Works

Local Model Works is a self-hosted control plane for a small heterogeneous AI fleet. It enrolls Linux nodes over mTLS, inventories accelerators and model caches, installs immutable OCI recipe packages, plans multi-node placements, supervises model-serving containers, runs compiler-backed benchmarks, and exposes a React operator console.

## Architecture

- `lmw-server`: Go control plane, SQLite state, authenticated browser/API listener, mTLS agent listener, embedded web console.
- `lmw-agent`: node daemon for hardware inventory, telemetry, Docker lifecycle, artifact transfer, and byte-cursor logs.
- `lmw`: offline operator CLI for account bootstrap, recipe authoring, and DGX Dashboard migration.
- `modules/`: compile-time first-party Fleet, Library, Serving, Benchmarks, Workshop, Runs, and Settings modules.

Recipes and images are digest-pinned. Git sources require full commit hashes. Agents only manage containers bearing Local Model Works ownership labels. Browser mutations require a secure origin-bound session and CSRF token.

## Build and verify

Requirements: Go 1.26, Node.js/npm, Docker, `protoc` through the checked-in Buf configuration, and `aarch64-linux-gnu-gcc` for the arm64 NVML agent release.

```bash
make generate
make test
npm --prefix web run test:e2e
make release
```

`make release` produces Linux amd64 and arm64 bundles plus `dist/SHA256SUMS`.

## Controller installation

Install the matching release binaries and systemd unit, then create the sole operator before starting the service:

```bash
sudo install -m 0755 lmw-server lmw /usr/local/bin/
sudo install -m 0644 deploy/systemd/local-model-works.service /etc/systemd/system/
sudo install -m 0640 deploy/systemd/server.env.example /etc/local-model-works/server.env
sudo lmw admin create --state /var/lib/local-model-works --username operator
sudo systemctl daemon-reload
sudo systemctl enable --now local-model-works.service
```

Edit `/etc/local-model-works/server.env` first. `LMW_PUBLIC_ORIGIN` must be the exact HTTPS browser origin. `LMW_PUBLIC_AGENT_URL` must be the tailnet-reachable HTTPS mTLS endpoint and its hostname must equal `LMW_SERVER_NAME`. Terminate browser TLS in a trusted reverse proxy; do not proxy the agent listener through a component that drops client certificates.

## Agent enrollment

Create a one-use enrollment token in the console, record the controller CA SHA-256 fingerprint, then run as root on each node:

```bash
sudo lmw-agent install \
  --server https://lmw.example.tailnet.ts.net:9443 \
  --ca-sha256 <64-lowercase-hex> \
  --token <64-lowercase-hex> \
  --run-as lmw-agent \
  --peer-advertise node.example.tailnet.ts.net:9444 \
  --cache-root /models
```

The installer writes a root-owned environment file and enables `local-model-works-agent.service`. Enrollment tokens are single-use; node certificates are independently rotatable.

## Recipe workflow

```bash
lmw recipe init --from-git https://github.com/org/model-recipe.git \
  --revision <full-commit-sha> --path recipe --output ./recipe
lmw recipe validate ./recipe
lmw recipe pack ./recipe --output ./recipe.oci
```

Imported Git recipes remain untrusted until an operator approves the stored digest. The Library builder can inspect a source tree, select assets, validate the complete manifest, and package it through a resumable run.

## Migration

The CLI provides offline scan/import commands. The authenticated migration API submits resumable jobs: scans persist a digest-addressed plan; imports require the exact digest and explicit confirmation, re-scan the source, verify it remains untouched, and write only to an isolated staging state root.

## License

Apache License 2.0. See [LICENSE](LICENSE).
