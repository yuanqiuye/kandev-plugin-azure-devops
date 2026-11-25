import { useCallback, useEffect, useMemo, useState } from "../runtime";
import { ChangeRequestDetail } from "../host-ui";
import { currentHost } from "../runtime";
import { useT } from "../i18n";
import { createBrowseApi } from "../browse/api";
import type { AzureDevOpsPullRequestFeedback } from "../browse/types";
import { createTaskLinksApi } from "./api";
import { AzureDevOpsAutoFixNotice } from "./autofix-notice";
import { pullRequestState, toChangeRequestDetail } from "./review-model";

export type ReviewPanelProps = {
  presentation: "desktop" | "mobile";
  workspaceId: string;
  taskId?: string;
  connectionScope: string;
  repositoryId: string;
  changeRequestNumber: string | number;
  onAutoFixStateChange?: () => void;
};

type PanelState = { feedback: AzureDevOpsPullRequestFeedback | null; loading: boolean; error: string | null };

/** Linked pull request detail on the host's native review surface. */
export function AzureDevOpsReviewPanel(props: ReviewPanelProps) {
  const t = useT();
  const host = currentHost();
  const taskApi = useMemo(() => createTaskLinksApi(host), [host]);
  const [state, setState] = useState<PanelState>({ feedback: null, loading: true, error: null });
  const [busyActionId, setBusyActionId] = useState<string | null>(null);
  const [generation, setGeneration] = useState(0);
  const number = Number(props.changeRequestNumber);
  const refresh = useCallback(() => setGeneration((value) => value + 1), []);

  useEffect(() => {
    const controller = new AbortController();
    setState((previous) => ({ ...previous, loading: previous.feedback === null, error: null }));
    createBrowseApi(host, props.workspaceId)
      .feedback(props.connectionScope, props.repositoryId, number, controller.signal)
      .then((feedback) => setState({ feedback, loading: false, error: null }))
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        const message = error instanceof Error ? error.message : String(error);
        setState((previous) => ({ ...previous, loading: false, error: t("requestFailed", { error: message }) }));
      });
    return () => controller.abort();
  }, [host, props.workspaceId, props.connectionScope, props.repositoryId, number, generation]);

  const prState = state.feedback
    ? pullRequestState(state.feedback.pullRequest.status, state.feedback.pullRequest.isDraft)
    : "open";
  const prOpen = prState === "open" || prState === "draft";

  const actions = useMemo(() => {
    if (!props.taskId || !prOpen) return undefined;
    return [
      { id: "reply-thread", label: t("replyAction"), pendingLabel: t("replyingAction"), placement: "thread", input: "text" },
      { id: "add-comment", label: t("commentAction"), pendingLabel: t("commentingAction"), placement: "comment", input: "text" },
    ];
  }, [props.taskId, prOpen, t]);

  const onAction = useCallback(
    async (request: { actionId: string; body?: string; threadId?: string }) => {
      if (!props.taskId || !request.body?.trim()) return;
      setBusyActionId(request.actionId);
      try {
        await taskApi.replyPullRequest(
          { workspaceId: props.workspaceId, taskId: props.taskId },
          {
            repositoryId: props.repositoryId,
            pullRequestId: number,
            ...(request.threadId ? { threadRef: request.threadId } : {}),
            content: request.body.trim(),
          },
        );
        host.toast.success(t("replySent"));
        refresh();
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        host.toast.error(t("requestFailed", { error: message }));
      } finally {
        setBusyActionId(null);
      }
    },
    [host, number, props.repositoryId, props.taskId, props.workspaceId, refresh, t, taskApi],
  );

  return (
    <ChangeRequestDetail
      detail={state.feedback ? toChangeRequestDetail(state.feedback, t("fallbackAuthor")) : null}
      presentation={props.presentation}
      loading={state.loading}
      error={state.error}
      onRefresh={refresh}
      onRetry={refresh}
      actions={actions}
      busyActionId={busyActionId}
      onAction={onAction}
      notice={
        props.taskId ? (
          <AzureDevOpsAutoFixNotice
            workspaceId={props.workspaceId}
            taskId={props.taskId}
            repositoryId={props.repositoryId}
            pullRequestId={number}
            prOpen={prOpen}
            onFeedbackRefresh={refresh}
            onStateChange={props.onAutoFixStateChange}
          />
        ) : undefined
      }
    />
  );
}
