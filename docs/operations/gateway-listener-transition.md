# Separate HTTPS listeners without losing Homepage

Homepage's single Gateway parent is constrained to port 443, with no listener
section name. Its hostname selects the matching HTTPS listener. Keep that port
constraint throughout the transition: an unconstrained parent can also attach
to a plaintext listener.

The customer identity TLS work is tracked in [#5829](https://github.com/devantler-tech/platform/issues/5829).
Its listener transition has separate, observed deployment stages:

1. Deploy the port-constrained Homepage parent. Verify the applied route's
   current generation, accepted/resolved parent, gateway readiness and verified
   HTTPS response before changing any listener.
2. Add the apex HTTPS listener using the existing certificate. Keep the original
   listener unchanged. Verify both the apex and existing routes.
3. Pin Homepage to the apex listener, retaining port 443. Verify again.
4. Restrict the original HTTPS listener to the existing wildcard scope. Verify
   every existing hostname, custom-domain listener and HTTP redirect.

Rollback reverses these stages. Retain the port-only bridge until the apex
listener has been removed; restoring the original named parent earlier can
lose Homepage through the controller's more-specific-hostname isolation.

Each stage needs its own protected delivery and live readback. Separate Flux
layers do not reconcile atomically, even when their inputs share a commit.
Never infer route acceptance from the manifest alone. Keep customer identity
ingress held until its complete profile, certificates, admission and native
login acceptance are verified; listener preparation does not activate it.
