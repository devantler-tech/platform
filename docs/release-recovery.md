# Recover a missing GitHub release

Use **Publish Release Recovery** in this repository’s Actions tab when a stable version tag exists but its GitHub release is missing. The operation preserves the tag and source and leaves the latest-release selection unchanged.

1. Select the **main** branch and copy the incident record’s tag, full source SHA, original failed run ID, run attempt and immutable producer reference into the matching inputs.
2. First enable **enable-recovery** and **offline-test**, leaving **confirm-publication** false. This builds the pinned recovery tool and checks that it refuses publication without a credential.
3. After the reviewed workflow and its required checks pass, run the same record on main with **enable-recovery** and **confirm-publication** true and **offline-test** false.
4. Independently check the published release’s tag and source, the unchanged original tag, and the latest-release selection. Repeat the same recovery request: a matching publication must be a verified no-op, with no publication step. Record the outcomes on the original incident.

An unsupported, conflicting or incomplete observation stops recovery. The normal release queue serializes this operation with ordinary releases.

The recovery engine is the unchanged command from [.github v13.4.0](https://github.com/devantler-tech/.github/releases/tag/v13.4.0), pinned to `f558e3ababb474ac0e6f27d988ce8c876a525196`. Updating it requires review of the new immutable source, the boundary tests and the regenerated offline fixture. Run `bash scripts/generate-release-recovery-fixture.sh` after changing the production workflow; do not edit the generated fixture.

The temporary **enable-recovery** rollout flag remains tracked by [.github#603](https://github.com/devantler-tech/.github/issues/603). Explicit **confirm-publication** remains an operator gate after rollout.
