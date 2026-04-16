# Project Context

- **Owner:** Jaydee94
- **Project:** PAM solution for Kubernetes called tether
- **Stack:** Go 1.24, Kubernetes controller-runtime, Cobra CLI, AWS SDK v2
- **Created:** 2026-04-06

## Learnings

- Initial focus includes operator safety and predictable reconciliation behavior.
- M1-01 implemented explicit lifecycle status signals on TetherLease with status.observedGeneration and status.conditions.
- A single Ready condition with focused reasons (Activated, InvalidDuration, ActivationFailed, Expired, Revoked) keeps diagnostics practical without over-modeling.
- Controller status writes can be made less noisy by mutating status and skipping Status().Update when no semantic change is detected.
- M1-05 added lifecycle observability with controller-runtime EventRecorder and transition logs to surface Activated/Expired/Revoked and validation/activation failures.
- A small counter metric (`tether_operator_reconcile_outcomes_total` with `outcome` and `reason` labels) is sufficient for practical reconcile visibility without additional frameworks.
- Fake recorder assertions in operator unit tests are a low-friction way to verify event emission paths.
- 2026-04-06T13:54:34Z (UTC): Completed M1-01 implementation/validation and M1-05 implementation; full `go test ./...` run finished green.
- 2026-04-06: M1-02 added pre-RBAC guardrails in activation with explicit rejection reasons for role, user, and duration policy bounds; rejected leases stay non-Active with Ready=False and do not create bindings.
- Practical policy defaults can stay code-local at this stage (allowlisted safe roles plus tether-* custom role pattern, reserved/system user rejects, and 1m to 8h duration bounds) while preserving clear operator diagnostics.
- 2026-04-06T14:04:56Z (UTC): Orchestration captured Bishop M1-02 implementation as delivered, then tracked reviewer-required revision and re-approval path.
