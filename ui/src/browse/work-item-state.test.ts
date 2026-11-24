import { describe, expect, it } from "vitest";
import {
  normalizeWorkItemStateFilter,
  partitionWorkItemsByDone,
  workItemStateGroup,
} from "./work-item-state";

describe("work item state classification and grouping", () => {
  it("classifies custom enterprise states from stateCategory and standard names as fallback", () => {
    expect(workItemStateGroup({ state: "To Do", stateCategory: "Proposed" })).toBe("todo");
    expect(workItemStateGroup({ state: "3.3 Mitigate - Done", stateCategory: "Completed" })).toBe("done");
    expect(workItemStateGroup({ state: "DC - New", stateCategory: "Proposed" })).toBe("todo");
    expect(workItemStateGroup({ state: "In Review", stateCategory: "InProgress" })).toBe("in_progress");
    expect(workItemStateGroup({ state: "Done" })).toBe("done");
    expect(workItemStateGroup({ state: "To Do" })).toBe("todo");
    expect(workItemStateGroup({ state: "Active" })).toBe("in_progress");
  });

  it("partitions mixed open and done work items while preserving relative order", () => {
    const items = [
      { id: 1, state: "Done", stateCategory: "Completed" },
      { id: 2, state: "To Do", stateCategory: "Proposed" },
      { id: 3, state: "3.3 Mitigate - Done", stateCategory: "Completed" },
      { id: 4, state: "Active", stateCategory: "InProgress" },
    ];
    const partitioned = partitionWorkItemsByDone(items);
    expect(partitioned.hasBoth).toBe(true);
    expect(partitioned.open.map((i) => i.id)).toEqual([2, 4]);
    expect(partitioned.done.map((i) => i.id)).toEqual([1, 3]);

    expect(partitionWorkItemsByDone(partitioned.open).hasBoth).toBe(false);
    expect(partitionWorkItemsByDone(partitioned.done).hasBoth).toBe(false);
  });

  it("accepts only supported state filter values", () => {
    expect(normalizeWorkItemStateFilter("todo")).toBe("todo");
    expect(normalizeWorkItemStateFilter("DONE")).toBe("done");
    expect(normalizeWorkItemStateFilter("in_progress")).toBe("in_progress");
    expect(normalizeWorkItemStateFilter("open")).toBe("open");
    expect(normalizeWorkItemStateFilter("active")).toBe("");
    expect(normalizeWorkItemStateFilter(undefined)).toBe("");
  });
});
