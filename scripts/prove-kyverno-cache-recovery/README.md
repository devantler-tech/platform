# Verify admission after ephemeral cache recovery

Dispatch **Prove Kyverno Cache Recovery** from current `main`, using a new first
attempt and confirmation `prove-kyverno-cache-recovery`. The workflow uses the
existing protected production credential and shared deployment lock.

The command reads fixed native Kubernetes resources and the one pull credential
the image policy uses. It qualifies the reviewed policy, complete exception
inventory, namespace and webhook scope, replica/disruption floors, ephemeral
cache, process ownership and service endpoints. Complete private objects are
compared before and after the trial; credentials and binding details are never
serialized. Reads, commands, responses and the overall trial are bounded.

The unsigned image must be readable and unsigned using the actual Kyverno pull
credential both before and after the requests. A signed ordinary-app control is
admitted, the confirmed unsigned control is denied by the named image policy,
and the signed control is admitted again. Every create request uses server-side
dry-run: no probe Pods are stored. Kyverno may emit its normal policy events.

The current signed digest was published and keyless-signed by [CD run
41172573040](https://github.com/devantler-tech/ascoachingogvaner/actions/runs/41172573040),
using the [approved immutable publisher](https://github.com/devantler-tech/.github/blob/467f07582436a26b62cf4aa756423512f85ba0c0/.github/workflows/publish-app.yaml#L415-L416).
The protected trial must establish its current availability and admission.

The receipt proves the observed service outcomes with matching before/after
bindings. It does not attribute requests to individual replicas or prove their
first cold-cache request. Kyverno hides internal attestor errors, so an unsigned
policy denial does not expose its internal cause. Generic API, registry,
authorization, quota, schema, security-policy and webhook transport errors never
count as a successful negative control. A changed source or binding discards the
whole receipt. This observation does not authorize storage activation or resets.
