#!/bin/sh
# Installed as /usr/local/bin/gh, ahead of the real binary on PATH.
#
# Hermes strips GH_TOKEN from every command its terminal tool runs, and
# refuses to allowlist it. The harness adapter therefore writes the token to
# a 0600 file outside the snapshotted /data tree and removes it from the
# environment; this wrapper hands it to the real gh for one invocation.
# git uses it too, through the system credential helper
# (`gh auth git-credential`, configured in the Dockerfile).
set -eu
token_file=/tmp/causely-secrets/gh-token
if [ -z "${GH_TOKEN:-}" ] && [ -r "$token_file" ]; then
	GH_TOKEN=$(cat "$token_file")
	export GH_TOKEN
fi
exec /opt/gh/bin/gh "$@"
