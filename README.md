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

Source-bound host `HF_HOME`/`HF_CACHE` settings, including the compiler's `hf_home` override for checkout-relative launchers, default to the selected execution device's first configured writable Hugging Face home. A `hub` directory alone is not a Hugging Face home. Missing usable roots block launch rather than silently creating a private model cache. Explicit settings, saved profiles and literal source defaults still take precedence. Portable profiles can omit these paths until devices are selected. Coordinated recipes use the head's host-cache setting and separate worker bindings; independent execution devices with different roots require an explicit common path. The resolved path is included in launch approval, so changing it requires a new plan. Existing deployments keep their frozen cache settings and mounts, including on restart or rollback; new defaults do not silently migrate them.

Consolidating existing caches is a separate storage operation: preserve model identities and revisions, validate complete blob hashes, and merge snapshot links into the canonical root. Byte-identical immutable files on the same filesystem can share hard links while old paths remain usable. Do not discard partial downloads or overwrite conflicting refs, and do not redirect a running container's bind mount. Shared cache selection avoids isolated copies; it does not prevent an upstream script from downloading a new or missing model revision.

Missing required `WORKER_SSH`/`WORKER_HOST` settings prefer an existing SSH alias in the selected head agent account whose effective user and destination match the selected worker. Before reporting Ready, source-owned worker SSH inputs are checked from that account with a bounded, noninteractive remote `true` command. Explicit targets and source-bound fabric IPs are checked exactly as rendered, not silently replaced. The check requires an updated, host-execution-enabled head agent, existing credentials and trusted host entries; it does not change SSH keys, configuration or trust. Executable `Match exec` configuration and proxy transports are unsupported and block preflight. **Retry readiness check** repeats the check without launching anything. Restarting a saved deployment rechecks its frozen target; new defaults do not migrate old deployments.

App-managed files are reused, with missing downloads authorized only by **Run**. For source-owned recipes, that same click authorizes the reviewed source's host execution; its scripts control their own file reuse, downloads and builds.

The eight maintained upstream procedures expose reviewed environment inputs, existing vLLM/SGLang/TensorFold options, Docker runtime choices and Compose settings through searchable setting groups. Optional overrides preserve upstream defaults until enabled. Runtime bindings validate the original source SHA-256 and exact spans and encode selected values for their shell, remote-shell, heredoc or Compose context. Device, GPU and fabric bindings remain controlled by the reviewed placement.

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

For individual immutable downloads, `source.type: file` accepts an HTTPS `source.identity` and requires `source.digest: sha256:<hex>`. Malformed URLs, control characters, embedded credentials, fragments and insecure HTTP origins are rejected during recipe validation. Separate file artifacts can select exact GGUF shards and sidecars without claiming that a partial Hugging Face checkout is a complete managed snapshot.

On **Devices**, review **Download to device** separately from running. Its frozen plan lists all declared model/data files, platform-specific images, helper packages, exact sources, credentials, destinations, storage, and staging requirements. Verified reuse and supported partial-data recovery precede eligible peer or origin acquisition; unknown identity, integrity, or storage requirements block submission. Download eligibility does not reserve GPUs or require an idle serving device. Completion requires every declared resource to be verified, not merely a successful transfer or an existing directory.

**Run on device** defaults to verified existing files only. Missing or changed resources block every launch gate without implicitly fetching them. **Download and run** is a separate explicit choice with a fresh resource review. Runtime settings, ranks, permissions, host preparation, and hardware suitability still require their own launch plan.

Each node reports durable per-file and aggregate acquisition progress. Interrupted downloads retain partial and verified data; reconnect and controller or agent restart do not silently replay them. **Resume** requires a fresh review of the original immutable content, runtime, devices, and destinations. Cancellation retains writer ownership until the agent confirms quiescence. Hugging Face snapshots become reusable only after every pinned file is validated and a completion manifest is committed; resumed peer copies revalidate retained bytes before appending.

On Linux agents, successful SHA-256 calculations are reused while the file identity, size, permissions, modification time, and change time remain unchanged. Launch checks and peer manifests therefore do not reread unchanged model weights at every stage. Downloads and repairs still require matching checksums; a changed file is hashed again, and a missing or mismatched file blocks launch. This bounded cache is process-local: agent restart or cache eviction requires fresh verification. Platforms without supported change-time metadata continue full verification.

Native managed workloads run without container-imposed CPU quotas, CPU affinity restrictions, or RAM ceilings. Recipes do not expose these controls. Upstream scripts own their own runtime options; device inventory and hardware checks do not constrain their host execution.

#### Qwen3.8 Flash Next with llama.cpp on RTX PRO 6000

[`qwen38-flash-next-llamacpp-rtx6000pro`](recipes/qwen38-flash-next-llamacpp-rtx6000pro/recipe.yaml) is a native managed-container bundle for one 96 GB-class SM120 GPU, not a Spark upstream-script adaptation. It uses the full, non-pruned Unsloth `UD-Q4_K_XL` checkpoint, shared `Q8_0` MTP head and `mmproj-F16.gguf` at HF revision `766911a6b7369840a91dbcd95f9f997acaab6cd6`. The six selected weight files total approximately `107.1 GiB`; other quantizations are not downloaded. Each file has its own SHA-256 and read-only mount.

The runtime is Unsloth's `b11539-mix-2500203` Linux x64 CUDA 13 portable archive, SHA-256 `888d8b324b0015d77372dbb8c2f2405d1b7f9d55eb3f8b4d3c40ff299719027e`. Release provenance is pinned to `3a1e7cc1d81709ba368a4ca41e138fadabceeb13`; the binary reports mixed-build commit `1cdd47c403d51390d85872bf3e1e50c5ca3ccae9`, which is not independently retrievable as a source tree. The CUDA 13.0.2 Ubuntu 24.04 image uses its exact x64 manifest `sha256:121b25126d81cddeae4c048aa46e94050514ca6b8609a124027b38073e7ef4b6`. A separately pinned Ubuntu OpenMP package supplies the missing runtime library. The launcher extracts these verified files under `/opt` in the writable container layer and directly executes `llama-server`; it does not install host packages, download at startup or allocate a RAM-backed installation directory.

Defaults are one `262144`-token slot, five-token MTP drafting, vision, GPU-resident main layers and lazy, file-backed n-gram embeddings (`--lazy-mode on --load-mode mmap`). Automatic fitting is disabled, so startup cannot silently shorten the configured context. Context capacity is shared across parallel slots; increasing concurrency does not give each slot the full configured window. CPU/RAM ceilings and WSL RAM settings are not changed, and the embedding file's virtual mapping is not a requirement to pin the entire table in RAM. LMW's selected GPU UUID controls both Docker device requests and CUDA visibility, including WSL systems where Docker filtering alone exposes other adapters.

Import this directory as a native recipe package and use the normal reviewed acquisition and launch workflow. Runtime/image storage is additional to the weight-file total. The OpenAI-compatible endpoint is `http://<workstation-address>:8000/v1`, model `Qwen3.8-Flash-Next`. No inference API key is configured; restrict network access to trusted clients. Model weights use Qwen Community License 1.0; llama.cpp is MIT, and container/runtime dependencies retain their own terms.


### Reviewed upstream procedures

Eight shipped source-owned procedures cover DeepSeek V4 Flash Vision on two Sparks, Qwen3.8 27B on RTX 6000 Pro or one Spark, GLM-5.3 Flash EXL3 on two Sparks with vLLM or TensorFold, and Qwen3.8 Flash Next on one Spark with vLLM or TensorFold or two Sparks with vLLM. They record reviewed source lifecycles and supported inputs, not replacement launchers or silently substituted model/image selections. Import and package validation do not establish live Spark/Docker execution or GPU inference quality.

The single-Spark Qwen3.8 Flash Next recipe serves on port `8000`. Its launch environment, advertised endpoint, and readiness probe use the same port; OpenAI-compatible clients should use `http://<spark-host>:8000/v1`.

Version `2.3.0` pins the [vLLM procedure at `b7c5da1a9e7448a14b480deee66eda3799b1eb43`](https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Single-DGX-Spark/tree/b7c5da1a9e7448a14b480deee66eda3799b1eb43), including reduced-vocabulary MTP drafting, cached-token usage reporting and the authored MTP/cache correctness fixes. It retains the stock Mia NVFP4 checkpoint, native 262,144-token context and four-stream profile; NVIDIA weights and the vLLM 0.30 lane remain opt-in. Upstream now recommends a [separate TensorFold procedure](https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Single-DGX-Spark-TensorFold); updating this vLLM recipe does not migrate engines. Changes after `7d0712dcb83cc58b467235e24d3a4429ed14518b` affect only the upstream README, not serving code.

#### Qwen3.8 Flash Next with TensorFold on one Spark

[`qwen38-flash-next-tensorfold-spark-tp1`](recipes/qwen38-flash-next-tensorfold-spark-tp1/recipe.yaml) version `3.0.0` pins the [authored procedure at `20e15e41f2f514a0d287710f4665ee1b5009ba41`](https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Single-DGX-Spark-TensorFold/tree/20e15e41f2f514a0d287710f4665ee1b5009ba41). It executes the original `scripts/prepare.sh`, `start.sh` and `stop.sh`, observes `qwen38-flash-next-tf`, and uses literal `.env` settings. The maintained binding fixes `HOST=0.0.0.0`, port `8000` (upstream's own default is now `8888`) and model alias `Qwen3.8-Flash-Next`; clients use `http://<spark-address>:8000/v1`. Stop the existing single-Spark server before starting it.

Published defaults are TensorFold `v1.1.4`'s native Zig engine (`tensorfold-native`) with 17 upstream patches (patch hash `4baf16137ecd`), up to five streams sharing one cache pool, a `262144`-token window that is also the maximum, fp8 KV, vision/video and MTP drafting. The fp8 KV cache is lossy; `kv_dtype` `bf16` is exact. The FP8 n-gram table loads with the weights; there is no int8 cache or SSD n-gram reader. Preparation downloads about `96 GiB` of weights and pulls the matching prebuilt image by digest, with an authored local-build fallback. Shared Hugging Face and compiled-kernel caches persist outside the checkout. Disk checks require up to `114` GB for an uncached model plus `40` GB for an unprepared image on the same filesystem; startup requires roughly `107 GiB` available memory, and the engine keeps `9 GiB` free. These upstream checks are not host-isolation guarantees.

The prebuilt image is `ghcr.io/miaai-lab/qwen3.8-flash-next-single-dgx-spark-tensorfold:v1.1.4-eab8582-4baf16137ecd`, pulled as `sha256:5c6c42b9747318c1cd8b6cb85cb69493fff2e82cb23b61e49b9cf54cd09ac28e`. The default checkpoint is `nvidia/Qwen3.8-Flash-Next-FP4-Local` at revision `b9ce18a6887e18c089fdea8509a33e63429a3300`; selecting another `model_id` drops that pin unless `model_revision` is also set. `checkpoint` `int4` and the gated `ablit` overlay, which removes safety refusals, remain upstream options and are off by default. Source pinning does not freeze fallback-build dependencies. Recipe scripts/patches are MIT; the default weights use the NVIDIA Open Model Agreement over a Qwen Community License 1.0 base, and the NVIDIA container has its own terms.

#### GLM-5.3 Flash EXL3 with TensorFold on two Sparks

[`glm53-flash-exl3-tensorfold-spark-tp2`](recipes/glm53-flash-exl3-tensorfold-spark-tp2/recipe.yaml) version `2.3.0` reviews [MiaAI-Lab's TensorFold procedure at `22f55f5ff129cf5e48bf847b23af3d873705587f`](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold/tree/22f55f5ff129cf5e48bf847b23af3d873705587f). This is a separate engine/procedure, not an update of the existing vLLM GLM recipe. It executes the authored `./start.sh` and `./stop.sh` from rank 0; rank 1 is an observer of the remote container, not a second launcher. Both ranks use the upstream container name `glm53-flash-tf`. There are no LMW-generated launchers or managed model/image artifacts.

Set the required `worker_ssh` to an existing SSH alias or `user@host` for the selected worker, usable noninteractively by the head agent account. LMW fixes `TP=2`, clears `WORKER2`, binds `WORKER` to that value, `FABRIC_PEER` to the selected worker's fabric address, and `HOST=0.0.0.0`/`PORT=8888`; managed endpoint, container and fabric identities are not independent settings. Upstream's experimental three-Spark mode is not part of this binding. The API and `/health` readiness probe both use port `8888`; OpenAI-compatible clients use `http://<head-address>:8888/v1` and model `GLM-5.3-Flash-EXL3`. This is the same port as the existing cluster GLM recipe: stop the existing deployment before replacing it. The all-interface upstream bind is not an authentication boundary; restrict network access separately.

The original [configuration](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold/blob/22f55f5ff129cf5e48bf847b23af3d873705587f/scripts/config.sh) remains authoritative. LMW writes `.env` as literal `KEY=value`, not shell code, using `scripts/local.sh.example` as the reviewed input template. Upstream sources `scripts/local.sh` as Bash but reads `.env` without executing it; environment values take precedence, then `scripts/local.sh`, then `.env`, then defaults. Its `.env` reader strips matching outer quotes and unquoted `#` comments and does not expand variables or commands.

Authored defaults are TensorFold `v0.6.0` plus the repository's 75 image-baked patches, four concurrent streams (up to eight explicitly configured), the model's native `1,048,576`-token prompt-plus-reply window, DFlash2 plus copy drafts, vision/video, thinking and a `32,768`-token default reply budget. Set the generated `context` string to `"262144"` for a server-enforced 256k window; leaving it unset preserves upstream's native default. The streams share a memory-sized KV pool; simultaneous full-size windows are not guaranteed. Dense `q4` and FP8 KV are **lossy**; chunked KDA prompt arithmetic is also not bit-identical to serial BF16. Upstream reports that short French coding prompts can lose their end-of-turn with dense q4 and run to the reply limit. Draft verification does not undo those quantization differences. The normal start can retry once with a smaller fitting window, so successful startup does not guarantee the configured maximum on a memory-constrained node.

Prerequisites are two GB10/128 GB Sparks, a directly addressed ConnectX-7 private IPv4 link with a RoCE v2 GID on each side (two compatible rails are used when available), Docker/NVIDIA runtime access and `rsync` on both nodes, and head-to-worker key-based SSH. The scripts also use Bash, `curl`, Python 3, `flock`, and standard Linux network/core utilities. Defaults need about `110 GiB` free memory per node at startup and roughly `205 GB` disk per node for the target, drafter and image, plus build/JIT headroom. First setup checks `180 GB` for the head download and `35 GB` under Docker's root, summing them when co-located; worker copy checks depend on actual snapshot sizes. Low free memory is an upstream warning, not a safe-memory guarantee: unified-memory exhaustion can freeze a Spark.

`start.sh` automatically calls the authored `scripts/prepare.sh` when its preparation marker differs. Preparation may pull/tag or build the image, stream it to the worker, download pinned Hugging Face snapshots, write cache refs/state/locks and `rsync` model files outside the checkout. Worker copies are compared by file names and sizes, not a full content-hash audit. By default the head cache is `HF_CACHE`/`HF_HOME` or `~/.cache/huggingface`, the worker cache is its remote `HF_HOME` or `~/.cache/huggingface`, and compiled kernels live under `~/.cache/tensorfold-glm53/<image hash>`. The worker needs no repository checkout. Optional upstream NFS mode requires an administrator-provisioned head export and creates a read-only Docker NFS volume on the worker; it is not the default. Startup validates arguments/link, launches worker then head with host network/IPC, `/dev/infiniband`, `IPC_LOCK` and unlimited memlock, waits for `/v1/models`, then requires a chat-completion smoke reply. Failures/timeouts can leave containers running; the authored foreground mode bypasses the normal readiness wait, smaller-window retry and smoke.

The defaults in the pinned source identify these upstream-owned resources:

| Resource | Pin |
| --- | --- |
| Target `Mia-AiLab/GLM-5.3-Flash-EXL3-4bpw-TensorFold` | HF commit `078455ffe6472f9a52fbc1139f58b9db2881b25c` |
| Drafter `incoai/GLM-5.3-Flash-DFlash2` | HF commit `bf582e4eacc1810f76656d1811693ff6c6737d2a` |
| Prebuilt `ghcr.io/miaai-lab/glm-5.3-flash-exl3-2x-dgx-sparks-tensorfold` (`v0.6.0-c4cab25d2d36`) | `sha256:b47c19d66633f27cbe37da13fbc580363f466c08b9529feab1eecb1a4b904bf1` |

The matching patch/package hash selects the prebuilt digest. If pulling it fails, upstream can build from mutable `nvcr.io/nvidia/pytorch:26.07-py3`, the TensorFold `v0.6.0` Git tag and pip dependencies (`av==18.1.0`, ranged xgrammar); changed patches can select a hash-derived image tag. A source commit therefore does not make every fallback dependency immutable, and existing local image reuse follows upstream's patch-label rules. Neither stop nor replacement rolls back host changes, Docker state or shared caches.

**Native context discovery correction.** The upstream image above omits context metadata from `/v1/models`. [`model-context.Dockerfile`](recipes/glm53-flash-exl3-tensorfold-spark-tp2/model-context.Dockerfile) builds a corrected image from that exact base; [`model-context.patch`](recipes/glm53-flash-exl3-tensorfold-spark-tp2/model-context.patch) makes each native model card publish `max_model_len` from `app.effective_context_window`. This uses the admitted per-request limit, including a smaller memory-fit limit, rather than the checkpoint maximum, shared pool size, or a hardcoded client override. It preserves model aliases, omits the field for unlimited contexts, and retains upstream notices.

Build from the project root with `docker build -f recipes/glm53-flash-exl3-tensorfold-spark-tp2/model-context.Dockerfile -t local-model-works/glm53-tensorfold:model-context-22f55f5 .` using native Spark Docker, then provision the identical image and pinned snapshots on both ranks. Select the generated `image` setting and `prepare="0"` explicitly; normal upstream preparation must not replace this corrected image with its stock patch stack. Its `tf.patches` label identifies a modified image, not the unmodified upstream hash. Set `context="262144"` and check both `/health.context_length` and `/v1/models.data[].max_model_len` after restart. Keeping existing weights requires explicitly retaining `model_id` and `model_revision`; a recipe's updated default target is not an instruction to replace a running checkpoint.

`stop.sh` gives both ranks up to 30 seconds by default, interrupts rather than drains active requests, and removes the named containers. It can return success while leaving rank 1 running when SSH is unavailable or the remote stop fails. LMW's rank-1 observation independently confirms teardown; a successful coordinator command alone must not free an unconfirmed worker.

**Licenses are separate.** The [pinned recipe `LICENSE` and `NOTICE`](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold/tree/22f55f5ff129cf5e48bf847b23af3d873705587f) license its own work under Apache-2.0 and retain TensorFold/other third-party notices. The new default target's [pinned `LICENSE`](https://huggingface.co/Mia-AiLab/GLM-5.3-Flash-EXL3-4bpw-TensorFold/blob/078455ffe6472f9a52fbc1139f58b9db2881b25c/LICENSE) and model-card metadata identify MIT; the upstream recipe README calls the checkpoint Apache-2.0, so consult the exact checkpoint revision's license rather than treating the recipe's license as its model license. The previously served `Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw` at `9eaebb7c4e96d983dcd538e18624622ba5b820a8` remains ShapleyMcg License 1.0: source-available, attribution-required and subject to a named-party exclusion, **not OSI open source**. The [pinned default DFlash2 card](https://huggingface.co/incoai/GLM-5.3-Flash-DFlash2/blob/bf582e4eacc1810f76656d1811693ff6c6737d2a/README.md) is CC BY-NC-ND 4.0, non-commercial/no derivatives; commercial licensing is separate. Upstream `DRAFTER=mtp` avoids downloading/using DFlash2 but serves one stream, not four. NVIDIA's image software remains under its separate NVIDIA terms; Apache-2.0 does not cover every dependency or model.

Required attribution for the previous TR3 target, retained from upstream:

> This work includes or was produced using ShapleyMcg, created by Brandon M. Music (https://github.com/brandonmmusic-max/shapleymcg). ShapleyMcg is licensed under the ShapleyMcg License v1.0, an attribution-required license that grants no rights to the person known as "0xSero." Use of ShapleyMcg without this attribution is unlicensed.

The previous [`tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark`](https://github.com/tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark) adaptation is blocked: reviewed revision `050081dc41ce6edd4d3f15fa19dc3410ba4210e3` has no authored TP2 stop procedure and hardcodes its author's fabric addresses and port 8000. An applicable complete upstream lifecycle must be reviewed; LMW will not edit those constants or silently substitute a generated launcher.

## Serving lifecycle

Serving separates deployments that still require operator attention from fully stopped history. A deployment remains in health check until its configured readiness probe succeeds; a running container alone is not reported healthy. An unexpected container exit records the rank, container, exit code, OOM state, runtime error, and persisted rank-specific logs; the controller then stops every remaining rank and releases leases only after each workload is confirmed down. Recovery is explicit: open the stopped deployment and choose **Restart** to create a fresh run with a newly planned, transactionally persisted placement. Repeated **Retry stop** actions are safe, and offline ranks retain their leases until reconnect confirms that the hardware is free.

Deployment details show **Live statistics**, shared with the fleet-node serving monitor: generation throughput, request/slot load, KV usage, draft acceptance and selectable history. Samples refresh every five seconds; the panel shows the last-sample time and distinguishes stale, unavailable and failed monitoring. Unsupported metrics, including unreported queue depth, display `—`, not zero; absent queue gauges are also omitted from API payloads and Prometheus output. Monitoring supports vLLM, SGLang, llama.cpp and TensorFold without restarting the serving workload.

**Total generation tok/s** has a dedicated generation-only graph scale and a prominent current value. Prompt accounting and prefill values do not share that graph, so their much larger bursts cannot flatten the generation trace.

TensorFold counters and load statistics come from `/health`. Generation is aggregate completion-token throughput over the sampling interval, including tokens from live replies, not an individual request's decode speed. **Prompt accounting rate** counts prompt tokens when requests complete, not instantaneous prefill speed; **draft acceptance** is cumulative across completed requests. Initial samples, reset counters and stale intervals leave rates unavailable until a fresh baseline exists. Active slots include decoding, filling and paused lanes without double-counting the `filling`/`prefilling` aliases.

TensorFold KV reporting prefers shared-pool occupancy from `/health` when present. Otherwise, `/metrics` supplies native active-stream context/window gauges, shown as **KV context (busiest stream)**. This is the largest stream ratio, not a sum, whole-pool percentage or GPU-memory utilization. The native gauges include the full prompt during prefill and exclude idle retained prefixes. API/history/Prometheus retain this separately as `kv_cache_max_stream_usage_ratio`; minute history averages sampled maxima. Missing or invalid gauges remain unavailable; an explicit idle zero remains zero. If the current sample omits KV gauges, history retains the most recently reported metric kind rather than hiding valid historical data.

llama.cpp is identified by its native metrics, not JSON error responses from another engine's endpoint. Generation combines completed-token counters with active-slot progress; prefill counts newly processed prompt tokens, excluding cache hits. Context and slot capacity come from `/props` and `/slots`; queue depth, prefix reuse and draft acceptance use `/metrics`. A disabled `/metrics` endpoint does not prevent collection from `/props` and `/slots`. Prefix reuse and draft acceptance are cumulative ratios. Initial, reset, stale or inconsistent samples leave rates unavailable. Builds without KV occupancy or latency distributions cannot report those metrics; KV displays **not reported**, rather than a fabricated utilization value.

Agent/controller reconnects reattach workload log readers without restarting model containers. Source cursors and committed stdout/stderr lengths are checkpointed durably; readers resume captured files and Docker streams instead of replaying all retained history. Retained replaced sources drain before retirement, and terminal EOF waits for the source producer's final output. The controller ignores overlapping delivery and requests missing byte ranges instead of appending gaps. Startup cache validation runs alongside the control session so large model caches do not block live logs, heartbeats, or controller commands; snapshot verification requirements are unchanged. These fixes require an updated `lmw-agent` on each affected node, not just an updated controller.

Legacy logs without source checkpoints retain their existing bytes and may receive one retained-history replay during upgrade; subsequent restarts resume the checkpoints. Bytes removed at the source before capture cannot be recovered. If legacy controller copies already differ from agent spools, stop the controller and affected agents, archive the controller originals, and restore each rank's stdout/stderr files from its corresponding agent spool before resuming offset-aware ingestion.

Deployment logs default to **operator view**: known per-iteration vLLM/GLM scheduler chatter, speculative-decoding diagnostics, and successful health/metrics polls are hidden. Warnings, errors, request failures, periodic throughput/queue/cache summaries, startup messages, and unfamiliar output remain visible. Toggle **Raw diagnostics** to inspect unfiltered output without restarting the stream or model. Each view retains its latest 2,000 lines independently so diagnostic floods do not displace operator history. Each stream's unfinished line is capped at 65,536 UTF-16 code units without splitting Unicode characters; these browser limits do not alter complete stored logs. Rank selection, follow/pause, and word wrapping work in both views. Run-operation logs remain unfiltered.

New deployment-log connections load one atomic recent snapshot of **stdout and stderr** (up to 200 lines within 64 KiB per stream), then follow both streams in delivery order. Partial lines remain separate between streams, and fragmented UTF-8 is preserved. **Connected · waiting for new log output** distinguishes a loaded snapshot from fresh output; **Connected · last received** reports when this browser received a new raw chunk, including hidden diagnostics. Follow positions the snapshot before display and resumes immediately even when the source is idle. Reconnects preserve both byte offsets with `Last-Event-ID: <stdout>:<stderr>`; API clients can explicitly send `Last-Event-ID: 0` to read both streams from the beginning. Cursors beyond retained files receive replacement snapshots. Old nonzero single-offset deployment cursors are rejected; reload existing console pages after upgrading. Terminal EOF waits for both streams to drain, and authentication failures stop reconnect attempts.

## Migration

The CLI provides offline scan/import commands. The authenticated migration API submits resumable jobs: scans persist a digest-addressed plan; imports require the exact digest and explicit confirmation, re-scan the source, verify it remains untouched, and write only to an isolated staging state root.

## License

Apache License 2.0. See [LICENSE](LICENSE).
