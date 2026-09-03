#!/usr/bin/env bash
set -euo pipefail

packages=("$@")
if [ "${#packages[@]}" -eq 0 ]; then
  packages=(
    ./internal/server
    ./internal/server/apitest
    ./internal/server/docstest
    ./internal/server/e2etest
    ./internal/server/workspacetest
    ./internal/github
    ./internal/db
    ./internal/workspace
    ./internal/workspace/gitstatustest
    ./internal/ratelimit
  )
fi

mkdir -p tmp
go test -race -shuffle=on -json "${packages[@]}" \
  | tee tmp/race-package-times.json \
  | jq -r '
      select(.Action == "pass" and .Package and (.Test | not))
      | "\(.Elapsed)s\t\(.Package)"
    '
