APP_NAME := Ember
APP_BUNDLE := build/bin/$(APP_NAME).app

.PHONY: build bundle install add-account reauth-account clean

build:
	go build ./...

bundle:
	sh scripts/build_bundle.sh

install: bundle
	ditto "$(APP_BUNDLE)" /Applications/$(APP_NAME).app

add-account:
	@test -n "$(NAME)" || (echo "usage: make add-account NAME=work" >&2; exit 2)
	sh scripts/add_account.sh "$(NAME)"

reauth-account:
	@test -n "$(NAME)" || (echo "usage: make reauth-account NAME=work" >&2; exit 2)
	sh scripts/add_account.sh --replace "$(NAME)"

clean:
	rm -rf build/bin
	rm -f ember
