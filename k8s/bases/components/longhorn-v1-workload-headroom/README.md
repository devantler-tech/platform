# Longhorn migration workload headroom

Keep the two KSail operator replicas away from the storage workers with the
smallest request budget during the migration. Other workers remain eligible;
the native rollout strategy, replica count, spread constraints and resource
requests are preserved.

This component is introduced disabled. Its selection belongs in a separate
reviewed Hetzner infrastructure revision after staging is merged. Local
rendering does not select this component. Deployment acceptance
requires both replacement replicas Ready, retirement of the previous replica
set, and a fresh complete Longhorn runtime capacity census. The temporary
placement is retired through #6432 after the migration.
