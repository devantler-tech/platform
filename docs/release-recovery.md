# Recover a missing GitHub release

Use **Publish Release Recovery** in this repository’s Actions tab when a stable version tag exists but its GitHub release is missing. The operation preserves the tag and source and leaves the latest-release selection unchanged.

1. Select the **main** branch and copy the incident record’s tag, full source SHA, original failed run ID, run attempt and immutable producer reference into the matching inputs.
2. First enable **enable-recovery** and **offline-test**, leaving **confirm-publication** false. This builds the pinned recovery tool and checks that it refuses publication without a credential.
3. After the reviewed workflow and its required checks pass, run the same record on main with **enable-recovery** and **confirm-publication** true and **offline-test** false.
4. Independently check the published release’s tag and source, the unchanged original tag, and the latest-release selection. Repeat the same recovery request: a matching publication must be a verified no-op, with no publication step. Record the outcomes on the original incident.

An unsupported, conflicting or incomplete observation stops recovery. After admitting a missing release with the read-only App token, the workflow verifies that GitHub refuses that token at its non-persisting writer-capability endpoint. Unexpected authorization, an unavailable service or an ambiguous error stops before a writer token is requested. The response is discarded without being logged. The writer must independently pass the same permission boundary before inspecting drafts or publishing. The normal release queue serializes this operation with ordinary releases.

The recovery engine is from [.github v14.0.2](https://github.com/devantler-tech/.github/releases/tag/v14.0.2), pinned to `0b137510d139bbf62e479d9610b0db17c972bc2b`. It verifies the token's effective Contents write capability rather than relying on user-role metadata. Updating it requires review of the new immutable source, the boundary tests and the regenerated offline fixture. Run `bash scripts/generate-release-recovery-fixture.sh` after changing the production workflow; it updates the marked credential-free job in `ci.yaml`. Do not edit that generated section. The separate default-off CI job runs the shipped condition and refusal controls, including `scripts/tests/test-release-writer-refusal.sh`.

The temporary **enable-recovery** rollout flag remains tracked by [.github#603](https://github.com/devantler-tech/.github/issues/603). Explicit **confirm-publication** remains an operator gate after rollout.
