# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/manager ./cmd/manager \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/agent ./cmd/agent

# One image, two binaries: the manifests pick /manager or /agent. The agent
# needs root (it is privileged anyway); the manager does not.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/manager"]
