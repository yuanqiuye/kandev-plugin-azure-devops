import { IconCheck } from "../icons";
import { Badge } from "../host-ui";

export type WorkItemStateGroup = "todo" | "in_progress" | "done" | "removed";

export type WorkItemStateFilter = "" | "open" | "todo" | "in_progress" | "done";

const VALID_FILTERS = new Set<string>(["open", "todo", "in_progress", "done"]);

export function normalizeWorkItemStateFilter(value: string | undefined): WorkItemStateFilter {
  const normalized = (value ?? "").trim().toLowerCase();
  return VALID_FILTERS.has(normalized) ? (normalized as WorkItemStateFilter) : "";
}

/**
 * Classifies a work item's workflow state into To Do (Proposed), In progress,
 * Done (Completed), or Removed using Azure DevOps' stateCategory metadata when
 * present and standard state names as a fallback.
 */
export function workItemStateGroup(item: { state?: string; stateCategory?: string }): WorkItemStateGroup {
  switch (item.stateCategory?.trim().toLowerCase()) {
    case "completed":
      return "done";
    case "proposed":
      return "todo";
    case "removed":
      return "removed";
    case "inprogress":
    case "resolved":
      return "in_progress";
  }
  switch (item.state?.trim().toLowerCase()) {
    case "done":
    case "closed":
    case "completed":
    case "inactive":
      return "done";
    case "removed":
    case "cut":
      return "removed";
    case "to do":
    case "new":
    case "proposed":
    case "requested":
    case "design":
      return "todo";
    default:
      return "in_progress";
  }
}

export function partitionWorkItemsByDone<T extends { state?: string; stateCategory?: string }>(items: T[]) {
  const open: T[] = [];
  const done: T[] = [];
  for (const item of items) {
    if (workItemStateGroup(item) === "done") done.push(item);
    else open.push(item);
  }
  return { open, done, hasBoth: open.length > 0 && done.length > 0 };
}

export function WorkItemStateBadge({ state, stateCategory }: { state?: string; stateCategory?: string }) {
  if (!state) return null;
  const group = workItemStateGroup({ state, stateCategory });
  if (group === "done") {
    return (
      <Badge variant="secondary" className="gap-1 text-muted-foreground" data-state-group="done">
        <IconCheck className="h-3 w-3" />
        <span>{state}</span>
      </Badge>
    );
  }
  if (group === "in_progress") {
    return (
      <Badge variant="default" data-state-group="in_progress">
        {state}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" data-state-group={group}>
      {state}
    </Badge>
  );
}
