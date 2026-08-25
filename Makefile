BINARIES := muro murod muro-broker

.PHONY: build test test-integration lint clean install

build:
	@mkdir -p bin
	@for b in $(BINARIES); do \
		CGO_ENABLED=0 go build -o bin/$$b ./cmd/$$b; \
	done

test:
	go test ./...

test-integration:
	@command -v bwrap >/dev/null || { echo "bwrap not found, skipping"; exit 0; }
	go test -tags=integration ./test/integration/...

lint:
	gofmt -l .
	go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed, skipping"

clean:
	rm -rf bin/

install: build
	sudo scripts/install.sh
