BINARY  := rollouts-plugin-kedify
IMG     := ghcr.io/kedify/argo-rollouts-plugin:dev
GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: all
all: build

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) .

.PHONY: test
test:
	go test ./... -count=1

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: k3d-import
k3d-import: docker-build
	k3d image import $(IMG)

.PHONY: clean
clean:
	rm -f $(BINARY)
