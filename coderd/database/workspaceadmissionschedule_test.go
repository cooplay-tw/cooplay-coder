package database_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbfake"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/testutil"
)

// scheduledWorkspace creates a workspace with a TTL whose two builds have a
// deadline, on a template with user autostop and a default TTL.
func scheduledWorkspace(t *testing.T, db database.Store) (template database.Template, ws database.WorkspaceTable, oldBuild, latestBuild database.WorkspaceBuild) {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	deadline := dbtime.Now().Add(time.Hour)
	first := dbfake.WorkspaceBuild(t, db, database.WorkspaceTable{
		OrganizationID: org.ID,
		OwnerID:        owner.ID,
		Ttl:            sql.NullInt64{Int64: int64(2 * time.Hour), Valid: true},
	}).Seed(database.WorkspaceBuild{Deadline: deadline}).Do()
	second := dbfake.WorkspaceBuild(t, db, first.Workspace).Seed(database.WorkspaceBuild{
		BuildNumber: 2,
		Deadline:    deadline,
	}).Do()
	template, err := db.GetTemplateByID(ctx, first.Workspace.TemplateID)
	require.NoError(t, err)
	require.NoError(t, db.UpdateTemplateScheduleByID(ctx, database.UpdateTemplateScheduleByIDParams{
		ID:                       template.ID,
		UpdatedAt:                dbtime.Now(),
		AllowUserAutostart:       true,
		AllowUserAutostop:        true,
		DefaultTTL:               int64(time.Hour),
		AutostopRequirementWeeks: template.AutostopRequirementWeeks,
	}))
	return template, first.Workspace, first.Build, second.Build
}

func TestWorkspaceAdmissionTemplateSchedule(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	template, ws, oldBuild, latestBuild := scheduledWorkspace(t, db)
	disable := func() (templates, workspaces, builds int64) {
		t.Helper()
		now := dbtime.Now()
		var err error
		templates, err = db.DisableWorkspaceAdmissionTemplateAutostop(ctx, database.DisableWorkspaceAdmissionTemplateAutostopParams{Now: now, TemplateID: template.ID})
		require.NoError(t, err)
		workspaces, err = db.ClearWorkspaceAdmissionWorkspaceTTLs(ctx, template.ID)
		require.NoError(t, err)
		builds, err = db.ClearWorkspaceAdmissionBuildDeadlines(ctx, database.ClearWorkspaceAdmissionBuildDeadlinesParams{Now: now, TemplateID: template.ID})
		require.NoError(t, err)
		return templates, workspaces, builds
	}

	// Templates that are not managed are left alone.
	templates, workspaces, builds := disable()
	require.Zero(t, templates+workspaces+builds)

	_, err := db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: admissionProfileParameter,
		Now:              dbtime.Now(),
		TemplateID:       template.ID,
	})
	require.NoError(t, err)
	templates, workspaces, builds = disable()
	require.EqualValues(t, 1, templates)
	require.EqualValues(t, 1, workspaces)
	require.EqualValues(t, 1, builds)

	got, err := db.GetTemplateByID(ctx, template.ID)
	require.NoError(t, err)
	require.False(t, got.AllowUserAutostop)
	require.Zero(t, got.DefaultTTL)
	gotWS, err := db.GetWorkspaceByID(ctx, ws.ID)
	require.NoError(t, err)
	require.False(t, gotWS.Ttl.Valid)
	latest, err := db.GetWorkspaceBuildByID(ctx, latestBuild.ID)
	require.NoError(t, err)
	require.True(t, latest.Deadline.IsZero())
	// Only the latest build matters to the lifecycle executor; older
	// builds keep their history.
	old, err := db.GetWorkspaceBuildByID(ctx, oldBuild.ID)
	require.NoError(t, err)
	require.False(t, old.Deadline.IsZero())

	// Nothing is left to change.
	templates, workspaces, builds = disable()
	require.Zero(t, templates+workspaces+builds)
}
