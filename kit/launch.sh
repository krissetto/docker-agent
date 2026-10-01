#!/bin/sh
set -eu
# SBX sets the process working directory to the mounted host workspace.
# Only a leading launcher option is parsed; agent options and prompts stay intact.
config=
case "${1-}" in
    --team)
        if [ "$#" -lt 2 ] || [ -z "$2" ]; then
            printf '%s\n' 'launch.sh: --team requires a file path' >&2
            exit 2
        fi
        config=$2
        shift 2
        ;;
    --team=*)
        config=${1#--team=}
        if [ -z "$config" ]; then
            printf '%s\n' 'launch.sh: --team requires a file path' >&2
            exit 2
        fi
        shift
        ;;
esac
if [ -n "$config" ]; then
    case "$config" in
        /*) ;;
        *) config="$PWD/$config" ;;
    esac
    if [ ! -f "$config" ] || [ ! -r "$config" ]; then
        printf 'launch.sh: team file is not a readable regular file: %s\n' "$config" >&2
        exit 2
    fi
else
    config="$PWD/hackerspace.yaml"
    if [ ! -f "$config" ]; then
        config=/opt/async-agent/hackerspace.yaml
    fi
fi
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
exec /opt/async-agent/docker-agent run "$config" \
    --working-dir "$PWD" "$@"
