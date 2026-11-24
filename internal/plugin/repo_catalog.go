package plugin

import (
	"context"
	"sort"
	"strings"
	"time"

	"kandev-plugin-azure-devops/internal/azure"
	"kandev-plugin-azure-devops/internal/store"
)

const (
	repoCatalogTTL        = 10 * time.Minute
	repoCatalogFetchLimit = 60 * time.Second
	repoCatalogActionWait = 12 * time.Second
	teamCatalogTTL        = 5 * time.Minute
	teamCatalogFetchLimit = 30 * time.Second
)

type cachedRepoCatalog struct {
	generation      int64
	organizationURL string
	loadedAt        time.Time
	repositories    []azure.Repository
	byProject       map[string][]azure.Repository
}

type inflightRepos struct {
	done chan struct{}
	err  error
}

type cachedTeamList struct {
	generation int64
	loadedAt   time.Time
	teams      []azure.Team
	mineIDs    []string
}

func (c *Connections) clearWorkspaceCaches(workspaceID string) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	delete(c.repoCatalogs, workspaceID)
	delete(c.repoInflight, workspaceID)
	prefix := workspaceID + "\x00"
	for key := range c.repoInflight {
		if strings.HasPrefix(key, prefix) {
			delete(c.repoInflight, key)
		}
	}
	delete(c.projectRepoCache, workspaceID)
	delete(c.teamCache, workspaceID)
}

// WarmRepositories starts a non-blocking background fetch of the workspace's
// repository catalog when it is missing or expired, so large Azure DevOps
// organizations (20k+ repos) are warm in memory before the user opens a picker.
func (c *Connections) WarmRepositories(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), repoCatalogFetchLimit)
	go func() {
		defer cancel()
		_, _, _ = c.AllRepositories(ctx, workspaceID, repoCatalogFetchLimit)
	}()
}

// CachedAllRepositories returns the cached organization repository catalog
// immediately if fresh for the current connection generation.
func (c *Connections) CachedAllRepositories(workspaceID string, connection store.Connection) ([]azure.Repository, bool) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	entry, ok := c.repoCatalogs[workspaceID]
	if !ok || entry.generation != connection.Generation || entry.organizationURL != connection.OrganizationURL {
		return nil, false
	}
	if c.now().Sub(entry.loadedAt) > repoCatalogTTL {
		return nil, false
	}
	return entry.repositories, true
}

// AllRepositories returns every repository in the connected organization,
// coalescing concurrent callers and running the upstream read on a detached
// 60-second context so a 15-second plugin action timeout does not abort an
// 18-second enterprise catalog transfer.
func (c *Connections) AllRepositories(ctx context.Context, workspaceID string, waitLimit time.Duration) ([]azure.Repository, store.Connection, error) {
	client, connection, err := c.Client(ctx, workspaceID)
	if err != nil {
		return nil, store.Connection{}, err
	}
	if cached, ok := c.CachedAllRepositories(workspaceID, connection); ok {
		return cached, connection, nil
	}

	flightKey := workspaceID
	c.catalogMu.Lock()
	flight := c.repoInflight[flightKey]
	if flight == nil {
		flight = &inflightRepos{done: make(chan struct{})}
		if c.repoInflight == nil {
			c.repoInflight = make(map[string]*inflightRepos)
		}
		c.repoInflight[flightKey] = flight
		go c.populateRepoCatalog(workspaceID, connection, client, flight)
	}
	c.catalogMu.Unlock()

	waitCtx := ctx
	var cancel context.CancelFunc
	if waitLimit > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, waitLimit)
		defer cancel()
	}
	select {
	case <-flight.done:
		if cached, ok := c.CachedAllRepositories(workspaceID, connection); ok {
			return cached, connection, nil
		}
		if flight.err != nil {
			return nil, connection, flight.err
		}
		return []azure.Repository{}, connection, nil
	case <-waitCtx.Done():
		return nil, connection, waitCtx.Err()
	}
}

func (c *Connections) populateRepoCatalog(workspaceID string, bound store.Connection, client Client, flight *inflightRepos) {
	defer func() {
		c.catalogMu.Lock()
		if c.repoInflight[workspaceID] == flight {
			delete(c.repoInflight, workspaceID)
		}
		c.catalogMu.Unlock()
		close(flight.done)
	}()

	fetchCtx, cancel := context.WithTimeout(context.Background(), repoCatalogFetchLimit)
	defer cancel()
	repos, err := client.ListAllRepositories(fetchCtx)
	if err != nil {
		flight.err = err
		return
	}
	current, found, loadErr := c.store.Connection(fetchCtx, workspaceID)
	if loadErr != nil || !found || current.Generation != bound.Generation || current.OrganizationURL != bound.OrganizationURL {
		return
	}

	byProject := make(map[string][]azure.Repository)
	for _, repo := range repos {
		if repo.IsDisabled {
			continue
		}
		if repo.ProjectID != "" {
			key := strings.ToLower(repo.ProjectID)
			byProject[key] = append(byProject[key], repo)
		}
		if repo.ProjectName != "" {
			nameKey := "name:" + strings.ToLower(repo.ProjectName)
			byProject[nameKey] = append(byProject[nameKey], repo)
		}
	}
	for key := range byProject {
		list := byProject[key]
		sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
		byProject[key] = list
	}

	c.catalogMu.Lock()
	if c.repoCatalogs == nil {
		c.repoCatalogs = make(map[string]cachedRepoCatalog)
	}
	c.repoCatalogs[workspaceID] = cachedRepoCatalog{
		generation:      bound.Generation,
		organizationURL: bound.OrganizationURL,
		loadedAt:        c.now(),
		repositories:    repos,
		byProject:       byProject,
	}
	c.catalogMu.Unlock()
}

// ProjectRepositories returns enabled repositories of projectID sorted by name,
// reusing the organization catalog when warm or caching per-project reads.
func (c *Connections) ProjectRepositories(ctx context.Context, workspaceID, projectID string) ([]azure.Repository, error) {
	client, connection, err := c.Client(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	projectKey := strings.ToLower(strings.TrimSpace(projectID))

	c.catalogMu.Lock()
	if orgCat, ok := c.repoCatalogs[workspaceID]; ok &&
		orgCat.generation == connection.Generation &&
		orgCat.organizationURL == connection.OrganizationURL &&
		c.now().Sub(orgCat.loadedAt) <= repoCatalogTTL {
		if list, found := orgCat.byProject[projectKey]; found {
			c.catalogMu.Unlock()
			return list, nil
		}
		if list, found := orgCat.byProject["name:"+projectKey]; found {
			c.catalogMu.Unlock()
			return list, nil
		}
	}
	if byProj := c.projectRepoCache[workspaceID]; byProj != nil {
		if cached, ok := byProj[projectKey]; ok && cached.generation == connection.Generation && c.now().Sub(cached.loadedAt) <= repoCatalogTTL {
			c.catalogMu.Unlock()
			return cached.repositories, nil
		}
	}
	flightKey := workspaceID + "\x00" + projectKey
	flight := c.repoInflight[flightKey]
	if flight == nil {
		flight = &inflightRepos{done: make(chan struct{})}
		if c.repoInflight == nil {
			c.repoInflight = make(map[string]*inflightRepos)
		}
		c.repoInflight[flightKey] = flight
		go c.populateProjectRepoCache(workspaceID, projectID, projectKey, connection, client, flight)
	}
	c.catalogMu.Unlock()

	select {
	case <-flight.done:
		c.catalogMu.Lock()
		if byProj := c.projectRepoCache[workspaceID]; byProj != nil {
			if cached, ok := byProj[projectKey]; ok && cached.generation == connection.Generation {
				c.catalogMu.Unlock()
				return cached.repositories, nil
			}
		}
		c.catalogMu.Unlock()
		if flight.err != nil {
			return nil, flight.err
		}
		return []azure.Repository{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Connections) populateProjectRepoCache(
	workspaceID, projectID, projectKey string, bound store.Connection, client Client, flight *inflightRepos,
) {
	flightKey := workspaceID + "\x00" + projectKey
	defer func() {
		c.catalogMu.Lock()
		if c.repoInflight[flightKey] == flight {
			delete(c.repoInflight, flightKey)
		}
		c.catalogMu.Unlock()
		close(flight.done)
	}()

	fetchCtx, cancel := context.WithTimeout(context.Background(), repoCatalogFetchLimit)
	defer cancel()
	raw, err := client.ListRepositories(fetchCtx, projectID)
	if err != nil {
		flight.err = err
		return
	}
	current, found, loadErr := c.store.Connection(fetchCtx, workspaceID)
	if loadErr != nil || !found || current.Generation != bound.Generation || current.OrganizationURL != bound.OrganizationURL {
		return
	}
	enabled := make([]azure.Repository, 0, len(raw))
	for _, repo := range raw {
		if !repo.IsDisabled {
			enabled = append(enabled, repo)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		if folded := strings.Compare(strings.ToLower(enabled[i].Name), strings.ToLower(enabled[j].Name)); folded != 0 {
			return folded < 0
		}
		return enabled[i].Name < enabled[j].Name
	})

	c.catalogMu.Lock()
	if c.projectRepoCache == nil {
		c.projectRepoCache = make(map[string]map[string]cachedProjectRepos)
	}
	if c.projectRepoCache[workspaceID] == nil {
		c.projectRepoCache[workspaceID] = make(map[string]cachedProjectRepos)
	}
	c.projectRepoCache[workspaceID][projectKey] = cachedProjectRepos{
		generation:   bound.Generation,
		loadedAt:     c.now(),
		repositories: enabled,
	}
	c.catalogMu.Unlock()
}

type cachedProjectRepos struct {
	generation   int64
	loadedAt     time.Time
	repositories []azure.Repository
}

// ProjectTeams returns cached (teams, mineIDs) for projectID.
func (c *Connections) ProjectTeams(ctx context.Context, workspaceID, projectID string) ([]azure.Team, []string, error) {
	client, connection, err := c.Client(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	projectKey := strings.ToLower(strings.TrimSpace(projectID))

	c.catalogMu.Lock()
	if byProj := c.teamCache[workspaceID]; byProj != nil {
		if cached, ok := byProj[projectKey]; ok && cached.generation == connection.Generation && c.now().Sub(cached.loadedAt) <= teamCatalogTTL {
			c.catalogMu.Unlock()
			return cached.teams, cached.mineIDs, nil
		}
	}
	c.catalogMu.Unlock()

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teamCatalogFetchLimit)
	defer cancel()
	var teams []azure.Team
	var mine []string
	var teamsErr, mineErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		mine, mineErr = client.ListMyTeamIDs(fetchCtx, projectID)
	}()
	teams, teamsErr = client.ListTeams(fetchCtx, projectID)
	<-done
	if teamsErr != nil {
		return nil, nil, teamsErr
	}
	if mineErr != nil {
		mine = nil
	}

	c.catalogMu.Lock()
	if c.teamCache == nil {
		c.teamCache = make(map[string]map[string]cachedTeamList)
	}
	if c.teamCache[workspaceID] == nil {
		c.teamCache[workspaceID] = make(map[string]cachedTeamList)
	}
	c.teamCache[workspaceID][projectKey] = cachedTeamList{
		generation: connection.Generation,
		loadedAt:   c.now(),
		teams:      nonNil(teams),
		mineIDs:    nonNil(mine),
	}
	c.catalogMu.Unlock()
	return nonNil(teams), nonNil(mine), nil
}
