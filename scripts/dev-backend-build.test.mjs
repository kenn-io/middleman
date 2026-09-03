import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

test("regenerates the Orval client before restoring schema constraints", async () => {
  const script = await readFile(new URL("./dev-backend-build.sh", import.meta.url), "utf8");
  const writeBackendSpec = script.indexOf('write_if_changed "$backend_spec" "$tmp_backend_spec"');
  const generateClient = script.indexOf('"$NODE_BIN" "$frontend_client_generator" openapi/openapi.yaml');
  const generateConstraints = script.indexOf('"$NODE_BIN" "$constraints_generator" "$backend_spec"');
  assert.notEqual(writeBackendSpec, -1);
  assert.notEqual(generateClient, -1);
  assert.notEqual(generateConstraints, -1);
  assert.ok(writeBackendSpec < generateClient);
  assert.ok(generateClient < generateConstraints);
  assert.ok(script.includes('"$constraints_generator"') && script.includes("compute_inputs_hash"));
});

test("writes generated backend OpenAPI input before Go client generation", async () => {
  const script = await readFile("scripts/dev-backend-build.sh", "utf8");

  const createBackendSpecDir = script.indexOf('mkdir -p "$(dirname "$backend_spec")"');
  const writeBackendSpec = script.indexOf('write_if_changed "$backend_spec" "$tmp_backend_spec"');
  const generateGoClient = script.indexOf('"$GO_BIN" generate ./internal/apiclient/generated');

  assert.notEqual(createBackendSpecDir, -1);
  assert.notEqual(writeBackendSpec, -1);
  assert.notEqual(generateGoClient, -1);
  assert.ok(createBackendSpecDir < writeBackendSpec);
  assert.ok(writeBackendSpec < generateGoClient);
});
