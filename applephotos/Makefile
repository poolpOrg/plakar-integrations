GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.0.0

all: build

build:
	${GO} build -v -o applephotos-importer${EXT} ./plugin/importer

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f applephotos_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm applephotos

install: package
	$(PLAKAR) pkg add -allow-unsigned ./applephotos_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f applephotos-importer applephotos_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
