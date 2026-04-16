# Squad Decisions

## Active Decisions

### 2026-04-06 - Adopt Initial Milestone Sequence

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/ripley-milestones-initial.md
- Decision: Use a three-milestone delivery plan for implementation sequencing.
- Scope:
	1. Lease lifecycle hardening for access safety.
	2. Proxy authentication and request gating.
	3. Audit evidence integrity and acceptance coverage.

### 2026-04-06 - Accept Milestone 1 Ticket Map

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/ripley-m1-ticket-map.md
- Decision: Adopt Ripley's six-ticket Milestone 1 planning map as the implementation baseline.
- Ticket map:
	1. Status conditions and observed generation.
	2. Lease validation and policy limits.
	3. RBAC drift self-healing idempotency.
	4. Cleanup hardening for revoke/delete/expire.
	5. Events, logs, and reconcile outcome metrics.
	6. Lifecycle hardening documentation and runbook.

### 2026-04-06 - Implement M1-01 Status Signals for TetherLease

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/bishop-m1-01-status-signals.md
- Decision: Implement lifecycle status signaling using `status.observedGeneration` and a `Ready` condition while preserving phase semantics.
- Scope:
	1. Add `status.observedGeneration`.
	2. Add `status.conditions` with `Ready` condition signaling.
	3. Set `Ready=True` on successful activation and `Ready=False` on practical failure paths.
	4. Reduce status-update noise by skipping status writes when no semantic status change occurred.

### 2026-04-06 - Implement M1-05 Lifecycle Observability

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/bishop-m1-05-observability.md
- Decision: Add lightweight operator observability with events, transition logs, and a reconcile outcome counter.
- Scope:
	1. Emit events for `Activated`, `Expired`, `Revoked`, `InvalidDuration`, and `ActivationFailed`.
	2. Log lease transition context (`name`, `namespace`, phase transition, reason).
	3. Emit `tether_operator_reconcile_outcomes_total{outcome,reason}` counter metric.
	4. Validate event-path behavior with operator unit tests.

### 2026-04-06 - Implement M1-06 Lifecycle Docs Alignment

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/ripley-m1-06-docs.md
- Decision: Align lifecycle hardening documentation and CRD descriptions with implemented behavior, without runtime changes.
- Scope:
	1. Document `status.phase`, `status.conditions`, and `observedGeneration` troubleshooting signals in README.
	2. Document expected `Ready` condition reasons/messages for operator triage.
	3. Clarify CRD schema descriptions for duration validation and condition semantics.
	4. Keep runtime logic and API shape unchanged.

### 2026-04-06 - Implement M1-02 Lease Admission Guardrails

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/bishop-m1-02-guardrails.md
- Decision: Enforce pre-RBAC lease guardrails for role, user, and duration policy so rejected leases never create bindings.
- Scope:
	1. Validate role safety policy, including allowed built-ins and bounded custom role names.
	2. Reject unsafe or reserved principals and enforce bounded user identity format.
	3. Enforce duration bounds with explicit reasons for too short/too long inputs.
	4. Keep rejected leases non-Active and prevent ClusterRoleBinding creation.

### 2026-04-06 - Reviewer Rejection for M1-02 Semantics

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/hudson-m1-02-review.md
- Decision: Reject initial M1-02 implementation pending reconcile-semantics fix.
- Required revisions:
	1. Treat terminal validation rejections as non-error reconcile outcomes after status/event signaling.
	2. Preserve Warning events and Ready=False reason/message diagnostics.
	3. Add explicit DurationTooShort unit test coverage.

### 2026-04-06 - Revise M1-02 After Reviewer Feedback

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/vasquez-m1-02-revision.md
- Decision: Apply the review-mandated M1-02 revision so terminal validation rejections return non-error reconcile results while retaining full rejection diagnostics.
- Scope:
	1. Return `ctrl.Result{}, nil` for InvalidRole, InvalidUser, InvalidDuration, DurationTooShort, and DurationTooLong terminal rejection paths.
	2. Keep Ready=False with precise reason/message and Warning event signaling.
	3. Add DurationTooShort test assertions for non-Active phase and no binding creation.

### 2026-04-06 - Approve Revised M1-02 Implementation

- Requested by: Jaydee94
- Source: .squad/decisions/inbox/hudson-m1-02-rereview.md
- Decision: Approve revised M1-02 implementation after verifying non-error terminal rejection semantics and explicit DurationTooShort coverage.
- Validation:
	1. `go test ./pkg/operator` pass.
	2. Reviewer confirms no remaining findings.

## Governance

- All meaningful changes require team consensus
- Document architectural decisions here
- Keep history focused on work, decisions focused on direction
