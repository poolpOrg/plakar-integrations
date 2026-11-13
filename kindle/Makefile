GO=go
EXT=

PLAKAR  = plakar
VERSION = v1.0.0

GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
PTAR   := kindle_$(VERSION)_$(GOOS)_$(GOARCH).ptar

all: build

build:
	${GO} build -v -o kindleImporter${EXT} ./plugin/importer
	${GO} build -v -o kindleExporter${EXT} ./plugin/exporter

package: build
	rm -f $(PTAR)
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm kindle

install: package
	$(PLAKAR) pkg add ./$(PTAR)

reinstall: uninstall install

test:
	${GO} test -v ./...

check: test

clean:
	rm -f kindleImporter${EXT} kindleExporter${EXT} kindle_*.ptar

.PHONY: all build package uninstall install reinstall test check clean
