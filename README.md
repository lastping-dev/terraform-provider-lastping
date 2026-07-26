# terraform-provider-lastping

Terraform provider for [LastPing](https://lastping.dev) — monitoring as code
for cron jobs, CI/CD pipelines, and AI-agent jobs.

This provider is under active development; the API surface may still change
before 1.0. See the [releases page](https://github.com/lastping-dev/terraform-provider-lastping/releases)
for what has shipped.

### Resources

| Resource | Purpose |
|---|---|
| `lastping_monitor` | A heartbeat, CI, or HTTP-probe monitor. |
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
      version = "~> 0.1"
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

### Provider configuration

| Attribute  | Env var             | Required | Description                                                  |
|------------|----------------------|----------|----------------------------------------------------------------|
| `endpoint` | `LASTPING_ENDPOINT`  | No       | LastPing API base URL. Defaults to `https://app.lastping.dev`. |
| `api_key`  | `LASTPING_API_KEY`   | Yes      | LastPing API key. Sensitive.                                    |

## Development

See the [Makefile](./Makefile) for common tasks: `make build`, `make test`,
`make lint`, `make docs`, `make sync-openapi`. Acceptance tests (`make testacc`)
require a LastPing backend and are not run in this repository's CI.

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

## License

[Mozilla Public License 2.0](./LICENSE)
