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

FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS task
RUN apk add --no-cache curl
WORKDIR /task
ARG TARGETOS TARGETARCH
# SHA256 pins from go-task/task v3.53.1's task_checksums.txt.
RUN <<EOF_TASK
set -eux
test "$TARGETOS" = linux
case "$TARGETARCH" in
    amd64) checksum=a54a408f6861ff921f6e87774180db31bacd8c1e7c944ca696db9fea49a82fc7 ;;
    arm64) checksum=e3ad19101493a0112e1f22ae8ccc54bf03e533b1076a0ca1e6c782a09ad2e588 ;;
    *) echo "Unsupported Task architecture: $TARGETARCH" >&2; exit 1 ;;
esac
curl --fail --silent --show-error --location "https://github.com/go-task/task/releases/download/v3.53.1/task_linux_${TARGETARCH}.tar.gz" -o task.tar.gz
printf '%s  task.tar.gz\n' "$checksum" | sha256sum -c -
tar -xzf task.tar.gz task LICENSE
EOF_TASK

FROM docker/sandbox-templates:shell-docker@sha256:5fc81bc7a127e59d81b244a06831ae3212a0310b2e5a0349c54e29249e45e919 AS runtime
ARG ASYNC_AGENT_KIT_VERSION="0.1.0"
LABEL com.docker.async-agent.kit.version=$ASYNC_AGENT_KIT_VERSION
USER root
COPY --from=builder --chmod=0755 /docker-agent /opt/async-agent/docker-agent
COPY --from=task --chmod=0755 /task/task /usr/local/bin/task
COPY --from=task --chmod=0644 /task/LICENSE /usr/local/share/licenses/task/LICENSE
COPY --chmod=0755 kit/launch.sh /opt/async-agent/launch.sh
COPY kit/hackerspace.yaml /opt/async-agent/hackerspace.yaml
RUN install -d -m 0700 -o agent -g agent /home/agent/.config /home/agent/.config/cagent
COPY --chown=agent:agent --chmod=0600 kit/user-config.yaml /home/agent/.config/cagent/config.yaml
ENV DOCKER_AGENT_AUTO_UPDATE=0 \
    DOCKER_AGENT_NO_TOUR=1 \
    DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 \
    TELEMETRY_ENABLED=false
USER agent

FROM runtime AS runtime-v3
ENTRYPOINT ["/opt/async-agent/launch.sh"]
CMD []
