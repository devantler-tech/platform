# World at Ruin zone release signer

The zone image has its own publisher: `server-cd.yaml` in `world-at-ruin`, on stable
`vMAJOR.MINOR.PATCH` release tags. Admission and Talos accept that identity only for
`ghcr.io/devantler-tech/world-at-ruin/zone`. Other application images retain their
existing shared publisher identity. The pull-credential bridge verifies access to
both the zone image and its separately signed manifest package before any cluster
write.

Deliver this support before enabling the tenant. The ordinary production deploy
publishes and waits for the new Flux revision before `cluster update` synchronizes
Talos machine config. Enabling the tenant first can therefore block its image pull
with the old host signer policy and prevent the later machine config sync.

Activation needs evidence from the delivered revision: the deploy's Talos machine
config update ran successfully, and `validate-image-verifier-liveness.sh` found the
exact zone rule and running trust material across a stable, complete node inventory.
A skipped update, an unreachable node or repository validation alone does not clear
this gate. Retain node details in private operator evidence. The workflow
`validate-image-verifier-liveness.yaml` can supply the read-only fleet check after
the signer delivery merges.

The following tenant change must enable its app-layer resource and pull-credential
namespace fanout together. Its readback must separately prove the signed manifest
revision, admitted image digest, certificate and secrets readiness, then the actual
authenticated client path over a verified TLS tunnel. Signer support alone does not
deploy a game server or establish that client path.
