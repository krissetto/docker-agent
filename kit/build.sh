#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'kit: %s\n' "$*" >&2; exit 1; }

# CLI_ARGS is data, not a shell command or a list to eval.
[[ $# -ge 1 && $# -le 2 ]] || fail 'usage: build.sh v3|v2 [repository:tag]'
version=$1
[[ $version == v3 || $version == v2 ]] || fail 'expected v3 or v2'
reference=${2:-kagent:local-$version}
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
if [[ $version == v2 ]]; then
    [[ ${#tag} -le 120 ]] || fail 'v2 tag plus -runtime must not exceed 128 characters'
fi
platform=${KIT_PLATFORM:-${DOCKER_DEFAULT_PLATFORM:-linux/amd64}}
[[ $platform != ,* && $platform != *, && $platform != *,,* ]] || fail 'platform list contains an empty entry'
IFS=',' read -r -a platforms <<< "$platform"
seen_platforms=,
for selected_platform in "${platforms[@]}"; do
    [[ $selected_platform =~ ^linux/[a-z0-9_]+(/[a-zA-Z0-9_.-]+)?$ ]] || fail 'expected Linux platforms separated by commas (for example linux/amd64,linux/arm64)'
    [[ $seen_platforms != *",$selected_platform,"* ]] || fail 'duplicate platform'
    seen_platforms+="$selected_platform,"
done
[[ $version != v2 || ${#platforms[@]} -eq 1 ]] || fail 'Kit v2 builds require one platform'

# Do not run tools or create outputs until all input validation has succeeded.
for tool in docker oras jq shasum; do
    command -v "$tool" >/dev/null || fail "required tool not found: $tool"
done
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$root/kit/oci.sh"
mkdir -p "$root/dist/kit"
output=$(mktemp -d "$root/dist/kit/$version.XXXXXX")
printf 'Build outputs: %s\n' "$output"

build=(docker buildx build --platform "$platform")
if [[ $version == v3 ]]; then
    image=$reference
    build+=(-f "$root/kit/async-agent.yaml")
    layout=$output/kit
    selector=kit
else
    image=$repository:$tag-runtime
    build+=(-f "$root/kit/async-agent.dockerfile" --target runtime-v2)
    layout=$output/runtime
    selector=runtime
fi
build+=(-t "$image")
"${build[@]}" --provenance=mode=min --output "type=oci,dest=$layout,tar=false" "$root"
prepare_layout "$layout" "$selector" "$version" "$platform"
image_digest=$(layout_digest "$layout" "$selector")
# The OCI export, not this metadata-losing Docker import, is authoritative.
if [[ ${#platforms[@]} -eq 1 ]]; then
    "${build[@]}" --load --provenance=false "$root"
    printf 'Local Docker image: %s (not an SBX kit reference)\n' "$image"
else
    printf 'Multi-platform export: %s (not loaded into Docker)\n' "$platform"
fi
printf 'Authoritative OCI layout: %s:%s\n' "$layout" "$selector"

if [[ $version == v2 ]]; then
    runtime_layout=$layout
    runtime_digest=$image_digest
    runtime_ref=$repository@$runtime_digest
    # Only sandbox.image changes; preserve the tracked published fixture verbatim.
    awk -v image="$runtime_ref" '
        /^sandbox:$/ { sandbox=1; print; next }
        sandbox && /^  image:/ { print "  image: " image; changed++; next }
        sandbox && /^[^ ]/ { sandbox=0 }
        { print }
        END { if (changed != 1) exit 1 }
    ' "$root/kit/v2/spec.yaml" > "$output/spec.yaml"
    layout=$output/kit
    oras push --oci-layout "$layout:kit" --image-spec v1.1 \
        --artifact-type application/vnd.docker.sandbox.kit.v2 \
        --config "$output/spec.yaml:application/vnd.docker.sandbox.kit.v2.spec+yaml"
    image_digest=$(layout_digest "$layout" kit)
    printf 'Runtime: %s (%s)\nGenerated spec: %s/spec.yaml\n' "$image" "$runtime_ref" "$output"
    printf 'Authoritative kit OCI layout: %s:kit\n' "$layout"
fi
printf 'Built kit: %s\nImmutable kit reference (after publication): %s@%s\n' "$reference" "$repository" "$image_digest"

# Never read a pipe/CI input, or open /dev/tty behind the caller's back.
answer=
if [[ -t 0 ]]; then
    printf 'Push %s%s? [y/N] ' "$reference" "$([[ $version == v2 ]] && printf ' and its runtime' || true)"
    IFS= read -r answer || answer=
fi
case "$answer" in
    [yY]|[yY][eE][sS])
        if [[ $version == v2 ]]; then
            oras cp --from-oci-layout "$runtime_layout:runtime" "$image"
        fi
        oras cp --from-oci-layout "$layout:kit" "$reference"
        printf 'Published kit: %s@%s\n' "$repository" "$image_digest"
        ;;
    *) printf 'Not pushed. OCI artifacts retained in %s\n' "$output" ;;
esac
