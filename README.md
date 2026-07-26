# terraform-provider-lastping

Terraform provider for [LastPing](https://lastping.dev) — monitoring as code
for cron jobs, CI/CD pipelines, and AI-agent jobs.

This provider is under active development. At this stage it only configures
the provider itself; no resources or data sources are implemented yet. Watch
this repository or the [releases page](https://github.com/lastping-dev/terraform-provider-lastping/releases)
for updates as resources land.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
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
`make lint`, `make docs`. Acceptance tests (`make testacc`) require a
LastPing backend and are not run in this repository's CI.

## License

[Mozilla Public License 2.0](./LICENSE)
