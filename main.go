package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/lastping-dev/terraform-provider-lastping/internal/provider"
)

// version is set by GoReleaser at build time.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/lastping-dev/lastping",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
