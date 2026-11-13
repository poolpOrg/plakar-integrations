GO  = go
EXT =

PLAKAR  = plakar
VERSION = v1.1.0-rc.1

all: build

build:
	${GO} build -v -o iphone-importer${EXT} ./plugin/importer
	${GO} build -v -o iphone-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

# hardware runs the tests that need an iPhone or iPad connected over USB.
hardware:
	PLAKAR_IPHONE_HARDWARE=1 ${GO} test -v -run Hardware ./...

check: test

package: build
	rm -f iphone_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm iphone

install: package
	$(PLAKAR) pkg add -allow-unsigned ./iphone_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f iphone-importer iphone-exporter iphone_*.ptar

.PHONY: all build test hardware check package uninstall install reinstall clean
