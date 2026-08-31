# Local Model Works

Local Model Works is a self-hosted control plane for a small heterogeneous AI fleet. It enrolls Linux nodes over mTLS, inventories accelerators and model caches, installs immutable OCI recipe packages, plans multi-node placements, supervises model-serving containers, runs compiler-backed benchmarks, and exposes a React operator console.

## Architecture

- `lmw-server`: Go control plane, SQLite state, authenticated browser/API listener, mTLS agent listener, embedded web console.
- `lmw-agent`: node daemon for hardware inventory, telemetry, Docker lifecycle, artifact transfer, and byte-cursor logs.
- `lmw`: offline operator CLI for account bootstrap, recipe authoring, and DGX Dashboard migration.
- `modules/`: compile-time first-party Fleet, Library, Serving, Benchmarks, Workshop, Runs, and Settings modules.

Recipes and images are digest-pinned. Git recipe imports resolve a branch or `HEAD` to one immutable commit before preview or installation; the offline CLI also accepts an explicit full commit hash. Agents only manage containers bearing Local Model Works ownership labels. Browser mutations require a secure origin-bound session and CSRF token.

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

The Library builder remains an authoring surface for inspecting source trees, selecting assets, validating manifests, and packaging resumable runs. Installed Git repositories are cataloged once by normalized URL and source path, with immutable commits beneath them. For native recipe bundles and registered deterministic compilers, the Library previews the exact hardware and added or removed permissions for a pinned newer commit, replaces deployments on the same nodes and ranks without executing the third-party repository, and reports durable per-hardware progress with automatic restoration on failure. Permission previews are informational; a recipe that passes validation is launchable.

### GLM-5.3 Flash on two DGX Sparks

The Library has deterministic compilers for the EXL3 target and [`tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark`](https://github.com/tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark). Choose **Import recipe**, paste the repository URL, optionally pin a full commit, and review the generated immutable contract before launch. LMW treats the repository as data and does not execute its scripts during import.

The NVFP4 contract presents three deliberate model choices: the stable censored RedHatAI W4A4 checkpoint is recommended and selected by default; the legacy censored ModelOpt checkpoint and abliterated uncensored checkpoint remain available with their corrupted-token and gating risks shown inline. The image, target and DFlash2 snapshots, sizes, two Spark roles, fabric requirements, elevated permissions, and upstream commit are all pinned in the review.

Before launch, create or repair a healthy RoCE fabric in **Fleet → Fabrics**. Each selected member records its own fabric address, network interface, RDMA device, and GID index, so asymmetric device names are supported. The launch confirmation keeps infrastructure detail out of the default path: it shows the selected target, endpoint, and whether artifacts are installed, require an origin download, or will be copied from another node. Multi-node recipes expose the actionable **Head · API** and worker assignments; blocking compatibility or fabric diagnostics remain visible.

Launch settings belong to the operator rather than the recipe. A saved launch profile is a named, digest-pinned combination of declared artifact variants and validated parameters; changing a saved value selects **Custom**, and profiles can be saved, updated, or deleted in the launcher. Single-value declarations stay hidden. The managed Qwen RTX 6000 Pro recipe therefore launches without unnecessary controls while still pinning its model, DFlash2 drafter, and FP8 KV-cache setting.

Each node then reports durable per-file and aggregate artifact progress. Hugging Face snapshots download with bounded concurrency and HTTP retries, resume partial files after stop or reconnect, and become eligible for placement only after every pinned file is size/digest validated and a completion manifest is committed. Peer copies report the same durable progress; a node with partial origin data resumes it instead of discarding it for a full peer copy.

The TP2 runtime prepares both hosts, starts the worker before the API head, and sizes FP8 KV cache from a live memory budget calibrated to preserve Spark control-plane headroom, without a fixed cache override. **Stop** cancels active preparation without deleting partial or verified cache data; **Start** revalidates the same placement and creates a fresh durable run. After `/health` passes, use **Verify** for the live probe or **Open Chat** to exercise `glm-5.3-flash`.

## Serving lifecycle

Serving separates deployments that still require operator attention from fully stopped history. A deployment remains in health check until its configured readiness probe succeeds; a running container alone is not reported healthy. An unexpected container exit records the rank, container, exit code, OOM state, runtime error, and persisted rank-specific logs; the controller then stops every remaining rank and releases leases only after each workload is confirmed down. Recovery is explicit: open the stopped deployment and choose **Restart** to create a fresh run with a newly planned, transactionally persisted placement. Repeated **Retry stop** actions are safe, and offline ranks retain their leases until reconnect confirms that the hardware is free.

## Migration

The CLI provides offline scan/import commands. The authenticated migration API submits resumable jobs: scans persist a digest-addressed plan; imports require the exact digest and explicit confirmation, re-scan the source, verify it remains untouched, and write only to an isolated staging state root.

## License

Apache License 2.0. See [LICENSE](LICENSE).
