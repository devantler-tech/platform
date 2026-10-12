# Node-local backup agent scheduling

Production Velero node-agents use the existing `hcloud-volume-workload` priority
tier. They can make room above movable default-priority workloads, but cannot
preempt equal-priority database instances or higher-priority platform and storage
components. The single Velero server and its scheduling remain unchanged.

This does not reduce memory requests, reserve transfer capacity, or make a backup
complete. Source-pinned transfer admission remains tracked in
[#4723](https://github.com/devantler-tech/platform/issues/4723), and a successful
new full backup is still required for
[#4729](https://github.com/devantler-tech/platform/issues/4729).

After deployment, verify every desired node-agent is Ready, database redundancy
and archiving remain healthy, and the next full backup covers all included
volumes. Preserve failed backup records; do not patch statuses or lower requests
to obtain a clean result.
