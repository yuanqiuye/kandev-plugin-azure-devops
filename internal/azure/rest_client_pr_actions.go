package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	maxFailedBuildTimelines = 4
	maxBuildFailureIssues   = 10
	maxBuildFailureChars    = 2000
)

var allowedThreadStatuses = map[string]string{
	"active":   "active",
	"fixed":    "fixed",
	"wontfix":  "wontFix",
	"closed":   "closed",
	"bydesign": "byDesign",
	"pending":  "pending",
}

// NormalizeThreadStatus validates and normalizes an Azure pull request thread status.
func NormalizeThreadStatus(status string) (string, error) {
	canonical, ok := allowedThreadStatuses[strings.ToLower(strings.TrimSpace(status))]
	if !ok {
		return "", fmt.Errorf("%w: unsupported pull request thread status %q", ErrInvalidConfig, status)
	}
	return canonical, nil
}

// ReplyToThread appends a text comment to an existing pull request thread.
func (c *RESTClient) ReplyToThread(ctx context.Context, projectID, repositoryID string, pullRequestID, threadID int, content string) (*Comment, error) {
	trimmed := strings.TrimSpace(content)
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repositoryID) == "" || pullRequestID <= 0 || threadID <= 0 || trimmed == "" {
		return nil, fmt.Errorf("%w: project, repository, pull request id, thread id, and non-empty content are required", ErrInvalidConfig)
	}
	endpoint := fmt.Sprintf("%s/threads/%d/comments?api-version=%s",
		pullRequestEndpoint(projectID, repositoryID, pullRequestID), threadID, restAPIVersion)
	var comment Comment
	if err := c.doJSON(ctx, http.MethodPost, endpoint, map[string]any{"content": trimmed, "commentType": 1}, &comment); err != nil {
		return nil, err
	}
	return &comment, nil
}

// CreateThread opens a new top-level pull request comment thread.
func (c *RESTClient) CreateThread(ctx context.Context, projectID, repositoryID string, pullRequestID int, content string) (*Thread, error) {
	trimmed := strings.TrimSpace(content)
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repositoryID) == "" || pullRequestID <= 0 || trimmed == "" {
		return nil, fmt.Errorf("%w: project, repository, pull request id, and non-empty content are required", ErrInvalidConfig)
	}
	endpoint := fmt.Sprintf("%s/threads?api-version=%s",
		pullRequestEndpoint(projectID, repositoryID, pullRequestID), restAPIVersion)
	body := map[string]any{
		"status":   "active",
		"comments": []map[string]any{{"content": trimmed, "commentType": 1}},
	}
	var thread Thread
	if err := c.doJSON(ctx, http.MethodPost, endpoint, body, &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

// UpdateThreadStatus updates the resolution status of a pull request thread.
func (c *RESTClient) UpdateThreadStatus(ctx context.Context, projectID, repositoryID string, pullRequestID, threadID int, status string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repositoryID) == "" || pullRequestID <= 0 || threadID <= 0 {
		return fmt.Errorf("%w: project, repository, pull request id, and thread id are required", ErrInvalidConfig)
	}
	canonical, err := NormalizeThreadStatus(status)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/threads/%d?api-version=%s",
		pullRequestEndpoint(projectID, repositoryID, pullRequestID), threadID, restAPIVersion)
	return c.doJSON(ctx, http.MethodPatch, endpoint, map[string]any{"status": canonical}, nil)
}

func (c *RESTClient) convertPolicyEvaluation(projectID string, raw rawPolicyEvaluation) PolicyEvaluation {
	name := strings.TrimSpace(raw.Configuration.Settings.DisplayName)
	if name == "" {
		name = strings.TrimSpace(raw.Context.BuildDefinitionName)
	}
	if name == "" {
		name = strings.TrimSpace(raw.Configuration.Type.DisplayName)
	}
	var outputParts []string
	if msg := strings.TrimSpace(raw.Context.ErrorMessage); msg != "" {
		outputParts = append(outputParts, msg)
	} else if statusText := strings.TrimSpace(raw.Context.StatusText); statusText != "" &&
		(strings.EqualFold(raw.Status, "rejected") || strings.EqualFold(raw.Status, "broken")) {
		outputParts = append(outputParts, statusText)
	}
	for _, line := range raw.Context.BuildOutputPreview {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			outputParts = append(outputParts, trimmed)
		}
	}
	buildURL := ""
	if raw.Context.BuildID > 0 && c.organization != "" && strings.TrimSpace(projectID) != "" {
		buildURL = "https://" + OrganizationHost + "/" + url.PathEscape(c.organization) + "/" +
			url.PathEscape(strings.TrimSpace(projectID)) + "/_build/results?buildId=" + strconv.Itoa(raw.Context.BuildID)
	}
	output := c.auth.Redact(strings.Join(outputParts, "\n"))
	if len(output) > maxBuildFailureChars {
		output = output[:maxBuildFailureChars]
	}
	return PolicyEvaluation{
		ID:         raw.ID,
		Status:     raw.Status,
		Name:       name,
		IsBlocking: raw.Configuration.IsBlocking,
		BuildID:    raw.Context.BuildID,
		BuildURL:   buildURL,
		Output:     output,
	}
}

func (c *RESTClient) fetchBuildFailureSummary(ctx context.Context, projectID string, buildID int) string {
	endpoint := fmt.Sprintf("/%s/_apis/build/builds/%d/timeline?api-version=%s", pathPart(projectID), buildID, restAPIVersion)
	var response struct {
		Records []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Result string `json:"result"`
			Issues []struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"issues"`
		} `json:"records"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return ""
	}
	var lines []string
	for _, rec := range response.Records {
		failed := strings.EqualFold(rec.Result, "failed")
		hasErrors := false
		for _, issue := range rec.Issues {
			if strings.EqualFold(issue.Type, "error") && strings.TrimSpace(issue.Message) != "" {
				hasErrors = true
				break
			}
		}
		if !failed && !hasErrors {
			continue
		}
		taskName := strings.TrimSpace(rec.Name)
		if taskName == "" {
			taskName = "Build step"
		}
		addedIssue := false
		for _, issue := range rec.Issues {
			msg := strings.TrimSpace(issue.Message)
			if msg == "" || !strings.EqualFold(issue.Type, "error") {
				continue
			}
			lines = append(lines, fmt.Sprintf("[%s] %s", taskName, msg))
			addedIssue = true
			if len(lines) >= maxBuildFailureIssues {
				break
			}
		}
		if !addedIssue && failed {
			lines = append(lines, fmt.Sprintf("[%s] failed", taskName))
		}
		if len(lines) >= maxBuildFailureIssues {
			break
		}
	}
	summary := c.auth.Redact(strings.Join(lines, "\n"))
	if len(summary) > maxBuildFailureChars {
		summary = summary[:maxBuildFailureChars]
	}
	return summary
}
