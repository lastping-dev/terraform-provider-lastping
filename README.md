<p align="center">
  <a href="https://lastping.dev/terraform">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/hero-dark.svg">
      <source media="(prefers-color-scheme: light)" srcset=".github/hero-light.svg">
      <img alt="LastPing. A stopped agent looks exactly like a thinking one. Monitoring for AI agent runs, cron jobs and CI/CD. Free for individuals." src=".github/hero-light.svg" width="100%">
    </picture>
  </a>
</p>

<p align="center">
  <a href="https://lastping.dev"><img alt="Website" src="https://img.shields.io/badge/Website-0f766e?style=for-the-badge"></a>
  <a href="https://lastping.dev/terraform"><img alt="Terraform docs" src="https://img.shields.io/badge/Terraform%20docs-2f3a49?style=for-the-badge"></a>
  <a href="https://app.lastping.dev/docs"><img alt="API docs" src="https://img.shields.io/badge/API%20docs-2f3a49?style=for-the-badge"></a>
  <a href="https://app.lastping.dev/status/lastping-self"><img alt="Status" src="https://img.shields.io/badge/Status-2f3a49?style=for-the-badge"></a>
</p>

<p align="center">
  <a href="https://registry.terraform.io/providers/lastping-dev/lastping"><img alt="Terraform Registry version" src="https://img.shields.io/github/v/release/lastping-dev/terraform-provider-lastping?label=registry&color=2dd4bf"></a>
  <a href="https://github.com/lastping-dev/terraform-provider-lastping/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/lastping-dev/terraform-provider-lastping/actions/workflows/ci.yml/badge.svg"></a>
  <a href="./LICENSE"><img alt="License: MPL-2.0" src="https://img.shields.io/github/license/lastping-dev/terraform-provider-lastping"></a>
</p>

# terraform-provider-lastping

The official Terraform provider for [LastPing](https://lastping.dev), monitoring
as code for cron jobs, CI/CD pipelines and AI agent runs. LastPing waits for
your jobs to check in and opens an incident when an expected signal never
arrives. This provider keeps monitors, agents, alert destinations, routing,
alert messages, status pages and API keys in the same configuration as the
infrastructure they watch. **Free for individuals.**

## Quick start

```hcl
terraform {
  required_providers {
    lastping = {
      source  = "lastping-dev/lastping"
      version = "~> 0.5"
    }
  }
}

provider "lastping" {}

# A nightly backup job that pings in on a cron schedule. grace_s is how long
# after the scheduled 03:00 UTC run the monitor waits before alerting.
resource "lastping_monitor" "nightly_backup" {
  name          = "Nightly backup"
  slug          = "nightly-backup"
  schedule_kind = "cron"
  cron_expr     = "0 3 * * *"
  tz            = "UTC"
  grace_s       = 900
}

output "ping_url" {
  value = lastping_monitor.nightly_backup.ping_url
}
```

```sh
export LASTPING_API_KEY=lp_your_key   # Settings, API keys at app.lastping.dev
terraform init && terraform apply
```

Then have the job request `ping_url` when it finishes. More examples, from
HTTP probes to output assertions and spend guards, are in
[`examples/`](./examples) and in the
[Registry documentation](https://registry.terraform.io/providers/lastping-dev/lastping/latest/docs).

This provider is under active development; the API surface may still change
before 1.0. See the [releases page](https://github.com/lastping-dev/terraform-provider-lastping/releases)
for what has shipped.

## What it manages

### Resources

| Resource | Purpose |
|---|---|
| `lastping_monitor` | A heartbeat, CI, or HTTP-probe monitor. |
| `lastping_agent` | An autonomous worker that owns monitors, with a live health rollup. |
| `lastping_destination` | Where alerts are delivered (Slack, email, webhook, …). |
| `lastping_route` | Which destinations a monitor notifies for one event type. |
| `lastping_alert_template` | Custom alert message bodies for a monitor. |
| `lastping_status_page` | A public or private status page over a set of monitors. |
| `lastping_api_key` | A managed API key. |

`lastping_api_key` is also available as an
[ephemeral resource](https://developer.hashicorp.com/terraform/language/resources/ephemeral),
which mints a short-lived key without ever writing it to state. That form needs
Terraform 1.10 or newer.

### Data sources

| Data source | Purpose |
|---|---|
| `lastping_monitor` | One monitor, by `slug` or `id`. |
| `lastping_monitors` | Every monitor in the project, optionally filtered by `tag`. |
| `lastping_destination` | One destination, by `id` or `name`. |
| `lastping_incidents` | One monitor's incident history (`monitor_id` is required — the API is per-monitor only). |
| `lastping_metrics` | The project's Prometheus exposition, verbatim. |
| `lastping_project` | The project the configured API key belongs to. |

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
  (>= 1.10 for the `lastping_api_key` ephemeral resource)
- A LastPing account and API key ([app.lastping.dev/app/settings](https://app.lastping.dev/app/settings))

## Using the provider

```hcl
terraform {
  required_providers {
    lastping = {
      source  = "lastping-dev/lastping"
      version = "~> 0.5"
    }
  }
}

provider "lastping" {
  # api_key can also be supplied via the LASTPING_API_KEY environment
  # variable, which is the recommended approach so the key does not
  # appear in configuration or state.
}
```

## Authentication

The provider needs a LastPing API key to authenticate. Create one at
[app.lastping.dev/app/settings](https://app.lastping.dev/app/settings), then
supply it either:

- via the `LASTPING_API_KEY` environment variable (recommended), or
- via the `api_key` attribute on the `provider "lastping"` block (marked
  sensitive, but present in configuration and state — prefer the
  environment variable).

**Key scopes.** Every LastPing key carries a scope: `read` (every GET), `write`
(everything except API key management) or `admin` (everything, key management
included). A new key gets `write` unless it asks for something else — that
default is the API's, not the provider's, so omitting `scope` on a
`lastping_api_key` means "let the API decide for a new key, and leave an
existing key's scope alone". `write` is enough for every resource here
**except** `lastping_api_key` — managing keys is precisely what `write` excludes
— so a configuration that creates API keys must be run with an `admin` key. A run that only needs key-management power for its own duration can mint a
short-lived one with the ephemeral resource's `scope = "admin"`; keys minted
that way are revoked with it, so anything that must outlive the run has to be
created by a credential that does.

### Provider configuration

| Attribute  | Env var             | Required | Description                                                  |
|------------|----------------------|----------|----------------------------------------------------------------|
| `endpoint` | `LASTPING_ENDPOINT`  | No       | LastPing API base URL. Defaults to `https://app.lastping.dev`. |
| `api_key`  | `LASTPING_API_KEY`   | Yes      | LastPing API key. Sensitive.                                    |

## Development

See the [Makefile](./Makefile) for common tasks: `make build`, `make test`,
`make lint`, `make docs`, `make sync-openapi`. Acceptance tests (`make testacc`)
require a LastPing backend and are not run in this repository's CI.

### Acceptance test backend

`make testacc` runs against a LastPing account through the public API, exactly
as a practitioner's configuration does. It reads `LASTPING_API_KEY` (an `admin`
API key for the project the tests should use) and `LASTPING_ENDPOINT` (the API
base URL, defaulting to `https://app.lastping.dev`), and skips entirely without
the key. Use a project set aside for testing: the suite creates and deletes real
monitors, destinations and routes in it.

**The project behind that key must have a verified email destination.** LastPing
auto-routes every new monitor's `down`, `fail` and `recovery` events to the
project's default email destination, and a project without one takes that path
never — which makes the acceptance suite pass against behaviour real users never
see. That is exactly how the route resource shipped unable to create a monitor
and its routes in the same apply: locally the monitor came back unrouted, so
nothing ever collided. Before running the suite, add an email destination to
the project in the console (or create one with `POST /api/v1/channels`, or a
`lastping_destination` with `kind = "email"`) and click the verification link
LastPing sends to that address.

Because this is checked once, centrally, in `testAccPreCheck`, every acceptance
test fails with those instructions rather than skipping — there is no longer a
code path where the suite passes against a project missing this destination.

**The scope tests need no extra seeding.** `TestAccAPIKey_scopeCapIsEnforcedByTheServer`
mints its own `write` key with the configured key and authenticates a second,
aliased provider with it, because the acceptance key is `admin`. The test accepts either refusal — the create-time
scope cap (400, `max_scope`) or, once the server enforces per-route scopes, the
route's own 403 (`required_scope`) — since which one arrives is a server-side
flag this repository does not control.

**`TestAccStatusPage_importOfForeignSlugIsNotFound` additionally wants a SECOND,
distinct project's API key**, in `LASTPING_ACC_FOREIGN_API_KEY`, to prove
cross-tenant status-page import stays invisible across projects. Unlike the
email destination above, a second project is more than a one-step setup, so it
is optional for local work and the test skips — loudly, naming the variable and
what goes unverified — rather than failing when it is unset. Running this
automatically with a second project's key is an open item.

### API contract test

`testdata/openapi.yaml` is a vendored copy of the published
[OpenAPI spec](https://app.lastping.dev/openapi.yaml), refreshed with
`make sync-openapi`. `internal/provider/contract_test.go` asserts that every
attribute the provider sends exists as a request property in that spec, and
every attribute it reads back exists as a response property — so a rename or
removal on the API side fails a pull request instead of a user's
`terraform apply`. It is a plain unit test and needs no backend.

Deliberate mismatches (a path parameter, a provider-side concept such as `ttl`)
are declared in the test with the reason they cannot be a spec property.

### Documentation

Registry documentation under [`docs/`](./docs) is generated from the schema and
the [`examples/`](./examples) directory by
[terraform-plugin-docs](https://github.com/hashicorp/terraform-plugin-docs); run
`make docs` and commit the result whenever the schema or an example changes. CI
fails if the committed docs are stale.

`make docs` requires Terraform 1.10 or newer on `PATH` and refuses to run
otherwise: an older CLI cannot see ephemeral resources, so tfplugindocs deletes
`docs/ephemeral-resources/` rather than regenerating it, and CI's staleness
check cannot detect a page that is already missing.

## Releasing

Releases are cut by pushing a tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` then builds every platform with
[GoReleaser](https://goreleaser.com), signs the `SHA256SUMS` file with GPG, and
publishes a GitHub release. The Terraform Registry picks that release up over
its webhook and serves the new version.

Two repository secrets have to exist before the first tag:

| Secret | Contents |
|---|---|
| `GPG_PRIVATE_KEY` | The ASCII-armored private signing key (`gpg --armor --export-secret-keys <id>`). |
| `PASSPHRASE` | That key's passphrase. |

The public half of the same key must also be uploaded to the Terraform Registry
under the publishing namespace.

**The signing key must be RSA.** The registry rejects ECC keys, and ECC
(Curve25519) is what modern GnuPG generates by default — so `gpg --gen-key`,
and the default path through `gpg --full-generate-key`, both produce a key the
registry will not accept. Ask for RSA explicitly:

```sh
gpg --full-generate-key   # choose "(1) RSA and RSA", 4096 bits
# or, non-interactively:
gpg --quick-generate-key "Your Name <you@example.com>" rsa4096 sign never
```

`terraform-registry-manifest.json` at the repository root declares protocol
version 6 (this provider is built on terraform-plugin-framework). It is both
committed and shipped as a release artifact; without it the registry assumes
protocol 5 and the provider fails to load for everyone.

The release config can be exercised without tagging anything:

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=sign,publish   # output in dist/
```

## License

[Mozilla Public License 2.0](./LICENSE)
