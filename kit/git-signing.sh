#!/bin/sh
# Turn on SSH commit signing when the host forwards an ssh-agent with a key.
# Idempotent and non-fatal: without an agent or key, git config is left as is
# and commits stay unsigned.
set -u
[ -n "${SSH_AUTH_SOCK:-}" ] || exit 0
command -v ssh-add >/dev/null 2>&1 || exit 0
key=$(ssh-add -L 2>/dev/null | head -n 1 | cut -d' ' -f1,2) || exit 0
[ -n "$key" ] || exit 0
email=$(git config --global user.email 2>/dev/null || true)
signers="${XDG_CONFIG_HOME:-$HOME/.config}/git/allowed_signers"
mkdir -p "$(dirname "$signers")"
printf '%s namespaces="git" %s\n' "${email:-agent}" "$key" > "$signers"
git config --global gpg.format ssh
git config --global user.signingkey "key::$key"
git config --global gpg.ssh.allowedSignersFile "$signers"
git config --global commit.gpgsign true
git config --global tag.gpgsign true
