#!/bin/sh
# Stdio ACP bridge to the lifecycle-owned API and shared Kit/TUI history.
# Tools and filesystem access stay in the sandbox API, not the ACP client.
set -eu
workspace=${WORKSPACE_DIR:-$PWD}
cd "$workspace"
team=${ASYNC_AGENT_KIT_TEAM:-auto}
if [ "$team" = auto ]; then
    team="$PWD/hackerspace.yaml"
    [ -f "$team" ] || team=/opt/async-agent/hackerspace.yaml
fi
case "$team" in
    /*) ;;
    *) team="$PWD/$team" ;;
esac
if [ ! -f "$team" ] || [ ! -r "$team" ]; then
    printf 'acp.sh: team file is not a readable regular file: %s\n' "$team" >&2
    exit 2
fi
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
exec /opt/async-agent/docker-agent serve acp --managed-api --managed-api-attach \
    --managed-api-state-dir "${ASYNC_AGENT_KIT_STATE_DIR:?missing creation-time state directory}" \
    --working-dir "$PWD" "$team"
