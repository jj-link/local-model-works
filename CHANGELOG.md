# Changelog

## Unreleased

- Feature: replace deployment-plan internals with a compact recipe launcher, operator-owned digest-pinned launch profiles, validated model/runtime settings, and explicit installed, origin-download, or peer-copy preparation states.
- Feature: complete the secure Go control plane and mTLS node agent, including ownership-scoped container lifecycle, artifact reconciliation and transfer, placement leases, and offline recovery.
- Feature: add immutable OCI recipe packaging, full-commit Git import, authoring CLI, and the inspectable Library recipe builder.
- Feature: add compile-time first-party backend and frontend modules, generated OpenAPI handlers, the Workshop topology surface, and behavioral browser coverage.
- Feature: integrate the warm editorial Sample A production interface, grouped functional navigation, recipe hardware catalog, workload-aware Fleet and Serving views, and authenticated deployment-backed Chat.
- Feature: aggregate Git-backed recipes by repository, deterministically compile pinned updates, preview affected hardware, and perform durable same-placement deployment replacement with progress and rollback.
- Feature: add URL-only Git import with immutable default-branch resolution and a deterministic `MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks` compiler, including pinned model/drafter artifacts, two-role Spark planning, per-member RoCE/RDMA/GID bindings, artifact-size previews, and guided launch UI.
- Feature: add persistent per-rank preparation progress, resumable bounded-concurrency Hugging Face snapshot downloads, verified snapshot completion manifests, preparation cancellation, endpoint-aware Chat/Verify actions, and role-specific deployment logs.
- Feature: add a deterministic compiler and guided launch flow for `tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark`, with pinned stable censored, legacy censored, and uncensored model choices; DFlash2; bounded host preparation; worker-first TP2 boot; and `/health` verification.
- Fix: retry interrupted HTTP artifact streams, recover complete partial files, project peer-copy progress durably, prefer resumable origin downloads when partial data exists, exclude host-preparation helpers from workload state, and remove stale untrusted transfer staging on agent startup.
- Fix: calibrate the NVFP4 runtime budget to keep sufficient KV capacity while preserving Spark OS, agent, SSH, and tailnet headroom; keep profiler-sized KV allocation; make launch-plan digests insensitive to harmless live telemetry changes; and prevent late pre-stop state reports from regressing a confirmed stop.
- Fix: make serving termination and stop convergence durable, retain structured crash causes and logs, safely release leases, support transactional restart, and separate active from stopped deployments in the operator console.
- Fix: compile Qwen repository updates when upstream omits its optional SGLang license copy, while retaining the controller-owned license asset and strict patch allow-list validation; normalize empty update API arrays so no-target previews render safely.
- Fix: make recipe update previews non-mutating, derive installed devices from valid package placements rather than deployment state, stage candidate packages on every installed device, and select the new repository version only after package delivery and running-deployment replacement succeed.
- Fix: remove recipe trust state and approval gates; validated recipes now plan and launch directly while import and update flows retain informational immutable-contract, permission, and hardware previews.
- Fix: remove recipe display names from manifests, storage, APIs, and launch flows; repository-backed recipes now use their GitHub `owner/repository` identity throughout the catalog.
- Fix: require complete validated model snapshots before placement, preserve partial cache data across stop/restart, avoid stale progress phase regressions, persist runtime container IDs, gate healthy state on the configured readiness probe, and retain historical logs after a rank crash or run replacement.
- Fix: resolve wildcard peer-transfer listeners through each source node's fabric address and normalize controller-managed transfer permissions before capability-dropped containers mount them.
- Feature: add compiler-backed Python, JavaScript, Go, Rust, Java, and C++ benchmark graders using multi-architecture digest-pinned toolchain images.
- Feature: add bounded five-second and one-minute telemetry retention, Prometheus exposition, and node history APIs.
- Feature: add digest-gated, resumable DGX Dashboard migration scan/import jobs that write to isolated staging state.
- Security: replace environment bootstrap credentials with offline operator creation, secure origin-bound sessions, nonce CSP, managed-container ownership, and root-owned systemd packaging.
- Security: add root-state-gated, origin-bound, one-use browser login tokens for repeatable operator-console automation without retaining the operator password.

