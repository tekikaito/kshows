IMAGE ?= ghcr.io/tekikaito/kshows:latest

.PHONY: build test vet run-mock image deploy

build:
	go build -o bin/kshows ./cmd/kshows

test:
	go test ./...
	node --test web/static/*.test.mjs

vet:
	go vet ./...

run-mock: build
	./bin/kshows --mock

image:
	docker build -t $(IMAGE) .

deploy:
	kubectl apply -f deploy/rbac.yaml -f deploy/deployment.yaml
