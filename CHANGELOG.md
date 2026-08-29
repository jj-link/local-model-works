# Changelog

## Unreleased

- Feature: complete the secure Go control plane and mTLS node agent, including ownership-scoped container lifecycle, artifact reconciliation and transfer, placement leases, and offline recovery.
- Feature: add immutable OCI recipe packaging, full-commit Git import, authoring CLI, and the inspectable Library recipe builder.
- Feature: add compile-time first-party backend and frontend modules, generated OpenAPI handlers, the Workshop topology surface, and behavioral browser coverage.
- Feature: integrate the warm editorial Sample A production interface, grouped functional navigation, recipe hardware catalog, workload-aware Fleet and Serving views, and authenticated deployment-backed Chat.
- Feature: aggregate Git-backed recipes by repository, deterministically compile pinned updates, preview affected hardware, and perform durable same-placement deployment replacement with progress and rollback.
- Fix: make serving termination and stop convergence durable, retain structured crash causes and logs, safely release leases, support transactional restart, and separate active from stopped deployments in the operator console.
- Fix: compile Qwen repository updates when upstream omits its optional SGLang license copy, while retaining the controller-owned license asset and strict patch allow-list validation; normalize empty update API arrays so no-target previews render safely.
- Fix: make recipe update previews non-mutating, derive installed devices from valid package placements rather than deployment state, stage candidate packages on every installed device, and select the new repository version only after package delivery and running-deployment replacement succeed.
- Feature: add compiler-backed Python, JavaScript, Go, Rust, Java, and C++ benchmark graders using multi-architecture digest-pinned toolchain images.
- Feature: add bounded five-second and one-minute telemetry retention, Prometheus exposition, and node history APIs.
- Feature: add digest-gated, resumable DGX Dashboard migration scan/import jobs that write to isolated staging state.
- Security: replace environment bootstrap credentials with offline operator creation, secure origin-bound sessions, nonce CSP, managed-container ownership, and root-owned systemd packaging.
- Security: add root-state-gated, origin-bound, one-use browser login tokens for repeatable operator-console automation without retaining the operator password.
