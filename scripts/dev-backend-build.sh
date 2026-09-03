#!/bin/sh

set -eu

state_dir="tmp/air"
input_hash_file="$state_dir/openapi-inputs.sha256"
frontend_spec="frontend/openapi/openapi.yaml"
backend_spec="internal/apiclient/spec/openapi.json"
frontend_constraints="frontend/src/lib/api/generated/schema-constraints.ts"
frontend_client_generator="frontend/scripts/generate-api-client.mjs"
constraints_generator="scripts/generate-schema-constraints.mjs"

mkdir -p "$state_dir"

resolve_go_bin() {
  if command -v go >/dev/null 2>&1; then
    command -v go
    return
  fi

  if [ -x /usr/local/go/bin/go ]; then
    printf '%s\n' /usr/local/go/bin/go
    return
  fi

  printf '%s\n' "go toolchain not found" >&2
  exit 127
}

resolve_node_bin() {
  if command -v node >/dev/null 2>&1; then
    command -v node
    return
  fi

  printf '%s\n' "node runtime not found" >&2
  exit 127
}

GO_BIN="$(resolve_go_bin)"
NODE_BIN="$(resolve_node_bin)"
exe_suffix=""

if [ "$("$GO_BIN" env GOOS)" = "windows" ]; then
  exe_suffix=".exe"
fi

compute_inputs_hash() {
  {
    printf '%s\n' "go.mod" "go.sum" "frontend/package.json" "$frontend_client_generator" "$constraints_generator"
    find cmd/kenn-forge-openapi internal/server -type f -name '*.go' | sort
  } | while IFS= read -r path; do
    [ -f "$path" ] || continue
    shasum -a 256 "$path"
  done | shasum -a 256 | awk '{print $1}'
}

write_if_changed() {
  destination="$1"
  source="$2"

  if [ -f "$destination" ] && cmp -s "$destination" "$source"; then
    rm -f "$source"
    return 1
  fi

  mv "$source" "$destination"
  return 0
}

generate_api_artifacts() {
  tmp_frontend_spec="$(mktemp "$state_dir/frontend-openapi.XXXXXX")"
  tmp_backend_spec="$(mktemp "$state_dir/backend-openapi.XXXXXX")"

  mkdir -p "$(dirname "$backend_spec")"

  GOCACHE="${GOCACHE:-/tmp/kenn-forge-gocache}" "$GO_BIN" run ./cmd/kenn-forge-openapi -out "$tmp_frontend_spec" -format yaml
  GOCACHE="${GOCACHE:-/tmp/kenn-forge-gocache}" "$GO_BIN" run ./cmd/kenn-forge-openapi -out "$tmp_backend_spec" -version 3.0

  write_if_changed "$frontend_spec" "$tmp_frontend_spec" >/dev/null 2>&1 || true

  write_if_changed "$backend_spec" "$tmp_backend_spec" >/dev/null 2>&1 || true

  "$NODE_BIN" "$frontend_client_generator" openapi/openapi.yaml

  # Numeric bounds come from the backend JSON spec. Generate them after Orval,
  # which replaces the generated client directory atomically.
  tmp_constraints="$(mktemp "$state_dir/frontend-schema-constraints.XXXXXX")"
  "$NODE_BIN" "$constraints_generator" "$backend_spec" "$tmp_constraints"
  write_if_changed "$frontend_constraints" "$tmp_constraints" >/dev/null 2>&1 || true

  GOCACHE="${GOCACHE:-/tmp/kenn-forge-gocache}" "$GO_BIN" generate ./internal/apiclient/generated
}

current_inputs_hash="$(compute_inputs_hash)"
previous_inputs_hash=""

if [ -f "$input_hash_file" ]; then
  previous_inputs_hash="$(cat "$input_hash_file")"
fi

if [ "$current_inputs_hash" != "$previous_inputs_hash" ]; then
  generate_api_artifacts
  printf '%s\n' "$current_inputs_hash" > "$input_hash_file"
fi

"$GO_BIN" build -o "./tmp/kenn-forge$exe_suffix" ./cmd/kenn-forge
