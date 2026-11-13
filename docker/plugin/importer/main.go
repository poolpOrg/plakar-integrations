package main

import (
	"os"

	sdk "github.com/PlakarKorp/go-kloset-sdk"
	"github.com/poolpOrg/plakar-integrations/docker/importer"
)

func main() {
	sdk.EntrypointImporter(os.Args, importer.NewImporter)
}
