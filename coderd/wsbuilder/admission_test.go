package wsbuilder_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/coderd/wsbuilder"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

type managedEnv struct {
	db       database.Store
	ps       pubsub.Pubsub
	owner    *codersdk.Client
	orgID    uuid.UUID
	member   *codersdk.Client
	memberID uuid.UUID
	template codersdk.Template
}

// newManagedEnv starts coderd with a provisioner and a template managed by
// workspace admission. db is the raw store so tests can grant admissions.
func newManagedEnv(t *testing.T) managedEnv {
	t.Helper()
	db, ps := dbtestutil.NewDB(t)
	owner := coderdtest.New(t, &coderdtest.Options{
		Database:                 db,
		Pubsub:                   ps,
		IncludeProvisionerDaemon: true,
	})
	first := coderdtest.CreateFirstUser(t, owner)
	member, memberUser := coderdtest.CreateAnotherUser(t, owner, first.OrganizationID)
	version := coderdtest.CreateTemplateVersion(t, owner, first.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, owner, version.ID)
	template := coderdtest.CreateTemplate(t, owner, first.OrganizationID, version.ID)
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	return managedEnv{
		db:       db,
		ps:       ps,
		owner:    owner,
		orgID:    first.OrganizationID,
		member:   member,
		memberID: memberUser.ID,
		template: template,
	}
}

func TestManagedTemplateStartBuilds(t *testing.T) {
	t.Parallel()

	t.Run("CreateCommitsWorkspaceBuildAndWaitingAdmission", func(t *testing.T) {
		t.Parallel()
		env := newManagedEnv(t)
		admissiontest.Entitle(t, env.db, env.memberID, env.template.ID, "small", "large")
		ctx := testutil.Context(t, testutil.WaitLong)

		workspace, err := env.member.CreateUserWorkspace(ctx, codersdk.Me, codersdk.CreateWorkspaceRequest{
			TemplateID:          env.template.ID,
			Name:                "managed",
			RichParameterValues: admissiontest.Profile("large"),
		})
		require.NoError(t, err)
		require.Equal(t, codersdk.ProvisionerJobPending, workspace.LatestBuild.Job.Status)

		admission := admissiontest.Admission(t, env.db, workspace.LatestBuild.Job.ID)
		require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
		require.Equal(t, workspaceadmission.ReasonWaitingForCapacity, admission.Reason)
		require.Equal(t, "large", admission.Profile)
		require.Equal(t, workspace.ID, admission.WorkspaceID)
		require.Equal(t, workspace.LatestBuild.ID, admission.WorkspaceBuildID)
		require.Equal(t, env.memberID, admission.OwnerID)
		require.Equal(t, env.template.ID, admission.TemplateID)
		require.Equal(t, workspace.LatestBuild.TemplateVersionID, admission.TemplateVersionID)

		// A capacity wait never takes a provisioner queue position.
		build, err := env.member.WorkspaceBuild(ctx, workspace.LatestBuild.ID)
		require.NoError(t, err)
		require.Zero(t, build.Job.QueuePosition)

		// Granting the admission lets the provisioner run the build.
		admissiontest.Grant(t, env.db, env.ps, workspace.LatestBuild.Job.ID)
		build = coderdtest.AwaitWorkspaceBuildJobCompleted(t, env.member, workspace.LatestBuild.ID)
		require.Equal(t, codersdk.ProvisionerJobSucceeded, build.Job.Status)
		admission = admissiontest.Admission(t, env.db, workspace.LatestBuild.Job.ID)
		require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admission.Status)

		// STOP never needs admission.
		stop, err := env.member.CreateWorkspaceBuild(ctx, workspace.ID, codersdk.CreateWorkspaceBuildRequest{
			Transition: codersdk.WorkspaceTransitionStop,
		})
		require.NoError(t, err)
		admissiontest.RequireNoAdmission(t, env.db, stop.Job.ID)
		coderdtest.AwaitWorkspaceBuildJobCompleted(t, env.member, stop.ID)

		// A later START, with a template version update, waits again.
		version := coderdtest.CreateTemplateVersion(t, env.owner, env.orgID, admissiontest.ProfileResponses(), func(req *codersdk.CreateTemplateVersionRequest) {
			req.TemplateID = env.template.ID
		})
		coderdtest.AwaitTemplateVersionJobCompleted(t, env.owner, version.ID)
		start, err := env.member.CreateWorkspaceBuild(ctx, workspace.ID, codersdk.CreateWorkspaceBuildRequest{
			Transition:        codersdk.WorkspaceTransitionStart,
			TemplateVersionID: version.ID,
		})
		require.NoError(t, err)
		admission = admissiontest.Admission(t, env.db, start.Job.ID)
		require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
		require.Equal(t, version.ID, admission.TemplateVersionID)
		require.Equal(t, "large", admission.Profile, "the profile comes from the previous build")
	})

	t.Run("RestartQueuesWaitingStart", func(t *testing.T) {
		t.Parallel()
		env := newManagedEnv(t)
		admissiontest.Entitle(t, env.db, env.memberID, env.template.ID, "small")
		ctx := testutil.Context(t, testutil.WaitLong)

		workspace := coderdtest.CreateWorkspace(t, env.member, env.template.ID, func(req *codersdk.CreateWorkspaceRequest) {
			req.RichParameterValues = admissiontest.Profile("small")
		})
		admissiontest.Grant(t, env.db, env.ps, workspace.LatestBuild.Job.ID)
		coderdtest.AwaitWorkspaceBuildJobCompleted(t, env.member, workspace.LatestBuild.ID)

		stop, err := env.member.CreateWorkspaceBuild(ctx, workspace.ID, codersdk.CreateWorkspaceBuildRequest{
			Transition: codersdk.WorkspaceTransitionStop,
			OnSuccess: &codersdk.CreateWorkspaceBuildOnSuccessRequest{
				Transition: codersdk.WorkspaceTransitionStart,
			},
		})
		require.NoError(t, err)
		coderdtest.AwaitWorkspaceBuildJobCompleted(t, env.member, stop.ID)

		var restart codersdk.WorkspaceBuild
		testutil.Eventually(ctx, t, func(ctx context.Context) bool {
			ws, err := env.member.Workspace(ctx, workspace.ID)
			if err != nil || ws.LatestBuild.BuildNumber <= stop.BuildNumber {
				return false
			}
			restart = ws.LatestBuild
			return true
		}, testutil.IntervalFast)
		require.Equal(t, codersdk.WorkspaceTransitionStart, restart.Transition)
		admission := admissiontest.Admission(t, env.db, restart.Job.ID)
		require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
	})

	t.Run("NotEntitled", func(t *testing.T) {
		t.Parallel()
		env := newManagedEnv(t)
		ctx := testutil.Context(t, testutil.WaitLong)

		_, err := env.member.CreateUserWorkspace(ctx, codersdk.Me, codersdk.CreateWorkspaceRequest{
			TemplateID:          env.template.ID,
			Name:                "denied",
			RichParameterValues: admissiontest.Profile("small"),
		})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())

		// The policy rejection rolls back the workspace.
		workspaces, err := env.member.Workspaces(ctx, codersdk.WorkspaceFilter{Owner: codersdk.Me})
		require.NoError(t, err)
		require.Empty(t, workspaces.Workspaces)
	})

	t.Run("ProfileNotEntitled", func(t *testing.T) {
		t.Parallel()
		env := newManagedEnv(t)
		admissiontest.Entitle(t, env.db, env.memberID, env.template.ID, "small")
		ctx := testutil.Context(t, testutil.WaitLong)

		_, err := env.member.CreateUserWorkspace(ctx, codersdk.Me, codersdk.CreateWorkspaceRequest{
			TemplateID:          env.template.ID,
			Name:                "toolarge",
			RichParameterValues: admissiontest.Profile("large"),
		})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())
	})

	t.Run("MissingProfileParameter", func(t *testing.T) {
		t.Parallel()
		env := newManagedEnv(t)
		admissiontest.Manage(t, env.db, env.template.ID, "missing_parameter")
		admissiontest.Entitle(t, env.db, env.memberID, env.template.ID, "small")
		ctx := testutil.Context(t, testutil.WaitLong)

		_, err := env.member.CreateUserWorkspace(ctx, codersdk.Me, codersdk.CreateWorkspaceRequest{
			TemplateID:          env.template.ID,
			Name:                "noprofile",
			RichParameterValues: admissiontest.Profile("small"),
		})
		var sdkErr *codersdk.Error
		require.ErrorAs(t, err, &sdkErr)
		require.Equal(t, http.StatusBadRequest, sdkErr.StatusCode())
		require.Contains(t, sdkErr.Message, "missing_parameter")
	})

	t.Run("PrebuildRejected", func(t *testing.T) {
		t.Parallel()
		db, ps := dbtestutil.NewDB(t)
		client, _, api := coderdtest.NewWithAPI(t, &coderdtest.Options{
			Database:                 db,
			Pubsub:                   ps,
			IncludeProvisionerDaemon: true,
		})
		first := coderdtest.CreateFirstUser(t, client)
		version := coderdtest.CreateTemplateVersion(t, client, first.OrganizationID, admissiontest.ProfileResponses())
		coderdtest.AwaitTemplateVersionJobCompleted(t, client, version.ID)
		template := coderdtest.CreateTemplate(t, client, first.OrganizationID, version.ID)
		admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
		ctx := testutil.Context(t, testutil.WaitLong)

		workspace, err := db.InsertWorkspace(ctx, database.InsertWorkspaceParams{
			ID:               uuid.New(),
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
			OwnerID:          database.PrebuildsSystemUserID,
			OrganizationID:   first.OrganizationID,
			TemplateID:       template.ID,
			Name:             "prebuild",
			AutomaticUpdates: database.AutomaticUpdatesNever,
			LastUsedAt:       time.Now(),
		})
		require.NoError(t, err)
		full, err := db.GetWorkspaceByID(ctx, workspace.ID)
		require.NoError(t, err)

		builder := wsbuilder.New(full, database.WorkspaceTransitionStart, wsbuilder.NoopUsageChecker{}).
			ActiveVersion().
			MarkPrebuild()
		_, _, _, err = builder.Build(dbauthz.AsSystemRestricted(ctx), api.Database, api.FileCache, nil, audit.WorkspaceBuildBaggage{})
		var buildErr wsbuilder.BuildError
		require.ErrorAs(t, err, &buildErr)
		require.Equal(t, http.StatusBadRequest, buildErr.Status)
		require.Contains(t, buildErr.Message, "Prebuilt workspaces are not supported")

		_, err = db.GetLatestWorkspaceBuildByWorkspaceID(ctx, workspace.ID)
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}
