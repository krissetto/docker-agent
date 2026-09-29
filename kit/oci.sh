#!/usr/bin/env bash
# Local OCI layout operations; never resolve a registry reference.

layout_digest() {
    local digest
    digest=$(jq -er --arg tag "$2" '
        [.manifests[] | select(.annotations["org.opencontainers.image.ref.name"] == $tag)]
        | if length == 1 then .[0].digest else error("expected one tagged OCI root") end
    ' "$1/index.json") || fail 'invalid OCI layout selector'
    [[ $digest =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'expected an OCI sha256 digest'
    printf '%s\n' "$digest"
}

prepare_layout() {
    local layout=$1 selector=$2 version=$3 platform=$4
    local digest blob child annotations updated size architecture variant
    digest=$(jq -er 'if (.manifests | length) == 1 then .manifests[0].digest else error("expected one exported OCI root") end' "$layout/index.json") || fail 'invalid exported OCI layout'
    [[ $digest =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'expected an OCI sha256 digest'
    blob=$layout/blobs/sha256/${digest#sha256:}
    [[ -f $blob ]] || fail 'exported OCI root blob is missing'
    if [[ $version == v3 ]]; then
        child=$digest
        if jq -e '.manifests != null' "$blob" >/dev/null; then
            architecture=${platform#linux/}
            variant=
            if [[ $architecture == */* ]]; then
                variant=${architecture#*/}
                architecture=${architecture%%/*}
            fi
            child=$(jq -er --arg arch "$architecture" --arg variant "$variant" '
                [.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)
                 | select($variant == "" or .platform.variant == $variant)]
                | if length == 1 then .[0].digest else error("expected one selected platform manifest") end
            ' "$blob") || fail 'cannot uniquely select the kit platform manifest'
        fi
        [[ $child =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'invalid platform manifest digest'
        annotations=$(jq -ce '
            .annotations | with_entries(select(.key == "vnd.docker.sandbox.kit.descriptor"
                or .key == "vnd.docker.sandbox.kit.schema-version"
                or .key == "vnd.docker.sandbox.kit.capabilities"))
            | if length == 3 and all(.[]; type == "string" and length > 0)
              then . else error("missing required kit annotations") end
        ' "$layout/blobs/sha256/${child#sha256:}") || fail 'platform manifest lacks required kit annotations'
        # Avoid rewriting an already annotated root; child blobs/provenance never change.
        if ! jq -e --argjson annotations "$annotations" '.annotations as $current | $annotations | to_entries | all(.[]; $current[.key] == .value)' "$blob" >/dev/null; then
            updated=$layout/promoted-index.json
            jq --argjson annotations "$annotations" '.annotations = ((.annotations // {}) + $annotations)' "$blob" > "$updated"
            digest=sha256:$(shasum -a 256 "$updated" | awk '{print $1}')
            blob=$layout/blobs/sha256/${digest#sha256:}
            mv "$updated" "$blob"
        fi
    fi
    size=$(wc -c < "$blob" | tr -d '[:space:]')
    jq --arg digest "$digest" --argjson size "$size" --arg tag "$selector" '
        .manifests[0].digest = $digest | .manifests[0].size = $size
        | .manifests[0].annotations["org.opencontainers.image.ref.name"] = $tag
    ' "$layout/index.json" > "$layout/index.json.tmp"
    mv "$layout/index.json.tmp" "$layout/index.json"
}
