package wakeintent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/coderd/workspaceadmission/wakeintent"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

const metricName = "coderd_workspace_wake_intents_total"

type recorderEnv struct {
	db       database.Store
	sqlDB    *sql.DB
	ps       pubsub.Pubsub
	clock    *quartz.Mock
	registry *prometheus.Registry
	recorder *wakeintent.Recorder
}

// newRecorderEnv starts a recorder on a dbauthz-wrapped store, so the tests
// also prove that the recorder subject has the permissions it needs.
func newRecorderEnv(t *testing.T) recorderEnv {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	db, ps, sqlDB := dbtestutil.NewDBWithSQLDB(t)
	clock := quartz.NewMock(t)
	clock.Set(dbtime.Now())
	registry := prometheus.NewRegistry()

	acs := &atomic.Pointer[dbauthz.AccessControlStore]{}
	var tacs dbauthz.AccessControlStore = dbauthz.AGPLTemplateAccessControlStore{}
	acs.Store(&tacs)
	authzDB := dbauthz.New(db, rbac.NewStrictCachingAuthorizer(prometheus.NewRegistry()), testutil.Logger(t), acs)

	trap := clock.Trap().NewTicker("wakeintent", "flush")
	defer trap.Close()
	recorder := wakeintent.New(wakeintent.Options{
		Logger:                         testutil.Logger(t),
		Database:                       authzDB,
		Pubsub:                         ps,
		Registerer:                     registry,
		Clock:                          clock,
		AgentInactiveDisconnectTimeout: time.Hour,
	})
	t.Cleanup(recorder.Close)
	trap.MustWait(ctx).MustRelease(ctx)
	return recorderEnv{db: db, sqlDB: sqlDB, ps: ps, clock: clock, registry: registry, recorder: recorder}
}

// workspace creates a workspace with one agent whose latest build is a
// succeeded START. Its template is not managed.
func (e recorderEnv) workspace(t *testing.T) dbfake.WorkspaceResponse {
	t.Helper()
	org := dbgen.Organization(t, e.db, database.Organization{})
	owner := dbgen.User(t, e.db, database.User{})
	return dbfake.WorkspaceBuild(t, e.db, database.WorkspaceTable{
		OrganizationID: org.ID,
		OwnerID:        owner.ID,
	}).WithAgent().Do()
}

// manage marks the template of ws as managed by workspace admission.
func (e recorderEnv) manage(t *testing.T, ws dbfake.WorkspaceResponse) {
	t.Helper()
	admissiontest.Manage(t, e.db, ws.Workspace.TemplateID, admissiontest.ProfileParameter)
}

// managedWorkspace creates a workspace like workspace on a managed template.
func (e recorderEnv) managedWorkspace(t *testing.T) dbfake.WorkspaceResponse {
	t.Helper()
	ws := e.workspace(t)
	e.manage(t, ws)
	return ws
}

func (e recorderEnv) connectAgent(t *testing.T, agentID uuid.UUID) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	now := e.clock.Now()
	require.NoError(t, e.db.UpdateWorkspaceAgentConnectionByID(ctx, database.UpdateWorkspaceAgentConnectionByIDParams{
		ID:               agentID,
		FirstConnectedAt: sql.NullTime{Time: now, Valid: true},
		LastConnectedAt:  sql.NullTime{Time: now, Valid: true},
		UpdatedAt:        now,
	}))
}

func (e recorderEnv) setPowerState(t *testing.T, ws database.WorkspaceTable, state string) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	_, err := e.sqlDB.ExecContext(ctx, `INSERT INTO workspace_admission_workspace_states
		(workspace_id, organization_id, power_state, power_epoch, generation, since, updated_at)
		VALUES ($1, $2, $3, 1, 1, NOW(), NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET power_state = EXCLUDED.power_state`,
		ws.ID, ws.OrganizationID, state)
	require.NoError(t, err)
}

func (e recorderEnv) intent(t *testing.T, workspaceID uuid.UUID) (database.WorkspaceAdmissionWakeIntent, bool) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	rows, err := e.db.GetWorkspaceAdmissionWakeIntents(ctx, database.GetWorkspaceAdmissionWakeIntentsParams{LimitCount: 1000})
	require.NoError(t, err)
	for _, row := range rows {
		if row.WorkspaceID == workspaceID {
			return row, true
		}
	}
	return database.WorkspaceAdmissionWakeIntent{}, false
}

// counter returns the value of the wake intent counter for one source and
// result, or zero when it was never incremented.
func (e recorderEnv) counter(t *testing.T, source wakeintent.Source, result string) float64 {
	t.Helper()
	families, err := e.registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["source"] == string(source) && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func (e recorderEnv) waitCounter(t *testing.T, source wakeintent.Source, result string, want float64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return e.counter(t, source, result) == want
	}, testutil.WaitShort, testutil.IntervalFast, "counter source=%s result=%s", source, result)
}

func TestRecorderWritesFirstIntentImmediately(t *testing.T) {
	t.Parallel()
	env := newRecorderEnv(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	ws := env.managedWorkspace(t)
	userID := ws.Workspace.OwnerID

	events := make(chan workspaceadmission.Event, 10)
	cancel, err := env.ps.Subscribe(workspaceadmission.EventChannel, func(_ context.Context, msg []byte) {
		var ev workspaceadmission.Event
		if json.Unmarshal(msg, &ev) == nil {
			events <- ev
		}
	})
	require.NoError(t, err)
	defer cancel()

	requestedAt := env.clock.Now()
	env.recorder.Enqueue(ws.Agents[0].ID, userID, wakeintent.SourcePTY)
	env.waitCounter(t, wakeintent.SourcePTY, wakeintent.ResultRecorded, 1)

	// No clock advance was needed: the first request is not debounced.
	intent, ok := env.intent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.Equal(t, ws.Agents[0].ID, intent.AgentID)
	require.Equal(t, "pty", intent.Source)
	require.Equal(t, uuid.NullUUID{UUID: userID, Valid: true}, intent.RequestedBy)
	require.EqualValues(t, 1, intent.RequestCount)
	require.True(t, intent.FirstRequestedAt.Equal(dbtime.Time(requestedAt)))
	require.False(t, intent.AckedAt.Valid)

	ev := testutil.TryReceive(ctx, t, events)
	require.Equal(t, workspaceadmission.EventKindWakeIntent, ev.Kind)
	require.NotNil(t, ev.WorkspaceID)
	require.Equal(t, ws.Workspace.ID, *ev.WorkspaceID)
}

func TestRecorderDebouncesRepeats(t *testing.T) {
	t.Parallel()
	env := newRecorderEnv(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	ws := env.managedWorkspace(t)
	agentID := ws.Agents[0].ID

	env.recorder.Enqueue(agentID, uuid.Nil, wakeintent.SourceTailnet)
	env.recorder.Enqueue(agentID, uuid.Nil, wakeintent.SourceTailnet)
	env.recorder.Enqueue(agentID, ws.Workspace.OwnerID, wakeintent.SourceApp)
	env.waitCounter(t, wakeintent.SourceTailnet, wakeintent.ResultRecorded, 1)
	env.waitCounter(t, wakeintent.SourceTailnet, wakeintent.ResultDebounced, 1)
	env.waitCounter(t, wakeintent.SourceApp, wakeintent.ResultDebounced, 1)

	first, ok := env.intent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.EqualValues(t, 1, first.RequestCount)
	require.Equal(t, "tailnet", first.Source)
	require.False(t, first.RequestedBy.Valid)

	// The window ends: the coalesced repeats are written together.
	env.clock.Advance(wakeintent.DebounceWindow).MustWait(ctx)
	require.Eventually(t, func() bool {
		intent, ok := env.intent(t, ws.Workspace.ID)
		return ok && intent.RequestCount == 3
	}, testutil.WaitShort, testutil.IntervalFast)
	flushed, _ := env.intent(t, ws.Workspace.ID)
	require.Equal(t, "app", flushed.Source)
	require.Equal(t, uuid.NullUUID{UUID: ws.Workspace.OwnerID, Valid: true}, flushed.RequestedBy)
	require.True(t, flushed.LastRequestedAt.After(first.LastRequestedAt))
	require.True(t, flushed.FirstRequestedAt.Equal(first.FirstRequestedAt))

	// A quiet window closes the debounce; the next request is written at
	// once again.
	env.clock.Advance(wakeintent.DebounceWindow).MustWait(ctx)
	env.clock.Advance(wakeintent.DebounceWindow).MustWait(ctx)
	env.recorder.Enqueue(agentID, uuid.Nil, wakeintent.SourceCLI)
	env.waitCounter(t, wakeintent.SourceCLI, wakeintent.ResultRecorded, 1)
	latest, _ := env.intent(t, ws.Workspace.ID)
	require.EqualValues(t, 4, latest.RequestCount)
	require.Equal(t, "cli", latest.Source)
}

func TestRecorderSkips(t *testing.T) {
	t.Parallel()

	type setup func(t *testing.T, env recorderEnv, ws dbfake.WorkspaceResponse)
	connected := func(t *testing.T, env recorderEnv, ws dbfake.WorkspaceResponse) {
		env.connectAgent(t, ws.Agents[0].ID)
	}
	withState := func(state string, connect bool) setup {
		return func(t *testing.T, env recorderEnv, ws dbfake.WorkspaceResponse) {
			if connect {
				env.connectAgent(t, ws.Agents[0].ID)
			}
			env.setPowerState(t, ws.Workspace, state)
		}
	}
	stopped := func(t *testing.T, env recorderEnv, ws dbfake.WorkspaceResponse) {
		dbfake.WorkspaceBuild(t, env.db, ws.Workspace).Seed(database.WorkspaceBuild{
			Transition:  database.WorkspaceTransitionStop,
			BuildNumber: 2,
		}).Do()
	}

	for _, tc := range []struct {
		name    string
		managed bool
		setup   setup
		result  string
	}{
		{name: "UnmanagedTemplate", managed: false, result: wakeintent.ResultSkippedUnmanaged},
		{name: "Stopped", managed: true, setup: stopped, result: wakeintent.ResultSkippedStopped},
		{name: "ConnectedWithoutPowerState", managed: true, setup: connected, result: wakeintent.ResultSkippedRunning},
		{name: "ConnectedRunning", managed: true, setup: withState("running", true), result: wakeintent.ResultSkippedRunning},
		{name: "ConnectedReduced", managed: true, setup: withState("reduced", true), result: wakeintent.ResultSkippedRunning},
		{name: "DisconnectedWithoutPowerState", managed: true, result: wakeintent.ResultRecorded},
		{name: "DisconnectedRunning", managed: true, setup: withState("running", false), result: wakeintent.ResultRecorded},
		// coderd may still show a suspended agent as connected.
		{name: "ConnectedHibernated", managed: true, setup: withState("hibernated", true), result: wakeintent.ResultRecorded},
		{name: "ConnectedPaused", managed: true, setup: withState("paused", true), result: wakeintent.ResultRecorded},
		{name: "ConnectedSleeping", managed: true, setup: withState("sleeping", true), result: wakeintent.ResultRecorded},
		{name: "ConnectedUnknown", managed: true, setup: withState("unknown", true), result: wakeintent.ResultRecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newRecorderEnv(t)
			ws := env.workspace(t)
			if tc.managed {
				env.manage(t, ws)
			}
			if tc.setup != nil {
				tc.setup(t, env, ws)
			}
			env.recorder.Enqueue(ws.Agents[0].ID, ws.Workspace.OwnerID, wakeintent.SourceTailnet)
			env.waitCounter(t, wakeintent.SourceTailnet, tc.result, 1)
			_, ok := env.intent(t, ws.Workspace.ID)
			require.Equal(t, tc.result == wakeintent.ResultRecorded, ok)
		})
	}

	t.Run("UnknownAgent", func(t *testing.T) {
		t.Parallel()
		env := newRecorderEnv(t)
		env.recorder.Enqueue(uuid.New(), uuid.Nil, wakeintent.SourceApp)
		env.waitCounter(t, wakeintent.SourceApp, wakeintent.ResultSkippedUnmanaged, 1)
	})
}

func TestRecorderSkipConnect(t *testing.T) {
	t.Parallel()
	env := newRecorderEnv(t)
	env.recorder.SkipConnect(wakeintent.SourceTailnet)
	require.EqualValues(t, 1, env.counter(t, wakeintent.SourceTailnet, wakeintent.ResultSkippedConnect))
}

// TestRecorderEnqueueNeverBlocks checks that Enqueue drops requests instead
// of blocking when the worker cannot keep up, which keeps the tailnet
// coordinator mutex free.
func TestRecorderEnqueueNeverBlocks(t *testing.T) {
	t.Parallel()
	env := newRecorderEnv(t)
	// A stopped worker never drains the queue.
	env.recorder.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range wakeintent.QueueSize + 5 {
			env.recorder.Enqueue(uuid.New(), uuid.Nil, wakeintent.SourceTailnet)
		}
	}()
	ctx := testutil.Context(t, testutil.WaitShort)
	testutil.TryReceive(ctx, t, done)
	require.EqualValues(t, 5, env.counter(t, wakeintent.SourceTailnet, wakeintent.ResultDropped))
}

func TestNilRecorder(t *testing.T) {
	t.Parallel()
	var recorder *wakeintent.Recorder
	recorder.Enqueue(uuid.New(), uuid.New(), wakeintent.SourceTailnet)
	recorder.SkipConnect(wakeintent.SourceTailnet)
	require.Equal(t, wakeintent.ResultFailed, recorder.Record(uuid.New(), uuid.New(), wakeintent.SourceCLI))
	recorder.Close()
}

func TestRecorderRecord(t *testing.T) {
	t.Parallel()
	env := newRecorderEnv(t)
	ws := env.managedWorkspace(t)
	userID := ws.Workspace.OwnerID

	// An explicit request is written before Record returns and is never
	// debounced.
	require.Equal(t, wakeintent.ResultRecorded, env.recorder.Record(ws.Agents[0].ID, userID, wakeintent.SourceCLI))
	intent, ok := env.intent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.Equal(t, "cli", intent.Source)
	require.Equal(t, uuid.NullUUID{UUID: userID, Valid: true}, intent.RequestedBy)
	require.EqualValues(t, 1, intent.RequestCount)
	require.Equal(t, wakeintent.ResultRecorded, env.recorder.Record(ws.Agents[0].ID, userID, wakeintent.SourceCLI))
	intent, ok = env.intent(t, ws.Workspace.ID)
	require.True(t, ok)
	require.EqualValues(t, 2, intent.RequestCount)
	require.EqualValues(t, 2, env.counter(t, wakeintent.SourceCLI, wakeintent.ResultRecorded))

	// The same skips as for queued requests.
	unmanaged := env.workspace(t)
	require.Equal(t, wakeintent.ResultSkippedUnmanaged, env.recorder.Record(unmanaged.Agents[0].ID, userID, wakeintent.SourceCLI))
	running := env.managedWorkspace(t)
	env.connectAgent(t, running.Agents[0].ID)
	require.Equal(t, wakeintent.ResultSkippedRunning, env.recorder.Record(running.Agents[0].ID, userID, wakeintent.SourceCLI))
	env.setPowerState(t, running.Workspace, "hibernated")
	require.Equal(t, wakeintent.ResultRecorded, env.recorder.Record(running.Agents[0].ID, userID, wakeintent.SourceCLI))
	require.EqualValues(t, 1, env.counter(t, wakeintent.SourceCLI, wakeintent.ResultSkippedUnmanaged))
	require.EqualValues(t, 1, env.counter(t, wakeintent.SourceCLI, wakeintent.ResultSkippedRunning))
}
