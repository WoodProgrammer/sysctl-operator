# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

# Force module mode so the build never falls back to GOPATH/legacy resolution
# (this is what produces "package sysctl-operator/internal/... is not in std").
ENV GO111MODULE=on
ENV CGO_ENABLED=0

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Cache deps before building and copying source so that we don't need to
# re-download as much and source changes don't invalidate the downloaded layer.
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build. GOARCH has no default so the binary is built for the host/target
# platform; see the BUILDPLATFORM notes in the kubebuilder scaffold.
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as a minimal base image to package the manager binary.
# Refer to https://github.com/GoogleContainerTools/distroless for more details.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
