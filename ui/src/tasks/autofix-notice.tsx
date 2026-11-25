import { currentHost, useCallback, useEffect, useMemo, useState } from "../runtime";
import { Alert, AlertDescription, Badge, Button, Checkbox, Label, Switch, Textarea } from "../host-ui";
import { useT, type TFunction } from "../i18n";
import {
  createTaskLinksApi,
  type ApprovePRDraftReplyInput,
  type PRAutoFixState,
  type PRRemediationDraft,
} from "./api";

const AUTO_FIX_POLL_MS = 10_000;

type EditableReplyState = ApprovePRDraftReplyInput & {
  author?: string;
  filePath?: string;
  line?: number;
  threadExcerpt?: string;
};

export function createEditableDraftForm(draft: PRRemediationDraft) {
  const initialCommit = (draft.commitMessage ?? "").trim();
  return {
    draftId: draft.draftId,
    commitPush: draft.hasChanges || initialCommit !== "",
    commitMessage: draft.commitMessage ?? "",
    replies: (draft.replies ?? []).map<EditableReplyState>((reply) => ({
      id: reply.id,
      threadId: reply.threadId,
      send: true,
      content: reply.content,
      resolveThread: Boolean(reply.resolveThread),
      author: reply.author,
      filePath: reply.filePath,
      line: reply.line,
      threadExcerpt: reply.threadExcerpt,
    })),
  };
}

export function statusBadgeLabel(t: TFunction, state: PRAutoFixState): string {
  switch (state.status) {
    case "awaiting_approval":
      return t("autoFixStatusAwaitingApproval");
    case "running":
      return t("autoFixStatusRunning", { round: state.roundCount, max: state.maxRounds });
    case "applying":
      return t("autoFixStatusApplying");
    case "exhausted":
      return t("autoFixStatusExhausted", { max: state.maxRounds });
    case "paused":
      return t("autoFixStatusPaused");
    default:
      return t("autoFixStatusMonitoring");
  }
}

export function AzureDevOpsAutoFixNotice({
  workspaceId,
  taskId,
  repositoryId,
  pullRequestId,
  prOpen,
  onFeedbackRefresh,
  onStateChange,
}: {
  workspaceId: string;
  taskId?: string;
  repositoryId: string;
  pullRequestId: number;
  prOpen: boolean;
  onFeedbackRefresh: () => void;
  onStateChange?: () => void;
}) {
  const t = useT();
  const host = currentHost();
  const api = useMemo(() => createTaskLinksApi(host), [host]);
  const [state, setState] = useState<PRAutoFixState | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [form, setForm] = useState<ReturnType<typeof createEditableDraftForm> | null>(null);
  const [revising, setRevising] = useState(false);
  const [feedbackText, setFeedbackText] = useState("");

  const load = useCallback(
    async (signal?: AbortSignal) => {
      if (!taskId || !workspaceId || !repositoryId || pullRequestId <= 0) return;
      const next = await api.getAutoFix({ workspaceId, taskId }, { repositoryId, pullRequestId }, signal);
      setState(next);
    },
    [api, workspaceId, taskId, repositoryId, pullRequestId],
  );

  useEffect(() => {
    if (!taskId) return undefined;
    const controller = new AbortController();
    void load(controller.signal).catch(() => undefined);
    const timer = setInterval(() => {
      void load(controller.signal).catch(() => undefined);
    }, AUTO_FIX_POLL_MS);
    return () => {
      controller.abort();
      clearInterval(timer);
    };
  }, [taskId, load]);

  useEffect(() => {
    const draft = state?.pendingDraft;
    if (!draft) {
      setForm(null);
      setRevising(false);
      setFeedbackText("");
      return;
    }
    setForm((current) => (current?.draftId === draft.draftId ? current : createEditableDraftForm(draft)));
  }, [state?.pendingDraft]);

  if (!taskId || !state) return null;

  const applyState = (next: PRAutoFixState) => {
    setState(next);
    onStateChange?.();
  };

  const runAction = async (kind: string, operation: () => Promise<PRAutoFixState>, successMessage?: string) => {
    setBusy(kind);
    try {
      const next = await operation();
      applyState(next);
      if (successMessage) host.toast.success(successMessage);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      host.toast.error(t("requestFailed", { error: message }));
    } finally {
      setBusy(null);
    }
  };

  const draft = state.pendingDraft;
  const commitInvalid = !!form && form.commitPush && form.commitMessage.trim() === "";

  return (
    <div className="space-y-2">
      <div className="space-y-2 rounded-md border p-3" data-testid="azure-devops-pr-autofix-controls">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-xs font-medium">{t("autoFixTitle")}</span>
            <Badge variant={state.status === "awaiting_approval" ? "default" : "secondary"}>
              {statusBadgeLabel(t, state)}
            </Badge>
          </div>
          {prOpen ? (
            <Button
              variant="outline"
              size="sm"
              data-testid="azure-devops-autofix-run-now"
              disabled={busy !== null}
              onClick={() =>
                void runAction(
                  "trigger",
                  () => api.triggerAutoFix({ workspaceId, taskId }, { repositoryId, pullRequestId }),
                  t("autoFixTriggered"),
                )
              }
            >
              {busy === "trigger" ? t("autoFixRunningNow") : t("runNow")}
            </Button>
          ) : null}
        </div>
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-3">
            <Label htmlFor="azure-devops-autofix-enabled" className="text-xs">
              {t("autoFixEnableLabel")}
            </Label>
            <Switch
              id="azure-devops-autofix-enabled"
              data-testid="azure-devops-autofix-enabled"
              checked={state.enabled}
              disabled={busy !== null}
              onCheckedChange={(checked) =>
                void runAction("toggle-enabled", () =>
                  api.updateAutoFix({ workspaceId, taskId }, { repositoryId, pullRequestId, enabled: checked }),
                )
              }
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="space-y-1">
              <Label htmlFor="azure-devops-autofix-require-approval" className="text-xs">
                {t("autoFixRequireApprovalLabel")}
              </Label>
              <p className="text-xs text-muted-foreground">{t("autoFixRequireApprovalHint")}</p>
            </div>
            <Switch
              id="azure-devops-autofix-require-approval"
              data-testid="azure-devops-autofix-require-approval"
              checked={state.requireApproval}
              disabled={busy !== null}
              onCheckedChange={(checked) =>
                void runAction("toggle-approval", () =>
                  api.updateAutoFix({ workspaceId, taskId }, { repositoryId, pullRequestId, requireApproval: checked }),
                )
              }
            />
          </div>
        </div>
        {state.lastError ? (
          <p className="text-xs text-destructive" data-testid="azure-devops-autofix-error">
            {state.lastError}
          </p>
        ) : null}
      </div>

      {draft && form ? (
        <Alert data-testid="azure-devops-pr-autofix-draft">
          <AlertDescription className="space-y-3">
            <div className="space-y-1">
              <p className="text-sm font-medium">{t("autoFixDraftTitle")}</p>
              <p className="text-xs text-muted-foreground">{draft.summary}</p>
            </div>

            <div className="space-y-1.5 rounded-md border p-3">
              <div className="flex items-center gap-2">
                <Checkbox
                  id="azure-devops-draft-commit-toggle"
                  data-testid="azure-devops-draft-commit-toggle"
                  checked={form.commitPush}
                  disabled={busy !== null}
                  onCheckedChange={(checked) =>
                    setForm((current) => (current ? { ...current, commitPush: checked } : current))
                  }
                />
                <Label htmlFor="azure-devops-draft-commit-toggle" className="text-xs font-medium">
                  {t("autoFixDraftCommitToggle")}
                </Label>
              </div>
              {form.commitPush ? (
                <Textarea
                  id="azure-devops-draft-commit-message"
                  data-testid="azure-devops-draft-commit-message"
                  rows={3}
                  className="font-mono text-xs"
                  placeholder={t("autoFixDraftCommitPlaceholder")}
                  value={form.commitMessage}
                  disabled={busy !== null}
                  onChange={(event) =>
                    setForm((current) => (current ? { ...current, commitMessage: event.target.value } : current))
                  }
                />
              ) : (
                <p className="text-xs text-muted-foreground">{t("autoFixDraftNoCommit")}</p>
              )}
            </div>

            {form.replies.length > 0 ? (
              <div className="space-y-2">
                <p className="text-xs font-medium">{t("autoFixDraftRepliesTitle")}</p>
                {form.replies.map((reply, index) => {
                  const location = reply.filePath
                    ? `${reply.filePath}${reply.line && reply.line > 0 ? `:${reply.line}` : ""}`
                    : "";
                  return (
                    <div key={reply.id} className="space-y-2 rounded-md border p-3">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <div className="flex items-center gap-2">
                          <Checkbox
                            id={`azure-devops-draft-reply-send-${reply.id}`}
                            checked={reply.send}
                            disabled={busy !== null}
                            onCheckedChange={(checked) =>
                              setForm((current) =>
                                current
                                  ? {
                                      ...current,
                                      replies: current.replies.map((item, idx) =>
                                        idx === index ? { ...item, send: checked } : item,
                                      ),
                                    }
                                  : current,
                              )
                            }
                          />
                          <Label htmlFor={`azure-devops-draft-reply-send-${reply.id}`} className="text-xs font-medium">
                            {reply.threadId > 0
                              ? t("autoFixDraftThreadHeader", { id: reply.threadId })
                              : t("autoFixDraftGeneralComment")}
                          </Label>
                        </div>
                        {location ? <span className="font-mono text-xs text-muted-foreground">{location}</span> : null}
                      </div>
                      {reply.threadExcerpt ? (
                        <p className="text-xs text-muted-foreground">
                          {reply.author ? `${reply.author}: ` : ""}
                          {reply.threadExcerpt}
                        </p>
                      ) : null}
                      {reply.send ? (
                        <>
                          <Textarea
                            rows={3}
                            className="text-xs"
                            data-testid={`azure-devops-draft-reply-content-${reply.threadId}`}
                            value={reply.content}
                            disabled={busy !== null}
                            onChange={(event) =>
                              setForm((current) =>
                                current
                                  ? {
                                      ...current,
                                      replies: current.replies.map((item, idx) =>
                                        idx === index ? { ...item, content: event.target.value } : item,
                                      ),
                                    }
                                  : current,
                              )
                            }
                          />
                          {reply.threadId > 0 ? (
                            <div className="flex items-center gap-2">
                              <Checkbox
                                id={`azure-devops-draft-reply-resolve-${reply.id}`}
                                data-testid={`azure-devops-draft-reply-resolve-${reply.threadId}`}
                                checked={reply.resolveThread}
                                disabled={busy !== null}
                                onCheckedChange={(checked) =>
                                  setForm((current) =>
                                    current
                                      ? {
                                          ...current,
                                          replies: current.replies.map((item, idx) =>
                                            idx === index ? { ...item, resolveThread: checked } : item,
                                          ),
                                        }
                                      : current,
                                  )
                                }
                              />
                              <Label htmlFor={`azure-devops-draft-reply-resolve-${reply.id}`} className="text-xs">
                                {t("autoFixDraftResolveThread")}
                              </Label>
                            </div>
                          ) : null}
                        </>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            ) : null}

            {revising ? (
              <div className="space-y-2 rounded-md border p-3">
                <Label htmlFor="azure-devops-draft-feedback" className="text-xs font-medium">
                  {t("autoFixFeedbackLabel")}
                </Label>
                <Textarea
                  id="azure-devops-draft-feedback"
                  data-testid="azure-devops-draft-feedback"
                  rows={3}
                  className="text-xs"
                  placeholder={t("autoFixFeedbackPlaceholder")}
                  value={feedbackText}
                  disabled={busy !== null}
                  onChange={(event) => setFeedbackText(event.target.value)}
                />
                <div className="flex flex-wrap items-center gap-2">
                  <Button
                    size="sm"
                    data-testid="azure-devops-draft-revise-submit"
                    disabled={busy !== null || feedbackText.trim() === ""}
                    onClick={() =>
                      void runAction(
                        "revise",
                        () =>
                          api.reviseAutoFix(
                            { workspaceId, taskId },
                            {
                              repositoryId,
                              pullRequestId,
                              draftId: form.draftId,
                              feedback: feedbackText.trim(),
                            },
                          ),
                        t("autoFixRevisionSent"),
                      )
                    }
                  >
                    {busy === "revise" ? t("autoFixSendingFeedback") : t("autoFixSendFeedback")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy !== null}
                    onClick={() => setRevising(false)}
                  >
                    {t("cancel")}
                  </Button>
                </div>
              </div>
            ) : null}

            <div className="flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                data-testid="azure-devops-draft-approve"
                disabled={busy !== null || commitInvalid}
                onClick={() =>
                  void runAction(
                    "approve",
                    async () => {
                      const next = await api.approveAutoFix(
                        { workspaceId, taskId },
                        {
                          repositoryId,
                          pullRequestId,
                          draftId: form.draftId,
                          commitPush: form.commitPush,
                          commitMessage: form.commitMessage,
                          replies: form.replies.map(({ id, threadId, send, content, resolveThread }) => ({
                            id,
                            threadId,
                            send,
                            content,
                            resolveThread,
                          })),
                        },
                      );
                      onFeedbackRefresh();
                      return next;
                    },
                    t("autoFixApproved"),
                  )
                }
              >
                {busy === "approve" ? t("autoFixApproving") : t("autoFixApproveApply")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                data-testid="azure-devops-draft-revise-toggle"
                disabled={busy !== null}
                onClick={() => setRevising((current) => !current)}
              >
                {t("autoFixRequestChanges")}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                data-testid="azure-devops-draft-dismiss"
                disabled={busy !== null}
                onClick={() =>
                  void runAction(
                    "dismiss",
                    () =>
                      api.dismissAutoFix(
                        { workspaceId, taskId },
                        { repositoryId, pullRequestId, draftId: form.draftId },
                      ),
                    t("autoFixDismissed"),
                  )
                }
              >
                {t("autoFixDismissDraft")}
              </Button>
            </div>
          </AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}
