#!/bin/sh
set -eu
# SBX sets the process working directory to the mounted host workspace.
config="$PWD/hackerspace.yaml"
if [ ! -f "$config" ]; then
    config=/opt/async-agent/hackerspace.yaml
fi
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
exec /opt/async-agent/docker-agent run "$config" \
    --working-dir "$PWD" "$@"
