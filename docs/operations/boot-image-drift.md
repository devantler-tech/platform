# A node left on another boot image by a failed deploy

Every production deploy ends by reading back the Talos boot image of every node
(`scripts/verify-talos-schematic-readback.sh`). It passes only when every node runs the Talos
version and Image Factory schematic the checkout expects, and is Ready and schedulable.

## The state this page is about

A deploy that changes the image upgrades nodes one at a time. If it fails after the first node,
that node keeps the new image while every other node keeps the old one. A deploy from `main` then
fails the readback on that node, and redeploying does not help: at an unchanged Talos version the
cluster update applies configuration without a reboot, so the node keeps its image.

The readback prints one line per node when it fails. The node with a different schematic is the one
left behind. Compare its schematic with the one the failed pull request defines:

```bash
git show <its-branch>:talos/factory-schematic.yaml | sha256sum
```

A match confirms where the image came from. No match means the cause is something else, and this
page does not apply.

## The reviewed record

`scripts/talos-boot-image-drift-accepted.tsv` may accept **one static node on one named image
until a date**, so that the repair can deploy:

| Column | Value |
|---|---|
| `node` | The static node's exact name. An autoscaled node is never accepted: it is replaced, not repaired. |
| `schematic` | The exact schematic the node reports. It cannot be the expected one. |
| `issue` | The platform issue that tracks the repair. |
| `expires` | The last day the row holds. |

With a matching row the readback reports a `::warning::` for the node and ends with
`PASS-WITH-ACCEPTED-DRIFT`, never with the plain `PASS`. Everything else still binds that node: the
Talos version, Ready, schedulable and the node counts. The row covers that node and that image only;
a second node on the same image, or the same node on any other image, fails as before.

The record is refused, before any node is read, unless it is the header and at most one row with
exactly four well-formed fields. The day after `expires` the row accepts nothing and the readback is
strict again.

## Adding and removing a row

A row narrows a deploy safety check, so it is added by a pull request the maintainer approves, and
that pull request is the first deploy that can pass. Set `expires` no further out than the repair
needs.

Remove the row in the pull request that repairs the node, or right after it. Once the node reports
the expected image the readback says so with a `::notice::` on every deploy until the row is gone.

## Limits

- The record tolerates the state; it does not repair it. The node is repaired by landing the change
  that defines its image, or by upgrading the node back.
- It accepts one node. If a failed deploy left more than one node behind, repair them instead.
