import type { PluginHostApi, ReviewTaskStatus } from "@kdlbs/plugin-sdk";

export type PullRequestLink = {
  projectId: string;
  projectName?: string;
  repositoryId: string;
  repositoryName?: string;
  pullRequestId: number;
  title?: string;
  url?: string;
  sourceBranch?: string;
  targetBranch?: string;
  linkedAt?: string;
};

export type PullRequestLinkStatus = PullRequestLink & {
  status?: string;
  isDraft?: boolean;
  autoFixStatus?: string;
  taskStatus?: ReviewTaskStatus;
  error?: string;
};

export type PRDraftReply = {
  id: string;
  threadId: number;
  author?: string;
  filePath?: string;
  line?: number;
  threadExcerpt?: string;
  content: string;
  resolveThread: boolean;
};

export type PRRemediationDraft = {
  draftId: string;
  summary: string;
  hasChanges: boolean;
  commitMessage?: string;
  replies: PRDraftReply[];
  createdAt?: string;
};

export type PRAutoFixState = {
  repositoryId: string;
  pullRequestId: number;
  enabled: boolean;
  requireApproval: boolean;
  status: "idle" | "running" | "awaiting_approval" | "applying" | "exhausted" | "paused" | string;
  roundCount: number;
  maxRounds: number;
  lastDispatchedAt?: string;
  lastError?: string;
  lastErrorAt?: string;
  pendingDraft?: PRRemediationDraft;
};

export type ApprovePRDraftReplyInput = {
  id: string;
  threadId: number;
  send: boolean;
  content: string;
  resolveThread: boolean;
};

export type WorkItemLink = {
  projectId: string;
  workItemId: number;
  title?: string;
  url?: string;
  state?: string;
  type?: string;
  linkedAt?: string;
};

export type TaskLinks = { pullRequests: PullRequestLinkStatus[]; workItems: WorkItemLink[] };

export type WorkspaceLinks = {
  pullRequests: Record<string, PullRequestLink[]>;
  workItems: Record<string, WorkItemLink[]>;
  /** Titles of the linked tasks the plugin could read. */
  tasks: Record<string, { title: string }>;
};

export type CreatePullRequestResult = {
  url?: string;
  number?: number;
  linked?: boolean;
  associationError?: string;
};

type TaskSelector = { workspaceId?: string; taskId: string };

/** Typed wrappers over the plugin's task link actions. */
export function createTaskLinksApi(host: PluginHostApi) {
  const onTask = <T>(key: string, selector: TaskSelector, body: unknown = {}, signal?: AbortSignal) =>
    host.api.invokeAction<T>(
      key,
      { ...(selector.workspaceId ? { workspaceId: selector.workspaceId } : {}), taskId: selector.taskId, body },
      { signal },
    );
  return {
    async get(selector: TaskSelector, signal?: AbortSignal): Promise<TaskLinks> {
      const response = await onTask<Partial<TaskLinks>>("task_links.get", selector, {}, signal);
      return { pullRequests: response.pullRequests ?? [], workItems: response.workItems ?? [] };
    },
    async workspace(workspaceId: string, signal?: AbortSignal): Promise<WorkspaceLinks> {
      const response = await host.api.invokeAction<Partial<WorkspaceLinks>>(
        "task_links.workspace",
        { workspaceId, body: {} },
        { signal },
      );
      return { pullRequests: response.pullRequests ?? {}, workItems: response.workItems ?? {}, tasks: response.tasks ?? {} };
    },
    linkPullRequest: (selector: TaskSelector, body: Record<string, unknown>, signal?: AbortSignal) =>
      onTask<{ pullRequest?: PullRequestLink }>("pull_requests.link", selector, body, signal),
    unlinkPullRequest: (selector: TaskSelector, repositoryId: string, pullRequestId: number, signal?: AbortSignal) =>
      onTask("pull_requests.unlink", selector, { repositoryId, pullRequestId }, signal),
    linkWorkItem: (selector: TaskSelector, body: Record<string, unknown>, signal?: AbortSignal) =>
      onTask<{ workItem?: WorkItemLink }>("work_items.link", selector, body, signal),
    unlinkWorkItem: (selector: TaskSelector, workItemId: number, signal?: AbortSignal) =>
      onTask("work_items.unlink", selector, { workItemId }, signal),
    createPullRequest: (
      selector: TaskSelector & { sessionId: string; repositoryId: string },
      body: { title: string; description: string; baseBranch?: string; draft: boolean },
      signal?: AbortSignal,
    ) =>
      host.api.invokeAction<CreatePullRequestResult>(
        "pull_requests.create",
        {
          workspaceId: selector.workspaceId,
          taskId: selector.taskId,
          sessionId: selector.sessionId,
          repositoryId: selector.repositoryId,
          body,
        },
        { signal },
      ),
    getAutoFix: async (
      selector: TaskSelector,
      pr: { repositoryId: string; pullRequestId: number },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.get", selector, pr, signal);
      return response.state;
    },
    updateAutoFix: async (
      selector: TaskSelector,
      body: { repositoryId: string; pullRequestId: number; enabled?: boolean; requireApproval?: boolean },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.update", selector, body, signal);
      return response.state;
    },
    triggerAutoFix: async (
      selector: TaskSelector,
      pr: { repositoryId: string; pullRequestId: number },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.trigger", selector, pr, signal);
      return response.state;
    },
    approveAutoFix: async (
      selector: TaskSelector,
      body: {
        repositoryId: string;
        pullRequestId: number;
        draftId: string;
        commitPush: boolean;
        commitMessage: string;
        replies: ApprovePRDraftReplyInput[];
      },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.approve", selector, body, signal);
      return response.state;
    },
    reviseAutoFix: async (
      selector: TaskSelector,
      body: { repositoryId: string; pullRequestId: number; draftId: string; feedback: string },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.revise", selector, body, signal);
      return response.state;
    },
    dismissAutoFix: async (
      selector: TaskSelector,
      body: { repositoryId: string; pullRequestId: number; draftId: string },
      signal?: AbortSignal,
    ): Promise<PRAutoFixState> => {
      const response = await onTask<{ state: PRAutoFixState }>("pull_requests.autofix.dismiss", selector, body, signal);
      return response.state;
    },
    replyPullRequest: (
      selector: TaskSelector,
      body: {
        repositoryId: string;
        pullRequestId: number;
        threadId?: number;
        threadRef?: string;
        content: string;
        resolveThread?: boolean;
      },
      signal?: AbortSignal,
    ) => onTask<{ threadId: number; commentId: number }>("pull_requests.reply", selector, body, signal),
  };
}

export type TaskLinksApi = ReturnType<typeof createTaskLinksApi>;
