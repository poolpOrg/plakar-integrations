GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.1.0-rc.1

all: build

build:
	${GO} build -v -o minecraft-importer${EXT} ./plugin/importer
	${GO} build -v -o minecraft-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f minecraft_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm minecraft

install: package
	$(PLAKAR) pkg add -allow-unsigned ./minecraft_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f minecraft-importer minecraft-exporter minecraft_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
