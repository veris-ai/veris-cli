VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test e2e lint dist image clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/veris ./cmd/veris

test:
	go test -race ./...

e2e: build
	bash testdata/e2e.sh $(PWD)/bin/veris

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
			-ldflags="$(LDFLAGS)" -o dist/veris-$$os-$$arch$$ext ./cmd/veris || exit 1; \
	done
	@ls -lh dist/

# The runner image, published to GHCR by .github/workflows/image.yml. This
# target is for reproducing that build locally; CI is what publishes.
#
# BOTH architectures, because this image runs on a developer's machine rather
# than on a cluster we choose. An amd64-only image on Apple Silicon does not
# fail at the workload -- it fails installing iptables inside the proxy
# ("Failed to initialize nft: Protocol not supported"), because emulation does
# not carry the netfilter syscalls, which reads as a broken proxy rather than
# as a missing architecture.
IMAGE ?= ghcr.io/veris-ai/veris-proxy

image:
	docker buildx build --platform linux/amd64,linux/arm64 \
		-f container/Dockerfile --target runner --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):runner \
		$(if $(PUSH),--push,--load) .

clean:
	rm -rf bin dist
