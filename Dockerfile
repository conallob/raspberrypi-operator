# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG VERSION=dev COMMIT=none DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV LDFLAGS="-s -w \
 -X github.com/conallob/raspberrypi-operator/internal/version.Version=${VERSION} \
 -X github.com/conallob/raspberrypi-operator/internal/version.Commit=${COMMIT} \
 -X github.com/conallob/raspberrypi-operator/internal/version.Date=${DATE}"
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o /out/manager ./cmd/manager \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o /out/agent ./cmd/agent

# Raspberry Pis are arm64, so that is the only platform built. The Go build
# cross-compiles on the build host; nothing here runs under emulation.
#
# One image, two binaries: the manifests pick /manager or /agent. The agent
# needs root (it is privileged anyway); the manager does not.
FROM --platform=linux/arm64 gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/manager"]
