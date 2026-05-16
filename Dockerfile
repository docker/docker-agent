# syntax=docker/dockerfile:1

ARG GO_VERSION="1.26.3"
ARG ALPINE_VERSION="3.22"
ARG XX_VERSION="1.9.0"

# xx is a helper for cross-compilation
FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

# osxcross contains the MacOSX cross toolchain for xx
FROM crazymax/osxcross:15.5-debian AS osxcross

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder-base
COPY --from=xx / /
RUN apk add --no-cache clang zig
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download
ENV CGO_ENABLED=1

FROM builder-base AS builder-linux
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/var/cache/apk,id=apk-$TARGETPLATFORM,sharing=locked \
    xx-apk add musl-dev
COPY . ./
ARG GIT_TAG
ARG GIT_COMMIT
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETPLATFORM \
    --mount=type=cache,target=/go/pkg/mod <<EOT
    set -ex
    test "$TARGETOS" = "linux"
    export XX_GO_PREFER_C_COMPILER=zig
    xx-go build -trimpath -tags no_audio -ldflags "-s -w -linkmode=external -X 'github.com/docker/docker-agent/pkg/version.Version=$GIT_TAG' -X 'github.com/docker/docker-agent/pkg/version.Commit=$GIT_COMMIT'" -o /binaries/docker-agent-$TARGETOS-$TARGETARCH .
    xx-verify --static /binaries/docker-agent-$TARGETOS-$TARGETARCH
EOT

FROM builder-base AS builder-cross
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/var/cache/apk,id=apk-$TARGETPLATFORM,sharing=locked \
    xx-apk add musl-dev
COPY . ./
ARG GIT_TAG
ARG GIT_COMMIT
RUN --mount=type=bind,from=osxcross,src=/osxsdk,target=/xx-sdk \
    --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETPLATFORM \
    --mount=type=cache,target=/go/pkg/mod <<EOT
    set -ex
    if [ "$TARGETOS" != "darwin" ]; then
      export XX_GO_PREFER_C_COMPILER=zig
    fi
    xx-go build -trimpath -tags no_audio -ldflags "-s -w -linkmode=external -X 'github.com/docker/docker-agent/pkg/version.Version=$GIT_TAG' -X 'github.com/docker/docker-agent/pkg/version.Commit=$GIT_COMMIT'" -o /binaries/docker-agent-$TARGETOS-$TARGETARCH .
    xx-verify --static /binaries/docker-agent-$TARGETOS-$TARGETARCH
    if [ "$TARGETOS" = "windows" ]; then
      mv /binaries/docker-agent-$TARGETOS-$TARGETARCH /binaries/docker-agent-$TARGETOS-$TARGETARCH.exe
    fi
EOT

FROM scratch AS local
ARG TARGETOS TARGETARCH
COPY --from=builder-cross /binaries/docker-agent-$TARGETOS-$TARGETARCH* docker-agent

FROM scratch AS cross
COPY --from=builder-cross /binaries .

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates docker-cli && \
    addgroup -S docker-agent && adduser -S -G docker-agent docker-agent && \
    mkdir /data /work && chmod 777 /data /work
ARG TARGETOS TARGETARCH
ENV DOCKER_MCP_IN_CONTAINER=1
ENV TERM=xterm-256color
COPY --from=docker/mcp-gateway:v2 /docker-mcp /usr/local/lib/docker/cli-plugins/
COPY --from=builder-linux /binaries/docker-agent-$TARGETOS-$TARGETARCH /docker-agent
USER docker-agent
WORKDIR /work
ENTRYPOINT ["/docker-agent"]
