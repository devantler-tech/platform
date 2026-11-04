The owner-only World zone trial is bound to the immutable v0.167.0 artifact,
image and source revision recorded in `testdata/audit/receipt.json.golden`.
Those opaque fixtures contain public release data only. Their bytes came from
native anonymous registry reads and the pinned source commit, without running
the application or downloading executable image layers.

`Validate(root)` is offline. It binds all trial-directory source bytes, the
trial security exception, all normalized World references elsewhere in `k8s`,
and the complete dedicated Vault policy and identity stanzas. Unrelated source
edits are allowed. The runtime-disposition guard additionally checks the effective
World profile across six native Kustomize layers, including overlays that add
World resources without a literal World reference in their source file.

`VerifyNative` requires checksum-installed Cosign 3.1.4, ORAS 1.3.6 and the
byte-approved kubectl 1.36.2. It verifies both package signatures against the
exact release publisher/issuer, exercises an actual wrong-publisher refusal,
fetches and compares all immutable metadata and seven artifact files, safely
extracts regular files only, and compares the complete six-object native render.
It also rechecks both image architecture configurations and the source/tag
receipt. All native stdout, stderr and statuses stay in the private evidence
directory. Registry, source, signature and operational errors refuse proof.

Run the fresh CI boundary with absolute `COSIGN_BIN` and `ORAS_BIN`:

```sh
bash scripts/with-runtime-disposition-tools.sh --kubectl-only -- \
  bash scripts/tests/test-world-trial-profile-native.sh .
```

Set `WORLD_TRIAL_EVIDENCE_DIR` to retain evidence. No cluster call or application
execution occurs. This configuration and selected-source proof makes no claim
about arbitrary runtime data. Retire the operator grant before accounts,
authoritative persistence or production zone credentials. A new release or
changed trial profile requires a new complete audit; editing an ignore reason
or generated artifact text is not an update procedure.
