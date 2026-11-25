package azure

import (
	"encoding/json"
	"time"
)

// Project is an Azure DevOps project visible to the authenticated user.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Repository is an Azure Repos Git repository.
type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ProjectID     string `json:"projectId"`
	ProjectName   string `json:"projectName"`
	DefaultBranch string `json:"defaultBranch"`
	WebURL        string `json:"webUrl"`
	IsDisabled    bool   `json:"isDisabled,omitempty"`
}

type Branch struct {
	Name string `json:"name"`
}

// Identity is the stable subset shared by Azure authors, reviewers, and
// comment authors.
type Identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName,omitempty"`
}

// WorkItem is a hydrated Azure Boards work item. Fields retains provider
// extensions while the named fields drive the Kandev browse UI.
type WorkItem struct {
	ID            int            `json:"id"`
	Revision      int            `json:"revision"`
	Title         string         `json:"title"`
	Description   string         `json:"description,omitempty"`
	State         string         `json:"state"`
	StateCategory string         `json:"stateCategory,omitempty"`
	Type          string         `json:"type"`
	Project       string         `json:"project,omitempty"`
	AreaPath      string         `json:"areaPath,omitempty"`
	AssignedTo    string         `json:"assignedTo,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
	WebURL        string         `json:"webUrl,omitempty"`
	APIURL        string         `json:"apiUrl,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
}

type WorkItemSearchResult struct {
	Items []WorkItem `json:"items"`
	Count int        `json:"count"`
}

// PlanningField is one supported Azure planning value normalized for the
// work-item detail surface. Values stay strings because Azure returns either
// numeric or string values depending on the process template.
type PlanningField struct {
	ReferenceName string `json:"referenceName"`
	Label         string `json:"label"`
	Value         string `json:"value"`
}

// WorkItemDetail is the full read-only projection used by the Azure work-item
// detail surface. WorkItem remains embedded so existing card fields retain
// their response shape.
type WorkItemDetail struct {
	WorkItem
	PlanningFields []PlanningField `json:"planningFields"`
}

// WorkItemComment is the safe subset of an Azure Boards discussion comment.
type WorkItemComment struct {
	ID          int        `json:"id"`
	Content     string     `json:"content"`
	Author      Identity   `json:"author"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// WorkItemCommentPage carries Azure's opaque continuation token verbatim.
type WorkItemCommentPage struct {
	Comments          []WorkItemComment `json:"comments"`
	ContinuationToken string            `json:"continuationToken,omitempty"`
}

type PullRequestFilter struct {
	ProjectID    string
	RepositoryID string
	Status       string
	CreatorID    string
	ReviewerID   string
	SourceBranch string
	TargetBranch string
	Skip         int
	Top          int
}

// PullRequest is the Azure-native PR summary used by list and task-link flows.
type PullRequest struct {
	ID             int        `json:"id"`
	Title          string     `json:"title"`
	Description    string     `json:"description,omitempty"`
	Status         string     `json:"status"`
	IsDraft        bool       `json:"isDraft"`
	SourceBranch   string     `json:"sourceBranch"`
	TargetBranch   string     `json:"targetBranch"`
	MergeStatus    string     `json:"mergeStatus,omitempty"`
	CreationDate   *time.Time `json:"creationDate,omitempty"`
	ClosedDate     *time.Time `json:"closedDate,omitempty"`
	Author         Identity   `json:"author"`
	ProjectID      string     `json:"projectId"`
	ProjectName    string     `json:"projectName"`
	RepositoryID   string     `json:"repositoryId"`
	RepositoryName string     `json:"repositoryName"`
	WebURL         string     `json:"webUrl"`
	APIURL         string     `json:"apiUrl"`
}

// CreatePullRequestRequest opens an Azure Repos pull request from a pushed
// branch. Branch names may be given with or without the refs/heads/ prefix.
type CreatePullRequestRequest struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
	IsDraft      bool
	WorkItemIDs  []int
}

type PullRequestPage struct {
	Items []PullRequest `json:"items"`
	Count int           `json:"count"`
	Skip  int           `json:"skip"`
	Top   int           `json:"top"`
}

type Reviewer struct {
	Identity
	Vote        int  `json:"vote"`
	IsRequired  bool `json:"isRequired"`
	HasDeclined bool `json:"hasDeclined"`
}

type Comment struct {
	ID          int       `json:"id"`
	ParentID    int       `json:"parentCommentId,omitempty"`
	Content     string    `json:"content"`
	Author      Identity  `json:"author"`
	CommentType string    `json:"commentType"`
	IsDeleted   bool      `json:"isDeleted,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// UnmarshalJSON translates Azure's wire timestamp and enum shapes while
// preserving Kandev's public response contract.
func (c *Comment) UnmarshalJSON(data []byte) error {
	type commentAlias struct {
		ID          int             `json:"id"`
		ParentID    int             `json:"parentCommentId"`
		Content     string          `json:"content"`
		Author      Identity        `json:"author"`
		CommentType json.RawMessage `json:"commentType"`
		IsDeleted   bool            `json:"isDeleted"`
		PublishedAt time.Time       `json:"publishedAt"`
		UpdatedAt   time.Time       `json:"updatedAt"`
	}
	var wire struct {
		commentAlias
		PublishedDate   time.Time `json:"publishedDate"`
		LastUpdatedDate time.Time `json:"lastUpdatedDate"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = Comment{
		ID:          wire.ID,
		ParentID:    wire.ParentID,
		Content:     wire.Content,
		Author:      wire.Author,
		CommentType: decodeCommentType(wire.CommentType),
		IsDeleted:   wire.IsDeleted,
		PublishedAt: wire.PublishedAt,
		UpdatedAt:   wire.UpdatedAt,
	}
	if !wire.PublishedDate.IsZero() {
		c.PublishedAt = wire.PublishedDate
	}
	if !wire.LastUpdatedDate.IsZero() {
		c.UpdatedAt = wire.LastUpdatedDate
	}
	return nil
}

func decodeCommentType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var num int
	if err := json.Unmarshal(raw, &num); err == nil {
		switch num {
		case 1:
			return "text"
		case 2:
			return "codeChange"
		case 3:
			return "system"
		}
	}
	return ""
}

type Thread struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	FilePath  string    `json:"filePath,omitempty"`
	Line      int       `json:"line,omitempty"`
	IsDeleted bool      `json:"isDeleted,omitempty"`
	Comments  []Comment `json:"comments"`
}

func (t *Thread) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID            int             `json:"id"`
		Status        json.RawMessage `json:"status"`
		FilePath      string          `json:"filePath"`
		Line          int             `json:"line"`
		IsDeleted     bool            `json:"isDeleted"`
		Comments      []Comment       `json:"comments"`
		ThreadContext *struct {
			FilePath       string `json:"filePath"`
			RightFileStart *struct {
				Line int `json:"line"`
			} `json:"rightFileStart"`
			LeftFileStart *struct {
				Line int `json:"line"`
			} `json:"leftFileStart"`
		} `json:"threadContext"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	filePath := wire.FilePath
	line := wire.Line
	if filePath == "" && wire.ThreadContext != nil && wire.ThreadContext.FilePath != "" {
		filePath = wire.ThreadContext.FilePath
		if len(filePath) > 1 && filePath[0] == '/' {
			filePath = filePath[1:]
		}
	}
	if line == 0 && wire.ThreadContext != nil {
		if wire.ThreadContext.RightFileStart != nil && wire.ThreadContext.RightFileStart.Line > 0 {
			line = wire.ThreadContext.RightFileStart.Line
		} else if wire.ThreadContext.LeftFileStart != nil && wire.ThreadContext.LeftFileStart.Line > 0 {
			line = wire.ThreadContext.LeftFileStart.Line
		}
	}
	*t = Thread{
		ID:        wire.ID,
		Status:    decodeThreadStatus(wire.Status),
		FilePath:  filePath,
		Line:      line,
		IsDeleted: wire.IsDeleted,
		Comments:  wire.Comments,
	}
	return nil
}

func decodeThreadStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var num int
	if err := json.Unmarshal(raw, &num); err == nil {
		switch num {
		case 1:
			return "active"
		case 2:
			return "fixed"
		case 3:
			return "wontFix"
		case 4:
			return "closed"
		case 5:
			return "byDesign"
		case 6:
			return "pending"
		}
	}
	return ""
}

type WorkItemRef struct {
	ID  int    `json:"id"`
	URL string `json:"url"`
}

type PolicyEvaluation struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Name       string `json:"name"`
	IsBlocking bool   `json:"isBlocking"`
	BuildID    int    `json:"buildId,omitempty"`
	BuildURL   string `json:"buildUrl,omitempty"`
	Output     string `json:"output,omitempty"`
}
