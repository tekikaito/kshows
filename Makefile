IMAGE ?= ghcr.io/tekikaito/kshows:latest

.PHONY: build test vet lint run-mock image deploy

build:
	go build -o bin/kshows ./cmd/kshows

test:
	go test ./...
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
	docker build -t $(IMAGE) .

deploy:
	kubectl apply -f deploy/rbac.yaml -f deploy/deployment.yaml
