VERSION := $(shell cat VERSION)
IMAGE   ?= ghcr.io/alvistar/wakegate

.PHONY: test build image
test:
	go vet ./... && go test ./...
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/wakegate ./cmd/wakegate
image:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(VERSION) --push .

.PHONY: build-node
build-node:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/wakegate-node ./cmd/wakegate-node
