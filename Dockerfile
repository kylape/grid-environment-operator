FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder
USER root
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOMAXPROCS=2 GOFLAGS=-p=1 CGO_ENABLED=0 go build -o /manager ./cmd/manager

FROM registry.access.redhat.com/ubi9/ubi-micro:9.6
WORKDIR /
COPY --from=builder /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
