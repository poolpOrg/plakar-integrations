GO	= go
EXT	=

PLAKAR	= plakar
VERSION	= v1.0.0

all: build

build:
	${GO} build -v -o n8n-importer${EXT} ./plugin/importer
	${GO} build -v -o n8n-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

check: test

package: build
	rm -f n8n_*.ptar
	$(PLAKAR) pkg create ./manifest.yaml $(VERSION)

uninstall:
	-$(PLAKAR) pkg rm n8n

install: package
	$(PLAKAR) pkg add -allow-unsigned ./n8n_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f n8n-importer${EXT} n8n-exporter${EXT} n8n_*.ptar

.PHONY: all build test check package uninstall install reinstall clean
