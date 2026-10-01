#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'kit: %s\n' "$*" >&2; exit 1; }

# CLI_ARGS is data, not a shell command or a list to eval.
[[ $# -le 1 ]] || fail 'usage: build.sh [repository:tag]'
reference=${1:-kagent:local-v3}
[[ $reference != -* && $reference != *@* ]] || fail 'expected a mutable image reference'
last=${reference##*/}
if [[ $last == *:* ]]; then
    repository=${reference%:*}
    tag=${reference##*:}
else
    repository=$reference
    tag=latest
fi
component='[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*'
registry=docker.io
path=$repository
if [[ $repository == */* ]]; then
    first=${repository%%/*}
    if [[ $first == *.* || $first == *:* || $first == localhost ]]; then
        [[ $first =~ ^[a-zA-Z0-9]+([.-][a-zA-Z0-9]+)*(:[0-9]+)?$ ]] || fail 'invalid registry host'
        registry=$first
        path=${repository#*/}
    fi
fi
[[ $path =~ ^${component}(/${component})*$ && ${#repository} -le 255 ]] || fail 'invalid image repository (expected lowercase repository[:tag])'
[[ $tag =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$ && ${#tag} -le 128 ]] || fail 'invalid image tag'
# Docker accepts familiar names; ORAS requires an explicit registry. Use one
# effective reference for builds, generated digest pins, output and publication.
if [[ $registry == docker.io && $path != */* ]]; then
    path=library/$path
fi
repository=$registry/$path
reference=$repository:$tag
platform=${KIT_PLATFORM:-${DOCKER_DEFAULT_PLATFORM:-linux/amd64}}
[[ $platform != ,* && $platform != *, && $platform != *,,* ]] || fail 'platform list contains an empty entry'
IFS=',' read -r -a platforms <<< "$platform"
seen_platforms=,
for selected_platform in "${platforms[@]}"; do
    [[ $selected_platform =~ ^linux/[a-z0-9_]+(/[a-zA-Z0-9_.-]+)?$ ]] || fail 'expected Linux platforms separated by commas (for example linux/amd64,linux/arm64)'
    [[ $seen_platforms != *",$selected_platform,"* ]] || fail 'duplicate platform'
    seen_platforms+="$selected_platform,"
done

# Do not run tools or create outputs until all input validation has succeeded.
for tool in docker oras jq shasum; do
    command -v "$tool" >/dev/null || fail "required tool not found: $tool"
done
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$root/kit/oci.sh"
mkdir -p "$root/dist/kit"
output=$(mktemp -d "$root/dist/kit/v3.XXXXXX")
printf 'Build outputs: %s\n' "$output"

layout=$output/kit
build=(docker buildx build --platform "$platform" -f "$root/kit/async-agent.yaml" -t "$reference")
"${build[@]}" --provenance=mode=min --output "type=oci,dest=$layout,tar=false" "$root"
prepare_layout "$layout" kit "$platform"
image_digest=$(layout_digest "$layout" kit)
# The OCI export, not this metadata-losing Docker import, is authoritative.
if [[ ${#platforms[@]} -eq 1 ]]; then
    "${build[@]}" --load --provenance=false "$root"
    printf 'Local Docker image: %s (not an SBX kit reference)\n' "$reference"
else
    printf 'Multi-platform export: %s (not loaded into Docker)\n' "$platform"
fi
printf 'Authoritative OCI layout: %s:%s\n' "$layout" kit

printf 'Built kit: %s\nImmutable kit reference (after publication): %s@%s\n' "$reference" "$repository" "$image_digest"

# Never read a pipe/CI input, or open /dev/tty behind the caller's back.
answer=
if [[ -t 0 ]]; then
    printf 'Push %s? [y/N] ' "$reference"
    IFS= read -r answer || answer=
fi
case "$answer" in
    [yY]|[yY][eE][sS])
        oras cp --from-oci-layout "$layout:kit" "$reference"
        printf 'Published kit: %s@%s\n' "$repository" "$image_digest"
        ;;
    *) printf 'Not pushed. OCI artifacts retained in %s\n' "$output" ;;
esac
