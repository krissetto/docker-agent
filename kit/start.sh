#!/bin/sh
set -eu
cd "/workspace"
team=$1
state=$2
model=$3
shift 3
case "$state" in
    /*) ;;
    *) printf '%s\n' 'start.sh: stateDir must be an absolute private guest path outside /workspace' >&2; exit 2 ;;
esac
if [ "$team" = auto ]; then
    team="/workspace/hackerspace.yaml"
    [ -f "$team" ] || team=/opt/async-agent/hackerspace.yaml
fi
case "$team" in
    /*) ;;
    *) team="/workspace/$team" ;;
esac
if [ ! -f "$team" ] || [ ! -r "$team" ]; then
    printf 'start.sh: team file is not a readable regular file: %s\n' "$team" >&2
    exit 2
fi
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
if [ -n "$model" ]; then
    set -- --model "$model"
fi
exec /opt/async-agent/docker-agent serve api --managed-api \
    --working-dir "/workspace" \
    --managed-api-state-dir "$state" \
    "$@" -- "$team"
