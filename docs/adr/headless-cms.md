# ADR: Headless CMS for client websites

- **Status:** Proposed. It recommends Payload; choosing the CMS is the maintainer's decision. The
  offline prototype has not run yet (see [Evidence and its limits](#evidence-and-its-limits)).
- **Decision (proposed):** Offer **Payload** (MIT) as the platform's headless CMS. Run one instance
  per client site, keep its content in that instance's own CloudNativePG database and its media in
  S3-compatible storage, and give editors accounts that live in that instance.
- **Runner-up:** **Strapi** (MIT community edition).
- **Tracks:** [#4928](https://github.com/devantler-tech/platform/issues/4928), the spike under the
  epic [#4927](https://github.com/devantler-tech/platform/issues/4927).

## Context

Clients who get a website from Devantler Tech want to change its text, images and structured data
themselves. Today every change is a code change by the maintainer. The epic sets the selection
criteria; this record applies them to the epic's candidates, using public documentation and
release data read on 2026-10-17.

Two facts about this platform shape the comparison:

- **Client sites run as tenants.** Each has its own repository, publishes a signed image and signed
  manifests, and runs in its own namespace behind a default-deny network floor
  ([tenant onboarding](../TENANTS.md)). Admission verifies first-party images by signature. The
  platform already runs a third-party Node application with its own CloudNativePG database,
  barman-cloud backups to S3-compatible storage and a Cilium policy (Umami), so that shape is
  proven here.
- **Every portfolio repository changes `main` only through reviewed pull requests.** A tool that
  writes to a site's repository therefore meets the same rules as code.

## Git-based or API-based

**API-based**, for this portfolio. The reasons:

- **Editor logins.** A self-hosted Git-based editor needs each editor to hold a Git-host account
  with write access to the site's repository (Decap, Sveltia, Keystatic). Removing that need costs a
  hosted service (Decap Turbo, Keystatic Cloud, Tina Cloud), which the epic puts out of scope. The
  two exceptions, Pages CMS and self-hosted TinaCMS, fail other criteria (see the table).
- **Edits land in code repositories.** A Git-based CMS commits to the site's repository. Either each
  client edit waits for a reviewed pull request, or the CMS gets a standing bypass on the branch
  rules. The first is not "edit it yourself"; the second is the blanket exception the epic rules out.
- **What API-based costs.** One more workload, a database and a media location per client. The
  platform already runs and backs up that shape, so the cost is capacity, not new machinery.
- **What we give up.** Content as files in Git, with Git history as its record. Payload's built-in
  versions and drafts keep a per-document history in the database instead.

## What "official container image" means here

No candidate under an OSI licence publishes an official server image. Payload offers an example
Dockerfile; Strapi's documentation says it builds no official images. Read strictly, nothing
passes every filter: Directus ships an image but fails the licence, and Decap and Sveltia are static
editors that fail the editor-login criterion.

This record reads the criterion as **a reproducible image built from the upstream's official
template, pinned to a release, built for amd64 and arm64, and signed by our own CI**. That is how
every tenant image on this platform already runs and how admission already verifies them. Accepting
that reading is a maintainer decision; without it, no candidate qualifies and the epic's criteria
need to change.

## Criteria results

✅ meets · ⚠️ partly, or with our own work · ❌ fails · — not assessed after a hard-filter failure.

| Candidate | OSI licence | Popular and maintained | Simple to run and use | Cloud-native self-hosting | Fits our sites | Own logins, per-client isolation | Platform guardrails |
|---|---|---|---|---|---|---|---|
| **Payload** | ✅ MIT. SSO and some workflow features are a paid add-on we do not need | ✅ ~45k stars, ~1M npm downloads a week, weekly releases | ✅ one Next.js process plus Postgres; polished editor | ⚠️ no official image, so we build it; Postgres and S3 adapters | ✅ REST and GraphQL, framework-agnostic live preview, built-in localisation | ✅ built-in users and access control; one instance per client | ✅ our signed image; no exception expected |
| **Strapi** | ⚠️ MIT except `ee/`; live preview, content history and SSO need paid plans | ✅ ~73k stars, weekly releases | ❌ 2 GB memory minimum, 4 GB recommended | ⚠️ no official image; Postgres; S3 provider | ✅ REST and GraphQL; free localisation | ✅ built-in users and roles; one instance per client | ✅ |
| **Directus** | ❌ MSCL since v12 (June 2026), with enforced limits on the free tier | ✅ ~38k stars, monthly releases | ✅ | ✅ official amd64 and arm64 image | ✅ | ✅ | — |
| **PocketBase** | ✅ MIT | ⚠️ ~61k stars, but pre-1.0 without a compatibility promise | ❌ its dashboard is for developers, not client editors | ❌ SQLite only, one instance, no official image | ⚠️ | ⚠️ | — |
| **Pages CMS** | ✅ MIT | ❌ ~4k stars; no release since 2.1.8 (8 June 2026) | ✅ simple forms | ⚠️ Next.js plus Postgres; no official image | ✅ any framework; live after a rebuild | ✅ invited by email, sign in with a one-time code | ❌ its GitHub App needs repository Administration and Actions write; edits commit to protected branches |
| **TinaCMS** (self-hosted) | ✅ Apache-2.0 | ✅ ~14k stars, several releases a month | ❌ a Node backend, MongoDB or Vercel KV, and a GitHub token | ❌ no Postgres adapter; media cannot stay in Git | ⚠️ Astro visual editing needs server rendering; SvelteKit not documented | ✅ username and password through Auth.js | ❌ commits with a personal access token to protected branches |
| **Decap CMS** | ✅ MIT | ✅ ~19k stars, regular releases (3.17.1, 14 October 2026) | ✅ a static editor | ✅ static files plus an OAuth helper | ✅ any framework; live after a rebuild | ❌ Git accounts with push access, or Netlify Identity or paid Decap Turbo | ❌ edits commit to protected branches |
| **Sveltia CMS** | ✅ MIT | ⚠️ ~3k stars; very frequent releases, but pre-1.0 with one maintainer | ✅ a static editor | ⚠️ static; its official OAuth helper runs only on Cloudflare Workers | ✅ any framework; live after a rebuild | ❌ Git accounts with write access | ❌ edits commit to protected branches |
| **Keystatic** | ✅ MIT | ❌ ~2.5k stars; pre-1.0 and "experimental", months between releases | ⚠️ needs server routes in the site | ⚠️ | ⚠️ SvelteKit only through a community package | ❌ GitHub accounts, or the hosted Keystatic Cloud | ❌ edits commit to protected branches |

## Recommendation: Payload

Payload is the only candidate that meets every criterion under the image reading above. It is MIT
for every feature this epic needs (editor accounts, access control, drafts and versions,
localisation, S3 media). It is among the most active projects in the field and keeps its content
in Postgres, which CloudNativePG already runs and backs up here. Its content model is TypeScript
code, so it is reviewed and versioned like the rest of the platform, and its editor needs no
technical knowledge.

Its costs, accepted with this choice:

- We build, sign and keep the image current ourselves.
- A static site needs a small publish hook coded in Payload; Payload has no built-in webhooks.
- Payload 4.0 is in beta (4.0.0-beta.2, 16 October 2026), so a major upgrade follows soon.
- Figma acquired Payload in June 2025. It stays MIT and releases weekly, but its direction is
  worth watching.

## Rejected alternatives

- **Strapi**, the runner-up. The most popular candidate and a good editor, but it needs 2–4 GB of
  memory per instance, which is far from "one small workload" when every client has their own. Live
  preview and content history, which make editing safe for non-technical clients, need a paid plan.
- **Directus.** The best editing experience and an official multi-arch image, but its licence is
  source-available (MSCL since v12), not OSI-approved, and the software enforces seat, collection and
  permission limits unless a licence key is installed.
- **PocketBase.** A backend rather than a CMS: its dashboard is for developers, it stores everything
  in one SQLite file on one instance, and it has not reached 1.0.
- **Pages CMS.** The only self-hosted Git-based editor with logins that need no Git account. Its
  GitHub App needs write access to repository administration and Actions on every client repository,
  its edits would need a bypass on protected branches, and it has had no release since June 2026.
- **TinaCMS (self-hosted).** Editors need no Git account, but it needs MongoDB or Vercel KV rather
  than Postgres, commits through a personal access token, cannot keep media in Git, and does not
  document SvelteKit.
- **Decap CMS and Sveltia CMS.** The simplest to run, but every editor needs a Git-host account with
  write access to the site's repository, and edits commit to protected branches.
- **Keystatic.** Editors without a GitHub account need the hosted Keystatic Cloud, the project is
  pre-1.0 and quiet for months at a time, and SvelteKit support is community-only.

## Deployment shape for increment 2

- **Code and image.** A small Payload application owned by Devantler Tech, started from Payload's
  official blank template at a pinned 3.x release. Its content model covers small business sites:
  pages with localised text and images, a site-settings record and simple structured collections,
  in Danish and English. CI builds one image for amd64 and arm64, signs it and publishes it to GHCR,
  so the existing admission policy verifies it. Renovate keeps Payload current.
- **One instance per client.** Each client gets their own Payload Deployment, database, media
  location and editor accounts, in that client's tenant namespace. There is no shared multi-tenant
  instance: a separate instance is the isolation boundary, so an application bug cannot show one
  client another's content.
- **A reusable building block.** One platform definition renders the per-client set: Deployment,
  Service, HTTPRoute on the shared Gateway, CloudNativePG Cluster, barman-cloud ObjectStore and
  ScheduledBackup, ExternalSecrets from OpenBao, and a CiliumNetworkPolicy. Model it on the
  existing `WebApp` KRO definition that composes onto a `Tenant` ([tenant abstraction](../tenant-abstraction.md)),
  or use a Kustomize component if KRO cannot express the database parts cleanly.
- **State and backups.** Content lives in the instance's CloudNativePG database, backed up with
  barman-cloud to the existing S3-compatible backup storage, as Umami is. Media goes through Payload's
  S3 storage adapter to S3-compatible storage under a per-client prefix. A restore of one instance is
  tested before the pilot.
- **Logins.** Payload's own users: an administrator role for Devantler Tech and an editor role for
  the client, limited by access control to the content they edit. The editor is not behind platform
  SSO. Payload's login lockout (`maxLoginAttempts`, `lockTime`) limits password guessing.
  Password-reset email needs an email provider, which the platform does not run today.
- **How a change goes live.** SvelteKit client sites read published content at request time from
  their instance's in-cluster API, so an edit is live at once. A fully static site such as
  devantler.tech would need a publish hook that starts its build, with a narrowly scoped token; that
  waits until such a site adopts the CMS.
- **Hardening.** Runs as non-root with a read-only root filesystem and writable scratch space only
  where Next.js needs it, as Umami does. Network egress is limited to its database, the storage
  endpoint and DNS. No new Kyverno or Kubescape exceptions.
- **Off by default.** The building block ships with no production instance. The first instance is
  declared for the pilot (increment 3).

## Evidence and its limits

- The criteria results come from public documentation, licences and release data read on
  2026-10-17. Star and download counts are approximate.
- **The offline prototype of the top two has not run.** Docker is not installed on the agent host,
  and no Kubernetes cluster was started for this spike. The editing flow — sign in, edit a text and
  an image, see the page change — is therefore not yet demonstrated. Increment 2 carries that check,
  with screenshots, before anything reaches production.
- Payload does not publish a memory footprint. Increment 2 measures it before sizing capacity.

## Decisions for the maintainer

1. Accept Payload, or choose Strapi or another candidate.
2. Accept the reading of "official container image" above.
3. Where the Payload application's code lives: one shared repository (recommended: one image to keep
   current and one content model), or inside each client site's repository (a model per site, and
   an image per client to keep current).
4. Add an email provider for password-reset mail, or hand initial passwords over in person for the
   pilot.

## Sources

- Payload: [deployment](https://payloadcms.com/docs/production/deployment),
  [authentication](https://payloadcms.com/docs/authentication/overview),
  [live preview](https://payloadcms.com/docs/live-preview/overview),
  [migrations](https://payloadcms.com/docs/database/migrations),
  [enterprise features](https://payloadcms.com/enterprise),
  [releases](https://github.com/payloadcms/payload/releases).
- Strapi: [Docker](https://docs.strapi.io/cms/installation/docker),
  [deployment requirements](https://docs.strapi.io/cms/deployment),
  [self-hosted pricing](https://strapi.io/pricing-self-hosted),
  [licence](https://github.com/strapi/strapi/blob/develop/LICENSE).
- Directus: [version 12 changes](https://directus.com/docs/releases/breaking-changes/version-12),
  [licensing](https://directus.com/docs/licensing/overview), [pricing](https://directus.com/pricing).
- PocketBase: [documentation](https://pocketbase.io/docs/),
  [going to production](https://pocketbase.io/docs/going-to-production/).
- Pages CMS: [self-hosting](https://pagescms.org/docs/guides/installing/self-host/),
  [GitHub App permissions](https://pagescms.org/docs/guides/installing/github-app/),
  [collaborators](https://pagescms.org/docs/configuration/collaborators/),
  [releases](https://github.com/pages-cms/pages-cms/releases).
- TinaCMS: [self-hosted overview](https://tina.io/docs/self-hosted/overview),
  [database adapters](https://tina.io/docs/reference/self-hosted/database-adapter/overview),
  [Auth.js provider](https://tina.io/docs/reference/self-hosted/auth-provider/authjs).
- Decap CMS: [backends](https://decapcms.org/docs/backends-overview/),
  [Decap Turbo](https://decapcms.org/turbo/).
- Sveltia CMS: [backends](https://sveltiacms.app/en/docs/backends),
  [roadmap](https://sveltiacms.app/en/docs/roadmap).
- Keystatic: [GitHub mode](https://keystatic.com/docs/github-mode),
  [Keystatic Cloud](https://keystatic.com/docs/cloud).
