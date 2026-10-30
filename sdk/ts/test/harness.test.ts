// Node test (node:test) for the @fuse/sdk harness surface (change 0084).
//
// validateHarnessShape must agree with the Go validator's shape layer
// (harness.ValidateShape) on every case in harness/testdata/shape_cases.json: same
// field paths, same rule ids, same order. The Go side runs the same file in
// harness/shape_parity_test.go, so a rule or path-format divergence between the two
// languages fails one of the two suites. It also checks the UI-hint options are
// readable from the generated descriptors, which is how a form renderer consumes them.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { create, fromJson, getOption, hasOption, type JsonValue } from "@bufbuild/protobuf";

import { harness } from "../src/index.js";

interface ShapeCase {
  name: string;
  spec: JsonValue;
  want: { fieldPath: string; ruleId: string }[];
}

const casesPath = fileURLToPath(new URL("../../../harness/testdata/shape_cases.json", import.meta.url));
const cases = JSON.parse(readFileSync(casesPath, "utf8")) as ShapeCase[];

for (const tc of cases) {
  test(`validateHarnessShape: ${tc.name}`, () => {
    const spec = fromJson(harness.HarnessSpecSchema, tc.spec);
    const got = harness.validateHarnessShape(spec).map((v) => ({ fieldPath: v.fieldPath, ruleId: v.ruleId }));
    assert.deepEqual(got, tc.want);
  });
}

test("validateHarnessShape reports a message for each violation", () => {
  const spec = create(harness.HarnessSpecSchema, {
    context: { summarization: { threshold: 2 } },
  });
  const vs = harness.validateHarnessShape(spec);
  assert.equal(vs.length, 1);
  assert.equal(vs[0].fieldPath, "context.summarization.threshold");
  assert.ok(vs[0].message.length > 0);
});

test("UI-hint options are readable from the generated descriptors", () => {
  const auth = harness.McpAuthSchema.fields.find((f) => f.name === "client_secret_ref");
  assert.ok(auth && hasOption(auth, harness.ui));
  const ui = getOption(auth, harness.ui);
  assert.equal(ui.secretRef, true);
  assert.equal(ui.widget, harness.Widget.SECRET_REF);
  assert.equal(ui.label, "Client secret");

  const total = harness.PoolSchema.fields.find((f) => f.name === "total");
  assert.ok(total);
  assert.equal(getOption(total, harness.ui).tightenOnly, true);

  const auto = harness.PermissionModeSchema.values.find((v) => v.number === harness.PermissionMode.AUTO);
  assert.ok(auto);
  assert.equal(getOption(auto, harness.value_ui).displayName, "Auto");
});
