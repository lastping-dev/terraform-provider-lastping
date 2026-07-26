default: build

# Where `make sync-openapi` pulls the spec from. This is the real served path;
# the docs UI at /docs/api/ is a viewer, not the document.
OPENAPI_URL ?= https://app.lastping.dev/openapi.yaml

# tfplugindocs drives a real Terraform CLI, and a CLI older than this does not
# know what an ephemeral resource is. See the `docs` target.
MIN_TF_VERSION := 1.10.0

build:
	go build ./...

test:
	go test ./... -timeout 120s

testacc:
	TF_ACC=1 go test ./... -v -timeout 30m

lint:
	golangci-lint run

# Refresh the vendored OpenAPI spec the contract test reads. The written file is
# checked for an `openapi:` key before it replaces the current one, because a
# proxy or error page served with a 200 would otherwise silently become the
# contract every future test is judged against.
sync-openapi:
	@tmp=$$(mktemp) && \
	curl -fsS "$(OPENAPI_URL)" -o "$$tmp" && \
	head -n 1 "$$tmp" | grep -q '^openapi:' || { \
		echo "sync-openapi: $(OPENAPI_URL) did not return an OpenAPI document:"; \
		head -n 5 "$$tmp"; rm -f "$$tmp"; exit 1; \
	}; \
	mv "$$tmp" testdata/openapi.yaml && \
	echo "sync-openapi: updated testdata/openapi.yaml from $(OPENAPI_URL)"

# Regenerate docs/ from the provider schema and examples/.
#
# GUARD: tfplugindocs asks the Terraform CLI on PATH to describe the provider,
# and a CLI older than 1.10 has no concept of ephemeral resources — it reports
# none, and tfplugindocs then DELETES docs/ephemeral-resources/api_key.md as
# though the page were stale. That deletion is easy to commit by accident, and
# CI's `git diff --exit-code -- docs/` cannot catch it: once the file is gone
# from the branch, regenerating on a modern CLI recreates it and the diff is
# clean. Refusing to run on an old CLI is the only place to stop it.
docs:
	@command -v terraform >/dev/null 2>&1 || { \
		echo "make docs: no terraform on PATH."; \
		echo "tfplugindocs shells out to the Terraform CLI; install $(MIN_TF_VERSION) or newer."; \
		exit 1; \
	}
	@v=$$(terraform version | head -n 1 | sed -e 's/^Terraform v//' -e 's/[^0-9.].*$$//'); \
	awk -v have="$$v" -v want="$(MIN_TF_VERSION)" 'BEGIN { \
		n = split(have, a, "."); split(want, b, "."); \
		if (n == 0) exit 1; \
		for (i = 1; i <= 3; i++) { \
			x = a[i] + 0; y = b[i] + 0; \
			if (x > y) exit 0; \
			if (x < y) exit 1; \
		} \
		exit 0 }' || { \
		echo "make docs: Terraform $$v is too old (need $(MIN_TF_VERSION) or newer)."; \
		echo ""; \
		echo "A CLI without ephemeral-resource support makes tfplugindocs delete"; \
		echo "docs/ephemeral-resources/*.md instead of regenerating it, and CI cannot"; \
		echo "detect a page that is already missing from the branch."; \
		echo ""; \
		echo "Install a newer CLI, or point make at one:"; \
		echo "  PATH=/path/to/terraform-$(MIN_TF_VERSION)/bin:\$$PATH make docs"; \
		exit 1; \
	}
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate

.PHONY: default build test testacc lint sync-openapi docs
