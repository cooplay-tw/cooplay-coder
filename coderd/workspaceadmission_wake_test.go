package coderd_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/provisionersdk/proto"
	"github.com/coder/coder/v2/tailnet"
	tailnetproto "github.com/coder/coder/v2/tailnet/proto"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/websocket"
)

type wakeEnv struct {
	api      *coderd.API
	db       database.Store
	owner    *codersdk.Client
	orgID    uuid.UUID
	member   *codersdk.Client
	memberID uuid.UUID
}

func newWakeEnv(t *testing.T) wakeEnv {
	t.Helper()
	db, ps := dbtestutil.NewDB(t)
	owner, _, api := coderdtest.NewWithAPI(t, &coderdtest.Options{
		Database: db,
		Pubsub:   ps,
	})
	first := coderdtest.CreateFirstUser(t, owner)
	member, memberUser := coderdtest.CreateAnotherUser(t, owner, first.OrganizationID)
	return wakeEnv{api: api, db: db, owner: owner, orgID: first.OrganizationID, member: member, memberID: memberUser.ID}
}

// workspace creates a running workspace of the member with one agent that
// serves an app with slug "app". The agent never connects and the template
// is not managed.
func (e wakeEnv) workspace(t *testing.T) dbfake.WorkspaceResponse {
	t.Helper()
	return dbfake.WorkspaceBuild(t, e.db, database.WorkspaceTable{
		OrganizationID: e.orgID,
		OwnerID:        e.memberID,
	}).WithAgent(func(agents []*proto.Agent) []*proto.Agent {
		agents[0].Apps = []*proto.App{{
			Slug:        "app",
			DisplayName: "app",
			Url:         "http://127.0.0.1:1",
		}}
		return agents
	}).Do()
}

// managedWorkspace creates a workspace like workspace on a managed template.
func (e wakeEnv) managedWorkspace(t *testing.T) dbfake.WorkspaceResponse {
	t.Helper()
	resp := e.workspace(t)
	admissiontest.Manage(t, e.db, resp.Workspace.TemplateID, admissiontest.ProfileParameter)
	return resp
}

func (e wakeEnv) wakeIntent(t *testing.T, workspaceID uuid.UUID) (database.WorkspaceAdmissionWakeIntent, bool) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	rows, err := e.db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{LimitCount: 1000})
	require.NoError(t, err)
	for _, row := range rows {
		if row.WorkspaceID == workspaceID {
			return row, true
		}
	}
	return database.WorkspaceAdmissionWakeIntent{}, false
}

func (e wakeEnv) requireWakeIntent(t *testing.T, workspaceID uuid.UUID, source string) database.WorkspaceAdmissionWakeIntent {
	t.Helper()
	var intent database.WorkspaceAdmissionWakeIntent
	require.Eventually(t, func() bool {
		var ok bool
		intent, ok = e.wakeIntent(t, workspaceID)
		return ok
	}, testutil.WaitLong, testutil.IntervalFast, "wake intent of workspace %s", workspaceID)
	require.Equal(t, source, intent.Source)
	require.Equal(t, uuid.NullUUID{UUID: e.memberID, Valid: true}, intent.RequestedBy)
	return intent
}

func wakeCounter(t *testing.T, registry *prometheus.Registry, source, result string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "coderd_workspace_wake_intents_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["source"] == source && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// dialTailnet opens the user tailnet RPC connection of the member.
func (e wakeEnv) dialTailnet(ctx context.Context, t *testing.T, header http.Header) tailnetproto.DRPCTailnetClient {
	t.Helper()
	u, err := e.member.URL.Parse("/api/v2/tailnet?version=2.0")
	require.NoError(t, err)
	if header == nil {
		header = http.Header{}
	}
	header.Set(codersdk.SessionTokenHeader, e.member.SessionToken())
	//nolint:bodyclose // websocket.Dial owns the HTTP response body on success.
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil && resp != nil {
		err = codersdk.ReadBodyAsError(resp)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	client, err := tailnet.NewDRPCClient(websocket.NetConn(ctx, conn, websocket.MessageBinary), testutil.Logger(t))
	require.NoError(t, err)
	return client
}

func addTunnel(ctx context.Context, t *testing.T, client tailnetproto.DRPCTailnetClient, agentID uuid.UUID) {
	t.Helper()
	stream, err := client.Coordinate(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&tailnetproto.CoordinateRequest{
		AddTunnel: &tailnetproto.CoordinateRequest_Tunnel{Id: tailnet.UUIDToByteSlice(agentID)},
	}))
}

func TestWakeIntentTailnet(t *testing.T) {
	t.Parallel()

	t.Run("RecordsTunnel", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		addTunnel(ctx, t, env.dialTailnet(ctx, t, nil), ws.Agents[0].ID)
		intent := env.requireWakeIntent(t, ws.Workspace.ID, "tailnet")
		require.Equal(t, ws.Agents[0].ID, intent.AgentID)
		// The recorder counts the request after the write returns.
		require.Eventually(t, func() bool {
			return wakeCounter(t, env.api.PrometheusRegistry, "tailnet", "recorded") == 1
		}, testutil.WaitLong, testutil.IntervalFast)
	})

	t.Run("UnmanagedNotRecorded", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.workspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		addTunnel(ctx, t, env.dialTailnet(ctx, t, nil), ws.Agents[0].ID)
		require.Eventually(t, func() bool {
			return wakeCounter(t, env.api.PrometheusRegistry, "tailnet", "skipped_unmanaged") == 1
		}, testutil.WaitLong, testutil.IntervalFast)
		_, ok := env.wakeIntent(t, ws.Workspace.ID)
		require.False(t, ok)
	})

	// Coder Connect subscribes to workspace updates and then opens tunnels
	// to every agent; it must never wake workspaces.
	t.Run("CoderConnectWorkspaceUpdates", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		client := env.dialTailnet(ctx, t, nil)
		updates, err := client.WorkspaceUpdates(ctx, &tailnetproto.WorkspaceUpdatesRequest{
			WorkspaceOwnerId: tailnet.UUIDToByteSlice(env.memberID),
		})
		require.NoError(t, err)
		_, err = updates.Recv()
		require.NoError(t, err)
		addTunnel(ctx, t, client, ws.Agents[0].ID)

		require.Eventually(t, func() bool {
			return wakeCounter(t, env.api.PrometheusRegistry, "tailnet", "skipped_connect") == 1
		}, testutil.WaitLong, testutil.IntervalFast)
		_, ok := env.wakeIntent(t, ws.Workspace.ID)
		require.False(t, ok)
		require.Zero(t, wakeCounter(t, env.api.PrometheusRegistry, "tailnet", "recorded"))
	})

	t.Run("CoderDesktopHeader", func(t *testing.T) {
		t.Parallel()
		env := newWakeEnv(t)
		ws := env.managedWorkspace(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		header := http.Header{}
		header.Set(codersdk.CoderDesktopTelemetryHeader, `{"device_id":"test","device_os":"macOS","coder_desktop_version":"0.0.1"}`)
		addTunnel(ctx, t, env.dialTailnet(ctx, t, header), ws.Agents[0].ID)
		require.Eventually(t, func() bool {
			return wakeCounter(t, env.api.PrometheusRegistry, "tailnet", "skipped_connect") == 1
		}, testutil.WaitLong, testutil.IntervalFast)
		_, ok := env.wakeIntent(t, ws.Workspace.ID)
		require.False(t, ok)
	})
}

func TestWakeIntentLegacyCoordinate(t *testing.T) {
	t.Parallel()
	env := newWakeEnv(t)
	ws := env.managedWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	u, err := env.member.URL.Parse(fmt.Sprintf("/api/v2/workspaceagents/%s/coordinate?version=2.0", ws.Agents[0].ID))
	require.NoError(t, err)
	header := http.Header{}
	header.Set(codersdk.SessionTokenHeader, env.member.SessionToken())
	//nolint:bodyclose // websocket.Dial owns the HTTP response body on success.
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil && resp != nil {
		err = codersdk.ReadBodyAsError(resp)
	}
	require.NoError(t, err)
	defer conn.Close(websocket.StatusNormalClosure, "done")

	env.requireWakeIntent(t, ws.Workspace.ID, "tailnet")
}

func TestWakeIntentPTY(t *testing.T) {
	t.Parallel()
	env := newWakeEnv(t)
	ws := env.managedWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	// The agent is offline, as it is while its workspace sleeps, so the
	// terminal is refused; the request still records a wake intent.
	conn, err := workspacesdk.New(env.member).AgentReconnectingPTY(ctx, workspacesdk.WorkspaceAgentReconnectingPTYOpts{
		AgentID:   ws.Agents[0].ID,
		Reconnect: uuid.New(),
		Width:     80,
		Height:    24,
	})
	if err == nil {
		_ = conn.Close()
	}
	env.requireWakeIntent(t, ws.Workspace.ID, "pty")
}

func TestWakeIntentApp(t *testing.T) {
	t.Parallel()
	env := newWakeEnv(t)
	ws := env.managedWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	member, err := env.member.User(ctx, codersdk.Me)
	require.NoError(t, err)
	res, err := env.member.Request(ctx, http.MethodGet,
		fmt.Sprintf("/@%s/%s.%s/apps/app/", member.Username, ws.Workspace.Name, ws.Agents[0].Name), nil)
	require.NoError(t, err)
	_ = res.Body.Close()
	// The agent is offline, so the app is unavailable, but the request
	// records a wake intent.
	require.NotEqual(t, http.StatusOK, res.StatusCode)
	env.requireWakeIntent(t, ws.Workspace.ID, "app")
}

func TestWakeIntentAppOtherUser(t *testing.T) {
	t.Parallel()
	env := newWakeEnv(t)
	ws := env.managedWorkspace(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	other, _ := coderdtest.CreateAnotherUser(t, env.owner, env.orgID)

	// A user without access to the app records nothing.
	member, err := env.member.User(ctx, codersdk.Me)
	require.NoError(t, err)
	res, err := other.Request(ctx, http.MethodGet,
		fmt.Sprintf("/@%s/%s.%s/apps/app/", member.Username, ws.Workspace.Name, ws.Agents[0].Name), nil)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.NotEqual(t, http.StatusOK, res.StatusCode)

	// A request of the owner afterwards is the first one recorded.
	res, err = env.member.Request(ctx, http.MethodGet,
		fmt.Sprintf("/@%s/%s.%s/apps/app/", member.Username, ws.Workspace.Name, ws.Agents[0].Name), nil)
	require.NoError(t, err)
	_ = res.Body.Close()
	intent := env.requireWakeIntent(t, ws.Workspace.ID, "app")
	require.EqualValues(t, 1, intent.RequestCount)
	require.Zero(t, wakeCounter(t, env.api.PrometheusRegistry, "app", "debounced"))
}

func TestWakeIntentExplicit(t *testing.T) {
	t.Parallel()
	env := newWakeEnv(t)
	ctx := testutil.Context(t, testutil.WaitLong)

	// The agent of a managed workspace is not connected, as while its
	// microVM sleeps. The intent is written before the answer.
	ws := env.managedWorkspace(t)
	resp, err := env.member.WorkspaceAgentWake(ctx, ws.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceAgentWakeRequested, resp.Result)
	intent, ok := env.wakeIntent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.Equal(t, "cli", intent.Source)
	require.Equal(t, ws.Agents[0].ID, intent.AgentID)
	require.Equal(t, uuid.NullUUID{UUID: env.memberID, Valid: true}, intent.RequestedBy)
	// Explicit requests are never debounced.
	resp, err = env.member.WorkspaceAgentWake(ctx, ws.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceAgentWakeRequested, resp.Result)
	intent, ok = env.wakeIntent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.EqualValues(t, 2, intent.RequestCount)

	// Unmanaged workspaces never sleep.
	unmanaged := env.workspace(t)
	resp, err = env.member.WorkspaceAgentWake(ctx, unmanaged.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceAgentWakeNotManaged, resp.Result)
	_, ok = env.wakeIntent(t, unmanaged.Workspace.ID)
	require.False(t, ok)

	// A connected agent without a reported sleep needs no wake.
	running := env.managedWorkspace(t)
	now := dbtime.Now()
	require.NoError(t, env.db.UpdateWorkspaceAgentConnectionByID(ctx, database.UpdateWorkspaceAgentConnectionByIDParams{
		ID:               running.Agents[0].ID,
		FirstConnectedAt: sql.NullTime{Time: now, Valid: true},
		LastConnectedAt:  sql.NullTime{Time: now, Valid: true},
		UpdatedAt:        now,
	}))
	resp, err = env.member.WorkspaceAgentWake(ctx, running.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceAgentWakeAwake, resp.Result)
	_, ok = env.wakeIntent(t, running.Workspace.ID)
	require.False(t, ok)

	// A stopped workspace is never woken; it has to be started.
	stopped := env.managedWorkspace(t)
	dbfake.WorkspaceBuild(t, env.db, stopped.Workspace).Seed(database.WorkspaceBuild{
		BuildNumber: 2,
		Transition:  database.WorkspaceTransitionStop,
	}).Do()
	resp, err = env.member.WorkspaceAgentWake(ctx, stopped.Agents[0].ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.WorkspaceAgentWakeNotStarted, resp.Result)

	// Users who may not connect to the agent learn nothing.
	other, _ := coderdtest.CreateAnotherUser(t, env.owner, env.orgID)
	_, err = other.WorkspaceAgentWake(ctx, ws.Agents[0].ID)
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusNotFound, sdkErr.StatusCode())
}
