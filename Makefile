VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test e2e lint dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/veris-proxy ./cmd/veris-proxy

test:
	go test -race ./...

e2e: build
	bash testdata/e2e.sh $(PWD)/bin/veris-proxy

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

# Every target is CGO_ENABLED=0, so each artifact is a single static binary
# that drops into any image, including Alpine and distroless.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
			-ldflags="$(LDFLAGS)" -o dist/veris-proxy-$$os-$$arch$$ext ./cmd/veris-proxy || exit 1; \
	done
	@ls -lh dist/

clean:
	rm -rf bin dist
