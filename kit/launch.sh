#!/bin/sh
set -eu
# Server configuration belongs to sandbox creation, never a connection.
case "${1-}" in
    --team|--team=*)
        printf '%s\n' 'launch.sh: --team is bound at creation; recreate with the team Kit argument' >&2
        exit 2
        ;;
esac
export ASYNC_AGENT_KIT_CONNECTION=1
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
cd "/workspace"
exec /opt/async-agent/docker-agent run /opt/async-agent/hackerspace.yaml \
    --managed-api --managed-api-attach \
    --managed-api-state-dir "${ASYNC_AGENT_KIT_STATE_DIR:?missing creation-time state directory}" "$@"
