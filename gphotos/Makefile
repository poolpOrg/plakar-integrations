GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.0.0

all: build

build:
	${GO} build -v -o gphotos-importer${EXT} ./plugin/importer

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f gphotos_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm gphotos

install: package
	$(PLAKAR) pkg add -allow-unsigned ./gphotos_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f gphotos-importer gphotos_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
