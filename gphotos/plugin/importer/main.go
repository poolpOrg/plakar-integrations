package main

import (
	"os"

	"github.com/PlakarKorp/integration-sdk/server"
	"github.com/poolpOrg/plakar-integrations/gphotos/importer"
)

func main() {
	server.EntrypointImporter(importer.NewImporter, os.Args)
}
