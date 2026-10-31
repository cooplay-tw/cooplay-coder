package coderd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// TestManagedTemplateSchedule checks that workspaces of managed templates
// never get a build deadline: the platform controller suspends them while
// their START build keeps running, and a deadline would make the lifecycle
// executor stop that build.
func TestManagedTemplateSchedule(t *testing.T) {
	t.Parallel()
	db, ps := dbtestutil.NewDB(t)
	owner := coderdtest.New(t, &coderdtest.Options{
		Database:                 db,
		Pubsub:                   ps,
		IncludeProvisionerDaemon: true,
	})
	first := coderdtest.CreateFirstUser(t, owner)
	member, memberUser := coderdtest.CreateAnotherUser(t, owner, first.OrganizationID)
	// Four builds and their API checks share this workflow context.
	ctx := testutil.Context(t, 2*testutil.WaitSuperLong)

	version := coderdtest.CreateTemplateVersion(t, owner, first.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, owner, version.ID)
	template := coderdtest.CreateTemplate(t, owner, first.OrganizationID, version.ID, func(req *codersdk.CreateTemplateRequest) {
		req.DefaultTTLMillis = ptr.Ref(time.Hour.Milliseconds())
		req.UseClassicParameterFlow = ptr.Ref(true)
	})
	require.Equal(t, time.Hour.Milliseconds(), template.DefaultTTLMillis)

	// completedBuild waits for the build job and then reads the build again:
	// the build in the response that first reports the job as completed can
	// predate the commit that set its deadline.
	completedBuild := func(buildID uuid.UUID) codersdk.WorkspaceBuild {
		t.Helper()
		coderdtest.AwaitWorkspaceBuildJobCompleted(t, member, buildID)
		build, err := member.WorkspaceBuild(ctx, buildID)
		require.NoError(t, err)
		return build
	}

	// Without admission, the workspace TTL gives builds a deadline.
	unmanaged := coderdtest.CreateWorkspace(t, member, template.ID, func(req *codersdk.CreateWorkspaceRequest) {
		req.RichParameterValues = admissiontest.Profile("small")
	})
	build := completedBuild(unmanaged.LatestBuild.ID)
	require.False(t, build.Deadline.IsZero())

	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	admissiontest.Entitle(t, db, memberUser.ID, template.ID, "small")

	// A workspace TTL requested at create is ignored.
	workspace := coderdtest.CreateWorkspace(t, member, template.ID, func(req *codersdk.CreateWorkspaceRequest) {
		req.RichParameterValues = admissiontest.Profile("small")
		req.TTLMillis = ptr.Ref((2 * time.Hour).Milliseconds())
	})
	admissiontest.Grant(t, db, ps, workspace.LatestBuild.Job.ID)
	build = completedBuild(workspace.LatestBuild.ID)
	require.Equal(t, codersdk.WorkspaceStatusRunning, build.Status)
	require.True(t, build.Deadline.IsZero(), "deadline %v", build.Deadline)

	// Users cannot set a TTL, and there is no deadline to extend.
	err := member.UpdateWorkspaceTTL(ctx, workspace.ID, codersdk.UpdateWorkspaceTTLRequest{
		TTLMillis: ptr.Ref(time.Hour.Milliseconds()),
	})
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusBadRequest, sdkErr.StatusCode())
	err = member.PutExtendWorkspace(ctx, workspace.ID, codersdk.PutExtendWorkspaceRequest{
		Deadline: time.Now().Add(time.Hour),
	})
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusConflict, sdkErr.StatusCode())

	// A new START of the workspace that existed before the template was
	// managed has no deadline either.
	restart := coderdtest.CreateWorkspaceBuild(t, member, unmanaged, database.WorkspaceTransitionStop)
	coderdtest.AwaitWorkspaceBuildJobCompleted(t, member, restart.ID)
	start, err := member.CreateWorkspaceBuild(ctx, unmanaged.ID, codersdk.CreateWorkspaceBuildRequest{
		Transition: codersdk.WorkspaceTransitionStart,
	})
	require.NoError(t, err)
	admissiontest.Grant(t, db, ps, start.Job.ID)
	build = completedBuild(start.ID)
	require.True(t, build.Deadline.IsZero(), "deadline %v", build.Deadline)
}
