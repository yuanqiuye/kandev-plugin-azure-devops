package azure

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const stateCatalogTTL = 10 // cached for the client instance lifetime

type projectStateCatalog struct {
	byTypeAndState map[string]string
	byState        map[string]string
	byCategory     map[string][]string
}

// FallbackStateCategory maps standard Azure Boards state names to their
// canonical category when project metadata is unavailable.
func FallbackStateCategory(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "done", "closed", "completed", "inactive":
		return "Completed"
	case "removed", "cut":
		return stateCategoryRemoved
	case "resolved":
		return "Resolved"
	case "to do", "new", "proposed", "requested", "design":
		return "Proposed"
	case "":
		return ""
	default:
		return "InProgress"
	}
}

func (c *RESTClient) loadProjectStateCatalog(ctx context.Context, projectID string) *projectStateCatalog {
	projectKey := strings.ToLower(strings.TrimSpace(projectID))
	if projectKey == "" {
		return nil
	}
	c.stateMu.Lock()
	if c.projectStates != nil {
		if cached, ok := c.projectStates[projectKey]; ok {
			c.stateMu.Unlock()
			return cached
		}
	}
	c.stateMu.Unlock()

	var response struct {
		Value []struct {
			Name   string          `json:"name"`
			States []WorkItemState `json:"states"`
		} `json:"value"`
	}
	endpoint := fmt.Sprintf("/%s/_apis/wit/workitemtypes?api-version=%s", pathPart(projectID), restAPIVersion)
	err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &response)
	if err != nil {
		// Cache an empty sentinel so servers without this endpoint (such as
		// unit-test mocks) do not repeat the request on every search.
		empty := &projectStateCatalog{}
		c.stateMu.Lock()
		if c.projectStates == nil {
			c.projectStates = make(map[string]*projectStateCatalog)
		}
		c.projectStates[projectKey] = empty
		c.stateMu.Unlock()
		return empty
	}

	cat := &projectStateCatalog{
		byTypeAndState: make(map[string]string),
		byState:        make(map[string]string),
		byCategory:     make(map[string][]string),
	}
	seenByCat := make(map[string]map[string]bool)
	for _, wit := range response.Value {
		for _, st := range wit.States {
			if st.Name == "" || st.Category == "" {
				continue
			}
			typeKey := strings.ToLower(wit.Name) + "\x00" + strings.ToLower(st.Name)
			cat.byTypeAndState[typeKey] = st.Category
			stateLower := strings.ToLower(st.Name)
			if _, exists := cat.byState[stateLower]; !exists {
				cat.byState[stateLower] = st.Category
			}
			catKey := strings.ToLower(st.Category)
			if seenByCat[catKey] == nil {
				seenByCat[catKey] = make(map[string]bool)
			}
			if !seenByCat[catKey][stateLower] {
				seenByCat[catKey][stateLower] = true
				cat.byCategory[catKey] = append(cat.byCategory[catKey], st.Name)
			}
		}
	}
	for catKey := range cat.byCategory {
		sort.Strings(cat.byCategory[catKey])
	}

	c.stateMu.Lock()
	if c.projectStates == nil {
		c.projectStates = make(map[string]*projectStateCatalog)
	}
	c.projectStates[projectKey] = cat
	c.stateMu.Unlock()
	_ = stateCatalogTTL
	return cat
}

func (c *RESTClient) cachedOrFallbackStateCategory(projectID string, item WorkItem) string {
	projectKey := strings.ToLower(strings.TrimSpace(projectID))
	if projectKey == "" {
		projectKey = strings.ToLower(strings.TrimSpace(item.Project))
	}
	c.stateMu.Lock()
	var cat *projectStateCatalog
	if c.projectStates != nil {
		cat = c.projectStates[projectKey]
	}
	c.stateMu.Unlock()
	if cat != nil && len(cat.byTypeAndState) > 0 {
		if category, ok := cat.byTypeAndState[strings.ToLower(item.Type)+"\x00"+strings.ToLower(item.State)]; ok {
			return category
		}
		if category, ok := cat.byState[strings.ToLower(item.State)]; ok {
			return category
		}
	}
	return FallbackStateCategory(item.State)
}

func (c *RESTClient) populateWorkItemStateCategories(ctx context.Context, projectID string, items []WorkItem) {
	if len(items) == 0 {
		return
	}
	project := projectID
	if project == "" {
		project = items[0].Project
	}
	cat := c.loadProjectStateCatalog(ctx, project)
	for i := range items {
		if items[i].StateCategory != "" {
			continue
		}
		if cat != nil && len(cat.byTypeAndState) > 0 {
			typeKey := strings.ToLower(items[i].Type) + "\x00" + strings.ToLower(items[i].State)
			if category, ok := cat.byTypeAndState[typeKey]; ok {
				items[i].StateCategory = category
				continue
			}
			if category, ok := cat.byState[strings.ToLower(items[i].State)]; ok {
				items[i].StateCategory = category
				continue
			}
		}
		items[i].StateCategory = FallbackStateCategory(items[i].State)
	}
}

// ProjectStatesByCategory returns the project's state names grouped by
// lowercase category ("proposed", "inprogress", "resolved", "completed",
// "removed"), or nil when metadata is unavailable.
func (c *RESTClient) ProjectStatesByCategory(ctx context.Context, projectID string) (map[string][]string, error) {
	cat := c.loadProjectStateCatalog(ctx, projectID)
	if cat == nil || len(cat.byCategory) == 0 {
		return nil, nil
	}
	return cat.byCategory, nil
}
