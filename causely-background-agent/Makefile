IMAGE ?= docker.io/causelyai/causely-hermes-harness
TAG ?= 0.1.0
# Platforms published by `make push` (one multi-arch image index).
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: test
test:
	go test ./...
	go vet ./...

# Local image for this machine's platform, loaded into Docker.
.PHONY: build
build: test
	docker buildx build --load --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .

# Build every platform in PLATFORMS without loading or pushing (a CI check).
.PHONY: build-all
build-all: test
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .

.PHONY: push
push: test
	docker buildx build --platform $(PLATFORMS) --push --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .
