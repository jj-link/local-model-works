# Changelog

## Unreleased

- Feature: center recipe work on a shared catalog with Configuration, Updates, and Devices, explicit Add from GitHub and save-only library actions, package-only repair, retained immutable versions, and dedicated AI assistance settings.
- Fix: hide the catalog's unfinished-work section when empty and label recoverable entries "Unfinished additions".
- Fix: remove model-workload CPU quotas, CPU affinity, and RAM ceilings from recipe schemas, templates, editing, launch rendering, and legacy imports; retain hardware inventory and compatibility checks.
- Fix: use decoded recipe digests for catalog repair, download, availability, and package routes so URL-encoded digests resolve correctly.
- Fix: retain the explicit repository-root path when pinning edited recipe source metadata, preserving schema validity for URL-only additions and repairs.
- Fix: compare new recipe configurations without parsing a nonexistent saved baseline, showing edited or suggested fields as additions instead of an internal JSON error.
- Fix: verify and acquire Docker images through their pinned index and requested platform while checking the exact child-manifest digest, recognizing existing multi-platform image caches without redundant pulls.
- Fix: verify classic Docker stores through explicit child-manifest references, recognize the ARM64/v8 baseline, and bound full-snapshot inspection time by declared content size rather than a fixed 45-second deadline.
- Feature: add frozen all-resource download plans, independent device acquisition, verified file/image/package availability, durable per-item progress, explicit resumable attempts, and cancellation ownership barriers across disconnects and restarts.
- Fix: default launches to verified existing resources, recheck resource identity at every launch gate, and preserve already-running models during controller reconciliation without reacquiring their files.
- Fix: retain independent editing and provider-recovery buffers, invalidate stale source consent and runtime reviews, and keep failed download and selected-replacement operations visible without automatic replay.
- Fix: recover the running head's endpoint from its frozen workload after an agent restart, restoring readiness observations without recreating containers or reacquiring resources.
- Fix: serialize agent-session responses and finish the writer before stream teardown, eliminating heartbeat and reconnect races.
- Feature: replace deployment-plan internals with a compact recipe launcher, operator-owned digest-pinned launch profiles, validated model/runtime settings, and explicit installed, origin-download, or peer-copy preparation states.
- Feature: require explicit per-rank target selection in the recipe launcher — planning holds until every rank has a node, plan requests always carry explicit placements, and changing a selection drops the stale plan before re-planning.
- Feature: complete the secure Go control plane and mTLS node agent, including ownership-scoped container lifecycle, artifact reconciliation and transfer, placement leases, and offline recovery.
- Feature: add immutable OCI recipe packaging, full-commit Git import, authoring CLI, and the inspectable catalog configuration editor.
- Feature: add persisted repair/update baselines, immutable source evidence, bounded assistant context, separately reviewed suggested changes, verified external references, exact package review, warning-bound library installation, and explicit launch planning.
- Feature: add isolated local, OpenAI-compatible, and native Codex assistant providers with encrypted credential references, device authorization, model discovery, consent-bound generation, cancellation, and durable operation recovery.
- Fix: preserve Git subpath compilation, atomically create and update module settings, normalize empty resolver inputs, validate exact install job inputs, and load editable recipe documents through the shared module host.
- Feature: add compile-time first-party backend and frontend modules, generated OpenAPI handlers, the Workshop topology surface, and behavioral browser coverage.
- Feature: integrate the warm editorial Sample A production interface, grouped functional navigation, recipe hardware catalog, workload-aware Fleet and Serving views, and authenticated deployment-backed Chat.
- Feature: aggregate Git-backed recipes by repository, deterministically compile pinned updates, and independently replace explicitly selected running deployments on their existing ranks, with durable progress and attempted restoration rather than catalog rollback.
- Feature: add URL-only Git import with immutable default-branch resolution and a deterministic `MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks` compiler, including pinned model/drafter artifacts, two-role Spark planning, per-member RoCE/RDMA/GID bindings, artifact-size previews, and guided launch UI.
- Feature: add persistent per-rank preparation progress, resumable bounded-concurrency Hugging Face snapshot downloads, verified snapshot completion manifests, preparation cancellation, endpoint-aware Chat/Verify actions, and role-specific deployment logs.
- Feature: add a deterministic compiler and guided launch flow for `tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark`, with pinned stable censored, legacy censored, and uncensored model choices; DFlash2; bounded host preparation; worker-first TP2 boot; and `/health` verification.
- Fix: retry interrupted HTTP artifact streams, recover complete partial files, project peer-copy progress durably, prefer resumable origin downloads when partial data exists, exclude host-preparation helpers from workload state, and remove stale untrusted transfer staging on agent startup.
- Fix: calibrate the NVFP4 runtime budget to keep sufficient KV capacity while preserving Spark OS, agent, SSH, and tailnet headroom; keep profiler-sized KV allocation; make launch-plan digests insensitive to harmless live telemetry changes; and prevent late pre-stop state reports from regressing a confirmed stop.
- Fix: make serving termination and stop convergence durable, retain structured crash causes and logs, safely release leases, support transactional restart, and separate active from stopped deployments in the operator console.
- Fix: compile Qwen repository updates when upstream omits its optional SGLang license copy, while retaining the controller-owned license asset and strict patch allow-list validation; normalize empty update API arrays so no-target previews render safely.
- Fix: make source-update previews non-mutating and catalog saves independent of device availability, package delivery, and running-deployment replacement; retain separate reviewed versions from the same source commit.
- Fix: refresh recipe, device, and operation state after completion, show pinned source identity, retain version history, and keep interrupted work discoverable.
- Fix: replace recipe trust state with immutable validation, warning acknowledgements, and explicit package, acquisition, and launch reviews.
- Fix: remove recipe display names from manifests, storage, APIs, and launch flows; repository-backed recipes now use their GitHub `owner/repository` identity throughout the catalog.
- Fix: require complete validated model snapshots before placement, preserve partial cache data across stop/restart, avoid stale progress phase regressions, persist runtime container IDs, gate healthy state on the configured readiness probe, and retain historical logs after a rank crash or run replacement.
- Fix: resolve wildcard peer-transfer listeners through each source node's fabric address and normalize controller-managed transfer permissions before capability-dropped containers mount them.
- Feature: add compiler-backed Python, JavaScript, Go, Rust, Java, and C++ benchmark graders using multi-architecture digest-pinned toolchain images.
- Feature: add bounded five-second and one-minute telemetry retention, Prometheus exposition, and node history APIs.
- Feature: add digest-gated, resumable DGX Dashboard migration scan/import jobs that write to isolated staging state.
- Security: replace environment bootstrap credentials with offline operator creation, secure origin-bound sessions, nonce CSP, managed-container ownership, and root-owned systemd packaging.
- Security: add root-state-gated, origin-bound, one-use browser login tokens for repeatable operator-console automation without retaining the operator password.

