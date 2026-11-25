# Bulwark webmail

[Bulwark](https://github.com/bulwarkmail/webmail) (AGPL-3.0-only) is a webmail for JMAP servers.
The platform runs it so that the maintainer can read the company mailbox from devices he does not
own ([#6662](https://github.com/devantler-tech/platform/issues/6662)). It is a Next.js server; the
browser talks JMAP straight to Stalwart, and the server handles the OAuth sign-in and checks
session limits.

It is a third-party app, so it runs in production only (ADR 0016), in the `bulwark` namespace
(`k8s/providers/hetzner/apps/bulwark/`). It is launched dark: it runs and must report ready, but
nothing routes to it until the swap step of #6662.

## Decisions

The maintainer's decisions of 2026-11-25:

- Bulwark serves `mail.${domain}`; Stalwart's web side moves to `stalwart.${domain}`.
- Bulwark sits behind GitHub SSO, and the mailbox sign-in is only Stalwart's own OAuth login: the
  mail password and two-factor code are typed on Stalwart's page, never Bulwark's.
- Sessions end after 15 minutes without activity and 12 hours after sign-in. "Remember me" is
  offered and off unless ticked.
- Uploads are allowed for the maintainer's account only. That is a Stalwart setting, changed in
  #6662's restart step.

## What is deployed

`deployment.yaml` explains each setting. In short:

- Sign-in is OAuth only (`OAUTH_ONLY`) against `JMAP_SERVER_URL=https://stalwart.${domain}`, as
  the public client `bulwark`.
- `NO_THIRD_PARTY_TRAFFIC` turns off every outside host Bulwark knows about: telemetry, update
  check, push relay, extension directory, sender favicons and the rest.
- `STALWART_FEATURES=false` hides the Stalwart account screens. `STALWART_ADMIN_ACCESS=off` with
  no `ADMIN_PASSWORD` leaves Bulwark's admin dashboard disabled: a Stalwart administrator gets no
  Bulwark admin rights, and there is no admin password to guess.
- The admin config is a read-only ConfigMap (`config-map.yaml`). Its `policy.json` holds the
  session limits in `organisation.session`: `idleMinutes: 15`, `maxHours: 12` and the default
  `warnSeconds: 60` ([Bulwark's sign-out times](https://bulwarkmail.org/docs/guides/organisation/sessions)).
- The session secret is generated once by an External Secrets `Password` generator and read from
  a file. A new secret, after a rebuild, signs everyone out once.
- One replica. Without a shared store (Valkey), sessions, sign-in limits and admin state are per
  pod, and they live on scratch volumes.
- The pod runs under restricted pod security with a read-only root filesystem, as UID 65532.
- The network policy admits no ingress. Egress is `stalwart.${domain}` on 443 and the DNS lookups
  for it. Readiness (`/api/health/ready`) proves that path: it needs an answer from Stalwart's
  session resource. Liveness (`/api/health/live`) does not, so a mail outage never restarts Bulwark.

`scripts/tests/bulwark/` pins these settings. Run `go test ./scripts/tests/bulwark/`.

The `apps` layer waits for Bulwark to be ready, so `stalwart.${domain}` must resolve before this
deploys: #6662's first step adds that hostname to Stalwart's route.

## Check it

Read-only checks against production:

```bash
kubectl --context oidc@prod -n bulwark get deployment bulwark
kubectl --context oidc@prod -n bulwark logs deployment/bulwark --tail 50
```

`READY 1/1` means Bulwark started, read its config and reached Stalwart.

## Update the image

Bulwark is pinned by tag and digest in `deployment.yaml`, and the test pins the same reference.

1. Read the release notes from the current version up to the new one, for changes to settings,
   `policy.json` or sign-in.
2. Resolve the new tag's digest, the multi-platform index:

   ```bash
   crane digest ghcr.io/bulwarkmail/webmail:<version>
   # or: docker buildx imagetools inspect ghcr.io/bulwarkmail/webmail:<version>
   ```

3. Check the settings against the new version's source: `lib/admin/types.ts` (environment
   variables and defaults), `lib/admin/organisation-policy.ts` (the session schema),
   `lib/admin/paths.ts` (config and state directories) and `Dockerfile` (port, user, writable
   directories).
4. Update the image in `deployment.yaml` and in `scripts/tests/bulwark/deployment_test.go`, then
   validate the manifests and run the tests.

## Planned route

The swap step of #6662 puts Bulwark at `mail.${domain}` behind GitHub SSO, like the platform's
other SSO-gated apps (for example `budget.${domain}`):

- An HTTPRoute in `bulwark` for `mail.${domain}` sends traffic to the `oauth2-proxy` Service. It
  needs an entry in oauth2-proxy's ReferenceGrant and stays DNS-only.
- The auth-proxy routes the host to the `bulwark` Service and needs an egress rule to it.
- Bulwark's network policy then admits ingress on 3000 from the auth-proxy pods only, not from
  the Gateway, so the SSO step cannot be bypassed.
- `TRUSTED_PROXY_DEPTH` is checked against that proxy chain.
- The SSO cookie's `__Host-` prefix lands before this route.

## Limits

From [Bulwark's sign-out times](https://bulwarkmail.org/docs/guides/organisation/sessions):

- The browser enforces both session limits. The server also refuses to restore or renew a
  session older than 12 hours, but Stalwart's own token lifetimes are separate.
- A sign-out after a limit does not end a sign-in session at the identity provider. If Stalwart
  keeps one, the same browser can sign in again without the password while it lasts.
