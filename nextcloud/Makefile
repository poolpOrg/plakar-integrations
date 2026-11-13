GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.1.0-rc.1

all: build

build:
	${GO} build -v -o nextcloud-importer${EXT} ./plugin/importer
	${GO} build -v -o nextcloud-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f nextcloud_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm nextcloud

install: package
	$(PLAKAR) pkg add -allow-unsigned ./nextcloud_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f nextcloud-importer nextcloud-exporter nextcloud_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
