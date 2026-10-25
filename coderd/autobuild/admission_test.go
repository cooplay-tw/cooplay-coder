package autobuild_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/autobuild"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// TestExecutorAutostartManagedTemplate verifies that autostart creates a
// START build that waits for workspace admission instead of reaching a
// provisioner directly.
func TestExecutorAutostartManagedTemplate(t *testing.T) {
	t.Parallel()

	var (
		db, ps  = dbtestutil.NewDB(t)
		sched   = mustSchedule(t, "CRON_TZ=UTC 0 * * * *")
		tickCh  = make(chan time.Time)
		statsCh = make(chan autobuild.Stats)
		client  = coderdtest.New(t, &coderdtest.Options{
			Database:                 db,
			Pubsub:                   ps,
			AutobuildTicker:          tickCh,
			IncludeProvisionerDaemon: true,
			AutobuildStats:           statsCh,
		})
		user    = coderdtest.CreateFirstUser(t, client)
		version = coderdtest.CreateTemplateVersion(t, client, user.OrganizationID, admissiontest.ProfileResponses())
	)
	coderdtest.AwaitTemplateVersionJobCompleted(t, client, version.ID)
	template := coderdtest.CreateTemplate(t, client, user.OrganizationID, version.ID)
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	admissiontest.Entitle(t, db, user.UserID, template.ID, "small")

	workspace := coderdtest.CreateWorkspace(t, client, template.ID, func(cwr *codersdk.CreateWorkspaceRequest) {
		cwr.AutostartSchedule = ptr.Ref(sched.String())
		cwr.RichParameterValues = admissiontest.Profile("small")
	})
	admissiontest.Grant(t, db, ps, workspace.LatestBuild.Job.ID)
	coderdtest.AwaitWorkspaceBuildJobCompleted(t, client, workspace.LatestBuild.ID)
	workspace = coderdtest.MustTransitionWorkspace(t, client, workspace.ID, codersdk.WorkspaceTransitionStart, codersdk.WorkspaceTransitionStop)

	p, err := coderdtest.GetProvisionerForTags(db, time.Now(), workspace.OrganizationID, map[string]string{})
	require.NoError(t, err)
	tickTime := coderdtest.NextAutostartTick(t, workspace)
	ctx := testutil.Context(t, testutil.WaitLong)
	coderdtest.UpdateProvisionerLastSeenAt(t, db, p.ID, tickTime)
	testutil.RequireSend(ctx, t, tickCh, tickTime)
	stats := testutil.RequireReceive(ctx, t, statsCh)
	close(tickCh)
	require.Empty(t, stats.Errors)
	require.Equal(t, database.WorkspaceTransitionStart, stats.Transitions[workspace.ID])

	workspace = coderdtest.MustWorkspace(t, client, workspace.ID)
	require.Equal(t, codersdk.BuildReasonAutostart, workspace.LatestBuild.Reason)
	require.Equal(t, codersdk.ProvisionerJobPending, workspace.LatestBuild.Job.Status)
	admission := admissiontest.Admission(t, db, workspace.LatestBuild.Job.ID)
	require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
	require.Equal(t, "small", admission.Profile)
}
