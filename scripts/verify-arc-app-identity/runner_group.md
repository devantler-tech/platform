# Runner-group capability proof

The manual **Verify ARC Runner Group Capability** workflow checks whether the
existing verified App can enforce the KSail canary's repository and workflow
restrictions. It does not activate a runner or route managed analysis.

Use protected main, the first attempt and the exact confirmation
`verify-arc-runner-group-capability`. Production TLS and the existing App identity
proof must pass first. The command binds its source and workflow revisions and
uses the existing production environment and deployment lock. Its transport
preflight reads declarations only; it is not a production capability verdict.

The existing identity verifier authenticates the App and installation before an
internal callback obtains a temporary installation token. The token remains in
memory and is restricted to the intended repository and permissions. Success
requires its revocation on exit. A usable token is revoked even when its returned
metadata is invalid; an uncertain mint or unusable successful response reports
`FAIL_CLEANUP` because revocation cannot be proved. Only fixed
`ARC_RUNNER_GROUP_CAPABILITY=<verdict>` lines are emitted.

`PASS_EXISTING` means the existing `platform` group has exactly the KSail
repository, public-repository access enabled, workflow restrictions enabled,
only the protected-main canary workflow allowed, and no self-hosted or hosted
runners. The verifier makes no changes to that group.

When that group is absent, the proof creates `ksail-capability-<run-id>` with the
same restrictions and zero runners. `PASS_DISPOSABLE` additionally requires
deleting the group created by that invocation and verifying its absence through
both the direct lookup and a complete group inventory. A pre-existing disposable
group is never adopted. Missing evidence, ambiguous ownership, changed identity,
new runner membership or failed cleanup prevents success.

`PASS_DISPOSABLE` proves API capability; it does not establish the configuration
of the actual ARC pool. Neither success verdict proves managed-analysis
eligibility. Its actual native workflow identity must be separately reviewed
before any workflow allow-list or routing change.

Pull requests run synthetic HTTPS/API fixtures without production credentials.
These fixtures establish command behavior, not production transport, account
capability, runner registration or a real analysis job.

```sh
go test -race ./scripts/verify-arc-app-identity
go vet ./scripts/verify-arc-app-identity
go run ./scripts/verify-arc-app-identity --preflight-runner-group
```

An ambiguous create response cannot establish ownership for automatic deletion.
Keep capability on hold while any reserved `ksail-capability-` group remains,
including residue from an earlier invocation. Verify it through the protected
operator path before another trial; do not adopt or delete another group's
resources to clear that hold.

Once a successful create identifies a new invocation-owned group, cleanup is
armed before checking its policy fields. Invalid create-response flags keep
capability on hold. Cleanup re-reads the live identity, default/inherited flags
and zero membership before deletion; unsafe live state reports `FAIL_CLEANUP`.

After cancellation, group removal, installation-token revocation and OpenBao
revocation share one six-second budget, including cleanup already in progress.
The authenticated tunnel remains available during that budget. Ordinary cleanup
retains each stage's existing timeout; any unconfirmed cleanup reports
`FAIL_CLEANUP` rather than capability success.
