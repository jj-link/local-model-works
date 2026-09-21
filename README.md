# Local Model Works

Local Model Works is a self-hosted control plane for a small heterogeneous AI fleet. It enrolls Linux nodes over mTLS, inventories accelerators and model caches, installs immutable OCI recipe packages, plans multi-node placements, supervises source-owned upstream procedures and native recipe workloads, runs compiler-backed benchmarks, and exposes a React operator console.

## Architecture

- `lmw-server`: Go control plane, SQLite state, authenticated browser/API listener, mTLS agent listener, embedded web console.
- `lmw-agent`: node daemon for hardware inventory, telemetry, reviewed upstream installation/lifecycle, Docker lifecycle, artifact transfer, and byte-cursor logs.
- `lmw`: offline operator CLI for recipe inspection and packaging, account bootstrap, and DGX Dashboard migration.
- `modules/`: compile-time first-party Fleet, Library, Serving, Benchmarks, Workshop, Runs, and Settings modules.

Recipe packages are digest-pinned. Git imports resolve a branch or `HEAD` to one immutable commit before preview or installation; the offline CLI also accepts an explicit full commit hash. Native managed-container recipes pin images and use Local Model Works ownership labels. Source-owned procedures execute original upstream scripts at the exact reviewed revision with separately approved host authority; upstream chooses its dependencies and container names. Browser mutations require a secure origin-bound session and CSRF token.

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

### Controllers hosted in WSL

Systemd services, including user services with lingering enabled, do not keep a WSL distribution alive after its Windows clients exit. For an always-running controller, set the native lifetime policy in the Windows user's `%UserProfile%\.wslconfig`, preserving existing sections:

```ini
[general]
instanceIdleTimeout=-1
```

This setting applies to all WSL distributions owned by that Windows user: once started, they remain running until explicitly stopped. It requires no keep-alive process or scheduled task. The [WSL 2.7.13 implementation](https://github.com/microsoft/WSL/blob/2.7.13/src/windows/service/exe/LxssUserSession.cpp#L2658-L2676) treats a negative instance timeout as disabling idle termination; `wsl2.vmIdleTimeout` controls a different, VM-level timer.

Apply the policy on a fresh instance start, accounting for other services before stopping a running distribution. This is not automatic startup after a Windows reboot or WSL upgrade; start the distribution again after that maintenance. Verify the HTTPS login page remains available after all `wsl.exe` client commands have exited and at least 90 seconds have passed, without a terminal or helper process holding the distribution open.

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

The installer writes a root-owned environment file, the service unit, and policy drop-ins; it does not start or restart `local-model-works-agent.service`. Enrollment tokens are single-use; node certificates are independently rotatable. After provisioning the service account and required access, run `sudo systemctl daemon-reload` and `sudo systemctl enable --now local-model-works-agent.service`.

Upstream host execution is **disabled by default**. To opt in, a Linux administrator must explicitly select both execution and host policy alongside the enrollment options:

```bash
sudo lmw-agent install \
  --server https://lmw.example.tailnet.ts.net:9443 \
  --ca-sha256 <64-lowercase-hex> \
  --token <64-lowercase-hex> \
  --run-as lmw-agent \
  --peer-advertise node.example.tailnet.ts.net:9444 \
  --cache-root /models \
  --upstream-execution \
  --upstream-host-policy=host
```

Host policy relaxes systemd filesystem/home, privilege, kernel and control-group confinement and shares host `/tmp`; it is **not a sandbox**. It does not grant Docker group membership, SSH keys, filesystem/home access rights, or sudo. Provision the service account with the privileges required by the reviewed source separately. Each launch also requires the user's explicit `host.upstream-exec` approval of its exact source, commands, configuration and devices.

Approved workers deliberately survive agent service stop/restart. Disabling execution does not terminate them. Stop workloads before retiring the agent or reverting to hardened policy; retain `--upstream-host-policy=host` with `--upstream-execution=false` when disabling new execution without changing worker survival.

## Recipe workflow

```bash
lmw recipe init --from-git https://github.com/org/model-recipe.git \
  --revision <full-commit-sha> --path recipe --output ./recipe
lmw recipe validate ./recipe
lmw recipe pack ./recipe --output ./recipe.oci
```

**Recipes → Catalog** is the shared entry point. Each documented launch procedure has a compact overview with its model/hardware identity, saved version, upstream link, and separate saved-package and deployment status. **Run on device**, **Update**, and **Settings** are the primary actions. Run settings apply only to the next reviewed launch; editing or resetting them does not change the saved recipe or running deployments. Procedures from the same repository remain separate catalog entries.

**Run on device** uses the same menu for every recipe. Choose the device explicitly (head and workers for a multi-device recipe); the recipe's default configuration is applied and readiness is checked automatically. **Settings** starts collapsed and contains searchable settings, optional profiles and file-download policies. **Cancel** and **Run** stay visible while settings scroll. There is no browser-recovery prompt or separate run-plan confirmation.

Required worker account, directory and cache settings are filled from the selected agents' reported facts when the source directly supports those inputs. Explicit settings, source defaults and saved profiles take precedence; unavailable facts or unrelated required inputs remain blockers rather than invented defaults.

Missing required `WORKER_SSH`/`WORKER_HOST` settings prefer an existing SSH alias in the selected head agent account whose effective user and destination match the selected worker. Before reporting Ready, source-owned worker SSH inputs are checked from that account with a bounded, noninteractive remote `true` command. Explicit targets and source-bound fabric IPs are checked exactly as rendered, not silently replaced. The check requires an updated, host-execution-enabled head agent, existing credentials and trusted host entries; it does not change SSH keys, configuration or trust. Executable `Match exec` configuration and proxy transports are unsupported and block preflight. **Retry readiness check** repeats the check without launching anything. Restarting a saved deployment rechecks its frozen target; new defaults do not migrate old deployments.

App-managed files are reused, with missing downloads authorized only by **Run**. For source-owned recipes, that same click authorizes the reviewed source's host execution; its scripts control their own file reuse, downloads and builds.

The six maintained upstream procedures expose reviewed environment inputs, existing vLLM/SGLang options, Docker runtime choices and Compose settings through searchable setting groups. Optional overrides preserve upstream defaults until enabled. Runtime bindings validate the original source SHA-256 and exact spans and encode selected values for their shell, remote-shell, heredoc or Compose context. Device, GPU and fabric bindings remain controlled by the reviewed placement.

Choose **Update** from Serving or the recipe overview to check upstream, automatically verify a supported target procedure, and prepare its immutable saved package. The confirmation lists the exact devices with installed versions; it has no setup fields or deployment selection. **Install update** acquires the verified package on those devices and prepares source-owned successor checkouts using saved settings and frozen device bindings. Changed source, settings, or target devices invalidate approval. Per-device progress remains available in operation history; cached package bytes alone do not count as completed source preparation.

Saved recipe overrides and authenticated installation-local source/configuration edits are carried forward automatically. Independent upstream changes are retained; overlapping text edits prefer the local installation. Target configuration bindings use the new source locations. Existing source workspaces remain intact, including when the target checkout already exists. Unsafe links, unverified history, unsupported simultaneous binary edits, or preservation limits fail explicitly instead of discarding installation changes.

This update path does not execute authored install/start/stop commands or change existing deployment pins. Historical native installations receive the verified package without invented source, SSH, or home-directory migration settings. Source lifecycle execution remains a separate launch operation. Preservation covers recorded source/configuration changes, not arbitrary host dependencies, image contents, or a host snapshot.

From **Serving**, open an existing deployment and choose **Configure**. This also supports native and catalog deployments without a linked repository. Settings start from the deployment's saved values; where version history is available, switching versions preserves each version's unsaved edits. Review changed fields, effective parameters, variants, devices and fabric, and the complete upstream launch contract before approving replacement. Applying affects only that deployment. Failure attempts restoration of its saved running or stopped state, not rollback of host or dependency changes. Merely editing or reviewing settings does not stop a server.

**Technical details** contains the exact repository, revision, source directory, authored lifecycle commands, manifest and provenance. **Version history** retains access to immutable saved packages. AI assistance is a secondary action, not a prerequisite for editing supported run inputs.

**Add from GitHub** pins and inventories the repository without executing its code. Investigation reads source in bounded requests, including CI build and devcontainer definitions, and can follow explicitly linked public instructions. Excluded or inaccessible files and unresolved facts remain visible. Review the exact source evidence, lifecycle, supported configuration, hardware and host requirements before accepting a procedure. Only reviewed revisions execute; a new revision or unsupported procedure requires evidence and review, not a silent repin of an old launch contract.

Choose an **AI assistant / model** in GitHub investigation; enrolled deployments, a connected Codex account, and configured providers are available through **Settings → AI providers**. AI assists evidence gathering, investigation and troubleshooting; it cannot generate executable substitutions, copy launchers, compose patches, rewrite source, or select replacement images/models for upstream execution. Missing facts require questions and further review. Before sharing source or failure logs, review the exact content, destination and request and consent explicitly; investigation can make multiple provider requests. Suggestions remain inert until accepted into reviewable drafts.

**Save recipe to library** saves an immutable reviewed contract; it does not install source on devices, download model resources, or change running deployments. The app owns approval, installation records, history, status, logs, start/stop and selected replacement. On the device it checks out the exact repository/revision and invokes the source-authored lifecycle, applying only reviewed inputs through the source's supported arguments, environment or configuration file. It does not replace upstream scripts with an application-authored serving stack.

Source scripts own network access, downloads, builds, caches, image/model choices and dependencies. A source commit does not pin mutable images or model downloads chosen by those scripts. Managed-resource inventory and the existing-file acquisition policy cannot restrict upstream operations, and managed-container safeguards do not contain their host authority.

**Edit saved recipe definition** (under Technical details) and **Review request and source** (under Ask AI about a change) create independent editable work without automatically sending it to an assistant. Review changed fields before saving. Source updates require evidence from the exact target revision and review of its authored lifecycle, permissions and target-supported inputs; an old helper or unsupported setting cannot stand in for target evidence. Changing approved inputs requires fresh review and approval.

To update running models, select exact deployments under **Replace selected running versions**, review the saved target and each deployment's target-supported inputs, and confirm the interruption. Unselected deployments are unchanged. Old catalog versions and source installations are retained, and failure attempts restoration of selected old running versions. Restoration is not guaranteed, does not roll back the saved catalog version, and is **not a host or dependency snapshot**: source-script changes to the host, caches, external resources or dependencies may remain.

### Native recipe bundles and managed resources

Native source-authored `recipe.yaml`/`recipe.json` bundles remain supported without reconstructing their runtime. Their declared package assets, images and model resources retain the generic packaging and acquisition workflow. Public registry references are verified and pinned to digests; verification failures block saving.

On **Devices**, review **Download to device** separately from running. Its frozen plan lists all declared model/data files, platform-specific images, helper packages, exact sources, credentials, destinations, storage, and staging requirements. Verified reuse and supported partial-data recovery precede eligible peer or origin acquisition; unknown identity, integrity, or storage requirements block submission. Download eligibility does not reserve GPUs or require an idle serving device. Completion requires every declared resource to be verified, not merely a successful transfer or an existing directory.

**Run on device** defaults to verified existing files only. Missing or changed resources block every launch gate without implicitly fetching them. **Download and run** is a separate explicit choice with a fresh resource review. Runtime settings, ranks, permissions, host preparation, and hardware suitability still require their own launch plan.

Each node reports durable per-file and aggregate acquisition progress. Interrupted downloads retain partial and verified data; reconnect and controller or agent restart do not silently replay them. **Resume** requires a fresh review of the original immutable content, runtime, devices, and destinations. Cancellation retains writer ownership until the agent confirms quiescence. Hugging Face snapshots become reusable only after every pinned file is validated and a completion manifest is committed; resumed peer copies revalidate retained bytes before appending.

On Linux agents, successful SHA-256 calculations are reused while the file identity, size, permissions, modification time, and change time remain unchanged. Launch checks and peer manifests therefore do not reread unchanged model weights at every stage. Downloads and repairs still require matching checksums; a changed file is hashed again, and a missing or mismatched file blocks launch. This bounded cache is process-local: agent restart or cache eviction requires fresh verification. Platforms without supported change-time metadata continue full verification.

Native managed workloads run without container-imposed CPU quotas, CPU affinity restrictions, or RAM ceilings. Recipes do not expose these controls. Upstream scripts own their own runtime options; device inventory and hardware checks do not constrain their host execution.

### Reviewed upstream procedures

Six shipped source-owned procedures cover DeepSeek V4 Flash Vision on two Sparks, Qwen3.8 27B on RTX 6000 Pro or one Spark, GLM-5.3 Flash EXL3 on two Sparks, and Qwen3.8 Flash Next on one or two Sparks. They record reviewed source lifecycles and supported inputs, not copied helpers, application-composed patch stacks, or replacement image/model selections. Import and package validation do not establish live Spark/Docker execution or GPU inference quality.

The previous [`tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark`](https://github.com/tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark) adaptation is blocked: reviewed revision `050081dc41ce6edd4d3f15fa19dc3410ba4210e3` has no authored TP2 stop procedure and hardcodes its author's fabric addresses and port 8000. An applicable complete upstream lifecycle must be reviewed; LMW will not edit those constants or silently substitute a generated launcher.

## Serving lifecycle

Serving separates deployments that still require operator attention from fully stopped history. A deployment remains in health check until its configured readiness probe succeeds; a running container alone is not reported healthy. An unexpected container exit records the rank, container, exit code, OOM state, runtime error, and persisted rank-specific logs; the controller then stops every remaining rank and releases leases only after each workload is confirmed down. Recovery is explicit: open the stopped deployment and choose **Restart** to create a fresh run with a newly planned, transactionally persisted placement. Repeated **Retry stop** actions are safe, and offline ranks retain their leases until reconnect confirms that the hardware is free.

## Migration

The CLI provides offline scan/import commands. The authenticated migration API submits resumable jobs: scans persist a digest-addressed plan; imports require the exact digest and explicit confirmation, re-scan the source, verify it remains untouched, and write only to an isolated staging state root.

## License

Apache License 2.0. See [LICENSE](LICENSE).
