package main

import (
	"os"

	"github.com/PlakarKorp/integration-sdk/server"
	"github.com/poolpOrg/plakar-integrations/kindle/importer"
)

func main() {
	server.EntrypointImporter(importer.NewKindleImporter, os.Args)
}
