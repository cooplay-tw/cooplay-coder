package admissionapi_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

// TestWorkspaceAgents checks the agents of the admission workspace view: the
// connection state of the latest build's agents as recorded in the
// database, with the controller contract keys.
func TestWorkspaceAgents(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	clock.Set(dbtime.Now())
	env := newAPIEnv(t, clock)
	ws := env.agentWorkspace(t)
	agentID := ws.Agents[0].ID
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	path := "/v1/workspaces/" + ws.Workspace.ID.String()

	// agents returns the agents of a workspace response with the JSON keys
	// of each agent.
	agents := func(raw json.RawMessage) ([]admissionapi.WorkspaceAgent, [][]string) {
		t.Helper()
		var body struct {
			Agents []map[string]json.RawMessage `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		keys := make([][]string, 0, len(body.Agents))
		for _, agent := range body.Agents {
			k := make([]string, 0, len(agent))
			for key := range agent {
				k = append(k, key)
			}
			sort.Strings(k)
			keys = append(keys, k)
		}
		var workspace admissionapi.Workspace
		require.NoError(t, json.Unmarshal(raw, &workspace))
		return workspace.Agents, keys
	}
	setConnection := func(lastConnectedAt, disconnectedAt time.Time) {
		t.Helper()
		require.NoError(t, env.db.UpdateWorkspaceAgentConnectionByID(ctx, database.UpdateWorkspaceAgentConnectionByIDParams{
			ID:               agentID,
			FirstConnectedAt: sql.NullTime{Time: lastConnectedAt, Valid: true},
			LastConnectedAt:  sql.NullTime{Time: lastConnectedAt, Valid: true},
			DisconnectedAt:   sql.NullTime{Time: disconnectedAt, Valid: !disconnectedAt.IsZero()},
			UpdatedAt:        clock.Now(),
		}))
	}

	// An agent that never connected.
	var raw json.RawMessage
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	got, keys := agents(raw)
	require.Equal(t, [][]string{{"id", "status"}}, keys)
	require.Equal(t, agentID, got[0].ID)
	require.Equal(t, string(codersdk.WorkspaceAgentConnecting), got[0].Status)

	// Connected.
	connectedAt := dbtime.Time(clock.Now())
	setConnection(connectedAt, time.Time{})
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	got, keys = agents(raw)
	require.Equal(t, [][]string{{"id", "last_connected_at", "status"}}, keys)
	require.Equal(t, string(codersdk.WorkspaceAgentConnected), got[0].Status)
	require.True(t, got[0].LastConnectedAt.Equal(connectedAt))

	// Disconnected after the last connection.
	disconnectedAt := connectedAt.Add(time.Second)
	setConnection(connectedAt, disconnectedAt)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	got, keys = agents(raw)
	require.Equal(t, [][]string{{"disconnected_at", "id", "last_connected_at", "status"}}, keys)
	require.Equal(t, string(codersdk.WorkspaceAgentDisconnected), got[0].Status)
	require.True(t, got[0].DisconnectedAt.Equal(disconnectedAt))

	// Reconnected, then silent for longer than the inactivity timeout, as a
	// paused microVM is: disconnected since its last connection.
	clock.Advance(2 * time.Second)
	reconnectedAt := dbtime.Time(clock.Now())
	setConnection(reconnectedAt, disconnectedAt)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	got, _ = agents(raw)
	require.Equal(t, string(codersdk.WorkspaceAgentConnected), got[0].Status)
	clock.Advance(env.agentInactiveTimeout + time.Second)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	got, _ = agents(raw)
	require.Equal(t, string(codersdk.WorkspaceAgentDisconnected), got[0].Status)
	require.True(t, got[0].LastConnectedAt.Equal(reconnectedAt))
	require.True(t, got[0].DisconnectedAt.Equal(reconnectedAt))

	// The list carries the agents of every workspace.
	var listed admissionapi.ListWorkspacesResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/workspaces", nil, &listed))
	found := false
	for _, w := range listed.Workspaces {
		if w.ID != ws.Workspace.ID {
			continue
		}
		found = true
		require.Len(t, w.Agents, 1)
		require.Equal(t, agentID, w.Agents[0].ID)
		require.Equal(t, string(codersdk.WorkspaceAgentDisconnected), w.Agents[0].Status)
	}
	require.True(t, found)

	// After a STOP build the latest build has no agents.
	dbfake.WorkspaceBuild(t, env.db, ws.Workspace).Seed(database.WorkspaceBuild{
		Transition:  database.WorkspaceTransitionStop,
		BuildNumber: 2,
	}).Do()
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &raw))
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotContains(t, body, "agents")
}
