package plugin

import (
	"context"
	"errors"
	"strings"
	"time"

	"kandev-plugin-azure-devops/internal/azure"
)

// listTeams lists the project's teams and names those of the connected
// identity, paged below Kandev's 1 MiB action response limit.
func (b *Browse) listTeams(ctx context.Context, workspaceID string, in browseInput) (any, error) {
	if err := in.requireProject(); err != nil {
		return nil, err
	}
	skip, err := decodeSkipCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	teams, mine, err := b.connections.ProjectTeams(ctx, workspaceID, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if skip >= len(teams) {
		return map[string]any{"teams": []azure.Team{}, "mineIds": []string{}}, nil
	}
	end := min(skip+maxTeamsPerActionPage, len(teams))
	mineSlice := []string{}
	if skip == 0 {
		mineSlice = nonNil(mine)
	}
	result := map[string]any{"teams": teams[skip:end], "mineIds": mineSlice}
	if end < len(teams) {
		result["nextCursor"] = encodeSkipCursor(end)
	}
	return result, nil
}

func (in browseInput) requireTeam() error {
	if strings.TrimSpace(in.ProjectID) == "" || strings.TrimSpace(in.TeamID) == "" {
		return invalidArgument(errors.New("a project and a team are required"))
	}
	return nil
}

func getTeamSettings(ctx context.Context, client Client, in browseInput) (any, error) {
	if err := in.requireTeam(); err != nil {
		return nil, err
	}
	return client.GetTeamSettings(ctx, in.ProjectID, in.TeamID)
}

func getSprintBoard(ctx context.Context, client Client, in browseInput) (any, error) {
	if err := in.requireTeam(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.IterationID) == "" {
		return nil, invalidArgument(errors.New("a sprint is required"))
	}
	board, err := client.GetSprintBoard(ctx, in.ProjectID, in.TeamID, in.IterationID, time.Now())
	if err != nil {
		return nil, err
	}
	for index := range board.Rows {
		row := &board.Rows[index]
		if row.Parent != nil {
			parent := summary(*row.Parent)
			row.Parent = &parent
		}
		for task := range row.Tasks {
			row.Tasks[task].WorkItem = summary(row.Tasks[task].WorkItem)
		}
	}
	return board, nil
}
