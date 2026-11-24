import type { AzureDevOpsFiltersState } from "./filters";
import type { AzureDevOpsSavedView } from "./types";
import { normalizeWorkItemStateFilter } from "./work-item-state";

/** The team, sprint, and state filters a saved work-item view records. */
export function workItemViewScope(
  filters: AzureDevOpsFiltersState,
): Pick<AzureDevOpsSavedView, "teamId" | "iterationId" | "status"> {
  return {
    ...(filters.teamId ? { teamId: filters.teamId } : {}),
    ...(filters.iterationId ? { iterationId: filters.iterationId } : {}),
    ...(filters.workItemState ? { status: filters.workItemState } : {}),
  };
}

/** The filters a saved view runs with; its team and sprint replace the current ones. */
export function filtersFromSavedView(
  current: AzureDevOpsFiltersState,
  view: AzureDevOpsSavedView,
): AzureDevOpsFiltersState {
  const isWorkItem = view.kind === "work_item";
  return {
    ...current,
    projectId: view.projectId,
    repositoryId: view.repositoryId ?? "",
    wiql: view.wiql ?? current.wiql,
    text: "",
    top: view.top ?? 50,
    status: isWorkItem ? current.status : (view.status ?? "active"),
    workItemState: isWorkItem ? normalizeWorkItemStateFilter(view.status) : "",
    creator: view.creator ?? "",
    reviewer: view.reviewer ?? "",
    teamId: view.teamId ?? "",
    iterationId: view.iterationId ?? "",
  };
}
