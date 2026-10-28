APP_NAME := Ember
APP_BUNDLE := build/bin/$(APP_NAME).app

.PHONY: build bundle install clean

build:
	go build ./...

bundle:
	sh scripts/build_bundle.sh

install: bundle
	ditto "$(APP_BUNDLE)" /Applications/$(APP_NAME).app

clean:
	rm -rf build/bin
	rm -f ember
