.PHONY: build test generate manifests

build:
	GOMAXPROCS=2 GOFLAGS=-p=1 CGO_ENABLED=0 go build -o bin/manager ./cmd/manager

test:
	GOMAXPROCS=2 GOFLAGS=-p=1 go test ./...

# Regenerate deepcopy functions, the CRD manifest, and RBAC after changing
# API types or rbac markers.
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 object paths=./api/...
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 crd rbac:roleName=manager-role paths=./... output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac

manifests: generate

clean:
	rm -rf bin/
