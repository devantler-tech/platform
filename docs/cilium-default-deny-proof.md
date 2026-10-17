# Cilium default-deny traffic proof

Issue [#3501](https://github.com/devantler-tech/platform/issues/3501) requires a
throwaway namespace proof before rolling the generated floor through production.
`Prove Cilium Default Deny` is dormant, dispatch-only plumbing for that gate. Its
presence or passing offline tests does not authorize a dispatch. Maintenance agents
prepare and statically validate it; the maintainer explicitly approves a concrete
run before the workflow starts its disposable cluster.

The workflow runs only from `main`, using an approved exact main SHA. It takes a
candidate PR number and exact head, verifies an open PR by a trusted author in this
repository, and downloads only the fixed generator and oauth2-proxy policy files.
It rechecks the PR head after the reads. It never checks out or runs candidate code.
The short-lived GitHub token has only contents and pull-request read access.

The proof uses checksum-verified KSail 7.193.8 and the Cilium 1.20.2 image/digest
that version embeds, matching the audited platform chart. A fresh hosted runner
creates one Vanilla/Docker node with a private kubeconfig and explicit context.
GitOps, policy engine, registry mirrors, storage, certificate management, metrics
and load balancing are disabled. It loads no production environment or credentials
and has no cloud provider. Four small, pinned toolbox pods run in `deny-proof`.

The main proof runner generates the candidate CNP and DNS policy with the pinned
Kyverno CLI and compares the complete default-deny `spec`/`specs` rule body with
the candidate's Flux copy.
Cilium documents an empty ingress/egress rule as
[default deny without an allowed peer](https://docs.cilium.io/en/stable/security/policy/layer3/#ingress-egress-default-deny);
the wildcard peer selector nested under `fromEndpoints`/`toEndpoints` is a different
allow-all shape. Runtime observations still remain required for this candidate.
The focused test applies those two generated CNPs, plus one narrow HTTP allow CNP.
Each HTTP helper rule sets both `enableDefaultDeny` directions to `false`.
[Cilium documents this opt-out](https://docs.cilium.io/en/stable/security/policy/intro/)
because selecting allow rules otherwise activate directional default deny and
could make a broken candidate floor appear to block traffic. The runner first
proves all HTTP pairs remain reachable with only the helper installed. It then
applies only the generated floor and proves both forbidden directions fail while
admitted HTTP and the denied target remain healthy. The generated DNS policy is
added after that causal phase, since its egress rules could independently enforce
default deny. DNS is intentionally excluded from the floor-only phase, which uses
fixed pod IPs, and must work again in the final phase.
It deliberately omits the standard NetworkPolicy mirror so another default-deny
source cannot stand in for the CNP under test. The existing static tests continue
to cover the companion mirror and all production policy copies.

The runner requires these observations:

- DNS and each HTTP pair work before and after installing the non-enforcing
  HTTP helper, before applying the candidate floor.
- All four probe pods stay ready with the same UID, IP and running container
  identity throughout the proof; a restart or failed read invalidates the run.
- Exactly the expected CNPs report `Valid=True` in each phase: one helper, then
  helper plus floor, then all three. Their complete `spec`/`specs` bodies match
  the candidate/main bodies that were applied. Adding a policy preserves each
  previously proven policy's UID, generation and complete body.
- DNS and admitted HTTP keep working; separate denied pairs demonstrate ingress
  and egress blocking. Each denied pair explicitly allows the opposite direction.
- Three consecutive samples report in-pod HTTP timeouts for both forbidden pairs
  alongside admitted HTTP in the floor-only phase. The final phase repeats these
  observations with successful DNS. An exec API error, failed baseline or
  connection refusal cannot count as a policy denial.
- The denied server answers HTTP on loopback before and after every traffic
  sample, so a dead listener cannot masquerade as blocked egress.
- A repeated CNP read preserves UID, generation, spec and validity. Cilium's
  `Valid` condition omits `observedGeneration`, so it is not used as a fence.
- Cleanup removes only the named run-owned Docker cluster and verifies its absence.
  There is no Docker prune, shared-volume deletion or ambient kubeconfig operation.

Inputs are the literal confirmation `RUN_DISPOSABLE_CILIUM_PROOF`, the approved
workflow main SHA, candidate PR, and candidate head SHA. An approved operator may
dispatch the workflow on `main` with those four exact values. A moved workflow or
candidate head requires a fresh run bound to that revision.

The Actions artifact contains only `receipt/result.json`: candidate file hashes,
workflow/candidate heads, tool versions, policy UID/generation/spec hashes, test
results, probe UIDs and hashed IP/container identities, and cleanup verdict. It
never contains kubeconfig, credentials or pod IPs.
Read the artifact from a successful exact-head run; a receipt is green only when
both `verdict` and `cleanup` are `PASS`, `floor_valid_cnp_count` is two,
`valid_cnp_count` is three, and all five `floor_tests` and all five final `tests`
results are true. The floor results include the helper-only HTTP positive control.

This proves basic CNP behavior on one Docker node. It does not prove production
rollout, all namespace health, cross-node/encrypted traffic or platform-template
acceptance. Those remain separate issue gates. No production rollout is triggered
by this workflow.

Offline maintenance validation is:

```bash
shellcheck .github/scripts/fetch-cilium-deny-candidate.sh scripts/prove-cilium-default-deny.sh scripts/tests/test-fetch-cilium-deny-candidate.sh scripts/tests/test-prove-cilium-default-deny.sh
bash scripts/tests/test-fetch-cilium-deny-candidate.sh
bash scripts/tests/test-prove-cilium-default-deny.sh
actionlint .github/workflows/prove-cilium-default-deny.yaml
```

These regressions replace only external process boundaries with recording tools.
They independently model enforcement by the HTTP helper, candidate floor and DNS
companion. Unselected or non-enforcing floors, and a floor with no datapath effect,
must fail even when the other policies could impose a deny. They start no cluster
and cannot replace the actual dispatched traffic proof.
