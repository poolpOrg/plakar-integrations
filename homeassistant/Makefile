GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.1.0-rc.1

all: build

build:
	${GO} build -v -o homeassistant-importer${EXT} ./plugin/importer
	${GO} build -v -o homeassistant-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f homeassistant_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm homeassistant

install: package
	$(PLAKAR) pkg add -allow-unsigned ./homeassistant_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f homeassistant-importer homeassistant-exporter homeassistant_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
