import { describe, expect, it } from "vitest";
import { createEditableDraftForm, statusBadgeLabel } from "./autofix-notice";
import type { PRAutoFixState } from "./api";

describe("autofix draft form", () => {
  it("defaults resolveThread to false and enables commitPush when changes exist", () => {
    const form = createEditableDraftForm({
      draftId: "d1",
      summary: "Fixed null guard",
      hasChanges: true,
      commitMessage: "fix: guard null token",
      replies: [
        {
          id: "r1",
          threadId: 7,
          author: "Grace",
          filePath: "src/auth.ts",
          line: 29,
          threadExcerpt: "Check null token",
          content: "Added null token guard.",
          resolveThread: false,
        },
      ],
    });
    expect(form.commitPush).toBe(true);
    expect(form.commitMessage).toBe("fix: guard null token");
    expect(form.replies).toEqual([
      expect.objectContaining({
        id: "r1",
        threadId: 7,
        send: true,
        content: "Added null token guard.",
        resolveThread: false,
        filePath: "src/auth.ts",
        line: 29,
      }),
    ]);
  });

  it("disables commitPush by default when neither changes nor commitMessage are present", () => {
    const form = createEditableDraftForm({
      draftId: "d2",
      summary: "Explained behavior only",
      hasChanges: false,
      replies: [{ id: "r2", threadId: 9, content: "Intentional retry backoff.", resolveThread: false }],
    });
    expect(form.commitPush).toBe(false);
    expect(form.replies[0]?.resolveThread).toBe(false);
  });

  it("maps status badges for every auto-fix state", () => {
    const t = (key: string, opts?: Record<string, unknown>) =>
      opts ? `${key}:${JSON.stringify(opts)}` : key;
    const base: PRAutoFixState = {
      repositoryId: "repo",
      pullRequestId: 42,
      enabled: true,
      requireApproval: true,
      status: "idle",
      roundCount: 2,
      maxRounds: 5,
    };
    expect(statusBadgeLabel(t, base)).toBe("autoFixStatusMonitoring");
    expect(statusBadgeLabel(t, { ...base, status: "awaiting_approval" })).toBe("autoFixStatusAwaitingApproval");
    expect(statusBadgeLabel(t, { ...base, status: "running" })).toBe('autoFixStatusRunning:{"round":2,"max":5}');
    expect(statusBadgeLabel(t, { ...base, status: "exhausted" })).toBe('autoFixStatusExhausted:{"max":5}');
  });
});
