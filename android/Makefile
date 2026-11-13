GO	= go
EXT	=

PLAKAR	= plakar
VERSION	= v1.1.0-rc.1

all: build

build:
	${GO} build -v -o android-importer${EXT} ./plugin/importer
	${GO} build -v -o android-exporter${EXT} ./plugin/exporter

test:
	${GO} vet ./...
	${GO} test ./...

# Runs the tests that need a real device, selected by its adb serial.
hwtest:
	@test -n "$(SERIAL)" || (echo "usage: make hwtest SERIAL=<adb serial>"; exit 1)
	PLAKAR_ANDROID_TEST_SERIAL=$(SERIAL) ${GO} test -v -count=1 -run Hardware ./...

check: test

package: build
	rm -f android_${VERSION}_*.ptar
	${PLAKAR} pkg create ./manifest.yaml ${VERSION}

uninstall:
	-$(PLAKAR) pkg rm android

install: package
	$(PLAKAR) pkg add -allow-unsigned ./android_${VERSION}_*.ptar

reinstall: uninstall install

clean:
	rm -f android-importer${EXT} android-exporter${EXT} android_*.ptar

.PHONY: all build test hwtest check package uninstall install reinstall clean
