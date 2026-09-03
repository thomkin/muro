BINARIES := muro murod muro-broker muro-shim muro-toolstub muro-quiet-chat

.PHONY: build test test-integration lint clean install

build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -o bin/$$b ./cmd/$$b; \
	done

test:
	go test ./...

test-integration: build
	@command -v bwrap >/dev/null || { echo "bwrap not found, skipping"; exit 0; }
	@command -v slirp4netns >/dev/null || { echo "slirp4netns not found, skipping"; exit 0; }
	@command -v nft >/dev/null || { echo "nft not found, skipping"; exit 0; }
	PATH="$(CURDIR)/bin:$$PATH" go test -tags=integration ./...

lint:
	gofmt -l .
	go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

clean:
	rm -rf bin/

install: build
	scripts/install.sh
