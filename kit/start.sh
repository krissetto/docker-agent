#!/bin/sh
set -eu
cd "${WORKSPACE_DIR:-$PWD}"
team=$1
state=$2
model=$3
team_edit=$4
shift 4
case "$state" in
    /*) ;;
    *) printf '%s\n' 'start.sh: stateDir must be an absolute private guest path outside the workspace' >&2; exit 2 ;;
esac
if [ "$team" = auto ]; then
    team="$PWD/hackerspace.yaml"
    [ -f "$team" ] || team=/opt/async-agent/hackerspace.yaml
fi
case "$team" in
    /*) ;;
    *) team="$PWD/$team" ;;
esac
if [ ! -f "$team" ] || [ ! -r "$team" ]; then
    printf 'start.sh: team file is not a readable regular file: %s\n' "$team" >&2
    exit 2
fi
export DOCKER_AGENT_AUTO_UPDATE=0 DOCKER_AGENT_NO_TOUR=1
export DOCKER_AGENT_HIDE_TELEMETRY_BANNER=1 TELEMETRY_ENABLED=false
# Enable SSH commit signing if the host forwards an ssh-agent key (no-op otherwise).
[ -x /opt/async-agent/git-signing.sh ] && /opt/async-agent/git-signing.sh || true
set -- --team-edit "$team_edit" "$@"
if [ -n "$model" ]; then
    set -- --model "$model" "$@"
fi
exec /opt/async-agent/docker-agent serve api --managed-api \
    --managed-api-listen-host 0.0.0.0 \
    --managed-api-listen-port 9182 \
    --working-dir "$PWD" \
    --managed-session-dir /home/agent/async-agent-sessions \
    --managed-api-state-dir "$state" \
    "$@" -- "$team"
