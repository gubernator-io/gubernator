#!/usr/bin/env bash
# Runs Tiger over every package in the module and exits with Tiger's result:
# 0 clean, 1 findings, 2 Tiger could not check.
#
# Tiger loads packages with the `go` command on PATH, so a newer local Go than
# the one go.mod names fails with "export data version N is greater than
# maximum supported version". Pin the toolchain to go.mod's `go` line.
set -u
cd "$(dirname "$0")/.." || exit 2
GOTOOLCHAIN="go$(sed -n 's/^go //p' go.mod)"
export GOTOOLCHAIN
exec tiger check ./...
