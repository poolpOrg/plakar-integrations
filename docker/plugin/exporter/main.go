package main

import (
	"os"

	sdk "github.com/PlakarKorp/go-kloset-sdk"
	"github.com/poolpOrg/plakar-integrations/docker"
)

func main() {
	sdk.EntrypointExporter(os.Args, docker.NewExporter)
}
