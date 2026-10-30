// @fuse/sdk harness surface (change 0084, ADR-0060): the generated fuse.harness.v1
// types - HarnessSpec and its messages, the HarnessService descriptor, and the UI-hint
// field options for clients that render a spec as a form - plus the shape validator.
//
// validateHarnessShape runs the protovalidate rules declared in harness.proto, the SAME
// rules the Go validator (github.com/ethanhinson/fuse/harness) runs, from the same
// descriptors, so a TS client rejects the same malformed input with the same field paths
// and rule ids. The Go validator additionally runs fuse's semantic config rules (model
// alias resolution, cross-field ordering); call HarnessService.ValidateHarness on a server
// that mounts it for the full verdict.

import { createValidator, type Validator } from "@bufbuild/protovalidate";
import { pathToString } from "@bufbuild/protobuf/reflect";

import {
  HarnessSpecSchema,
  type HarnessSpec,
} from "../../../proto/gen/ts/fuse/harness/v1/harness_pb.js";

export * from "../../../proto/gen/ts/fuse/harness/v1/harness_pb.js";
export * from "../../../proto/gen/ts/fuse/harness/v1/options_pb.js";

/** One shape violation, in the same form the Go validator reports. */
export interface HarnessViolation {
  /** protovalidate field path, e.g. `mcp_servers[0].url`. */
  fieldPath: string;
  /** Human-readable description relative to the field. */
  message: string;
  /** Rule identifier, e.g. `string.uri`. */
  ruleId: string;
}

let validator: Validator | undefined;

/**
 * Validate a HarnessSpec against the protovalidate shape rules. Returns every
 * violation, sorted by field path then rule id (the Go validator's order); an
 * empty array means the spec's shape is valid. Throws if a rule fails to
 * compile or evaluate (a validator fault, not a spec problem).
 */
export function validateHarnessShape(spec: HarnessSpec): HarnessViolation[] {
  validator ??= createValidator();
  const result = validator.validate(HarnessSpecSchema, spec);
  if (result.kind === "error") {
    throw result.error;
  }
  if (result.kind === "valid") {
    return [];
  }
  const out = result.violations.map((v) => ({
    fieldPath: pathToString(v.field),
    message: v.message,
    ruleId: v.ruleId,
  }));
  out.sort((a, b) =>
    a.fieldPath !== b.fieldPath
      ? a.fieldPath < b.fieldPath ? -1 : 1
      : a.ruleId < b.ruleId ? -1 : a.ruleId > b.ruleId ? 1 : 0,
  );
  return out;
}
