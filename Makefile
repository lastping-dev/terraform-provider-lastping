default: build

build:
	go build ./...

test:
	go test ./... -timeout 120s

testacc:
	TF_ACC=1 go test ./... -v -timeout 30m

lint:
	golangci-lint run

docs:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate

.PHONY: default build test testacc lint docs
