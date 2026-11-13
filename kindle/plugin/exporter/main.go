package main

import (
	"os"

	"github.com/PlakarKorp/integration-sdk/server"
	"github.com/poolpOrg/plakar-integrations/kindle/exporter"
)

func main() {
	server.EntrypointExporter(exporter.NewKindleExporter, os.Args)
}
