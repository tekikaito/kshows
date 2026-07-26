VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/tekikaito/kshows:$(VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint run-mock image deploy clean

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/kshows ./cmd/kshows

test:
	go test -race ./...
	node --test web/static/*.test.mjs

vet:
	go vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; falling back to go vet"; \
		go vet ./...; \
	fi

run-mock: build
	./bin/kshows --mock

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

deploy:
	kubectl apply -f deploy/rbac.yaml -f deploy/deployment.yaml

clean:
	rm -rf bin dist
