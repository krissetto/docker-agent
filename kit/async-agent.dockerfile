# Linux static build follows the repository Dockerfile's builder-linux target.
ARG GO_VERSION="1.27.0"
ARG ALPINE_VERSION="3.23"
ARG XX_VERSION="1.9.0"

FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder
COPY --from=xx / /
RUN apk add --no-cache clang zig
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download
ENV CGO_ENABLED=1
ARG TARGETPLATFORM TARGETOS TARGETARCH
RUN --mount=type=cache,target=/var/cache/apk,id=apk-$TARGETPLATFORM,sharing=locked \
    xx-apk add musl-dev
COPY go.mod go.sum main.go ./
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETPLATFORM \
    --mount=type=cache,target=/go/pkg/mod <<EOF_BUILD
set -eux
test "$TARGETOS" = linux
export XX_GO_PREFER_C_COMPILER=zig
xx-go build -trimpath -tags no_audio -ldflags "-s -w -linkmode=external -X github.com/docker/docker-agent/pkg/version.Version=dev -X github.com/docker/docker-agent/pkg/version.Commit=local-source" -o /docker-agent .
xx-verify --static /docker-agent
EOF_BUILD

FROM docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919 AS runtime
ARG ASYNC_AGENT_KIT_VERSION="0.1.0"
LABEL com.docker.async-agent.kit.version=$ASYNC_AGENT_KIT_VERSION
USER root
COPY --from=builder --chmod=0755 /docker-agent /opt/async-agent/docker-agent
COPY --chmod=0755 kit/launch.sh /opt/async-agent/launch.sh
COPY kit/hackerspace.yaml /opt/async-agent/hackerspace.yaml
ENV DOCKER_AGENT_AUTO_UPDATE=0 \
    DOCKER_AGENT_NO_TOUR=1 \
    DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 \
    TELEMETRY_ENABLED=false
USER agent

FROM runtime AS runtime-v3
ENTRYPOINT ["/opt/async-agent/launch.sh"]
CMD []
