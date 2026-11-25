import { describe, expect, it } from "vite-plus/test";

import type { SessionGroupInput } from "./sessionGroups.js";
import { sameField, sameSessionFields, sidebarIndexRowToSession } from "./sessionRows.js";

const baseRow: SessionGroupInput = {
  id: "sess-1",
  project: "demo",
  machine: "local",
  source_machines: ["local", "remote"],
  agent: "claude",
  started_at: "2026-01-01T00:00:00Z",
  ended_at: null,
  created_at: "2026-01-01T00:00:00Z",
  message_count: 10,
  transcript_revision: "rev-1",
  is_teammate: false,
};

describe("sameSessionFields", () => {
  it("treats two rows mapped from equal index rows as unchanged", () => {
    const a = sidebarIndexRowToSession(baseRow);
    const b = sidebarIndexRowToSession({ ...baseRow });
    expect(sameSessionFields(a, b)).toBe(true);
  });

  it("detects a scalar change", () => {
    const a = sidebarIndexRowToSession(baseRow);
    const b = sidebarIndexRowToSession({ ...baseRow, message_count: 11 });
    expect(sameSessionFields(a, b)).toBe(false);
  });

  it("detects array field changes even at the same length", () => {
    const a = sidebarIndexRowToSession(baseRow);
    const changed = sidebarIndexRowToSession({
      ...baseRow,
      source_machines: ["local", "other"],
    });
    expect(sameSessionFields(a, changed)).toBe(false);
    const grown = sidebarIndexRowToSession({
      ...baseRow,
      source_machines: ["local", "remote", "extra"],
    });
    expect(sameSessionFields(a, grown)).toBe(false);
  });

  it("detects keys that exist only on the other session", () => {
    const a = sidebarIndexRowToSession(baseRow);
    const b = { ...a, extra_field: "new" } as typeof a;
    expect(sameSessionFields(a, b)).toBe(false);
  });
});

describe("sameField", () => {
  it("compares array contents element-wise", () => {
    expect(sameField(["a", "b"], ["a", "b"])).toBe(true);
    expect(sameField(["a", "b"], ["a", "c"])).toBe(false);
    expect(sameField(["a"], ["a", "b"])).toBe(false);
    expect(sameField([], [])).toBe(true);
  });

  it("does not equate an array with a non-array", () => {
    expect(sameField(["a"], "a")).toBe(false);
    expect(sameField("a", ["a"])).toBe(false);
    expect(sameField(undefined, [])).toBe(false);
  });

  it("skips comparison at sparse holes in the first array", () => {
    // The comparator only checks indexes present in a, so a hole in a
    // matches anything in b — including a defined value. Pin the contract.
    // eslint-disable-next-line no-sparse-arrays
    expect(sameField([, "b"], [undefined, "b"])).toBe(true);
    // eslint-disable-next-line no-sparse-arrays
    expect(sameField([, "b"], ["x", "b"])).toBe(true);
    // The reverse is not symmetric: b's hole is read as undefined.
    // eslint-disable-next-line no-sparse-arrays
    expect(sameField(["x", "b"], [, "b"])).toBe(false);
  });
});
