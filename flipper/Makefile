GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.1.0-rc.1

all: build

build:
	${GO} build -v -o flipper-importer${EXT} ./plugin/importer
	${GO} build -v -o flipper-exporter${EXT} ./plugin/exporter
	${GO} build -v -o flipper-storage${EXT}  ./plugin/storage

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f flipper_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm flipper

install: package
	$(PLAKAR) pkg add -allow-unsigned ./flipper_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f flipper-importer flipper-exporter flipper-storage flipper_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
