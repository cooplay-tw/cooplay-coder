package admissionapi_test

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// TestPutTemplateDisablesAutostop checks that managing a template turns off
// every schedule that could stop the build of one of its workspaces.
func TestPutTemplateDisablesAutostop(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	_, err := env.owner.UpdateTemplateMeta(ctx, env.unmanaged.ID, codersdk.UpdateTemplateMeta{
		DefaultTTLMillis: ptr.Ref(time.Hour.Milliseconds()),
	})
	require.NoError(t, err)
	workspace := coderdtest.CreateWorkspace(t, env.owner, env.unmanaged.ID, func(req *codersdk.CreateWorkspaceRequest) {
		req.TTLMillis = ptr.Ref((2 * time.Hour).Milliseconds())
	})
	deadline := dbtime.Now().Add(time.Hour)
	require.NoError(t, env.db.UpdateWorkspaceBuildDeadlineByID(ctx, database.UpdateWorkspaceBuildDeadlineByIDParams{
		ID:        workspace.LatestBuild.ID,
		Deadline:  deadline,
		UpdatedAt: dbtime.Now(),
	}))
	before, err := env.db.GetTemplateByID(ctx, env.unmanaged.ID)
	require.NoError(t, err)
	require.True(t, before.AllowUserAutostop)
	require.Equal(t, time.Hour.Nanoseconds(), before.DefaultTTL)

	var tpl admissionapi.Template
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/templates/"+env.unmanaged.ID.String(), admissionapi.PutTemplateRequest{ProfileParameter: "profile"}, &tpl))

	after, err := env.db.GetTemplateByID(ctx, env.unmanaged.ID)
	require.NoError(t, err)
	require.False(t, after.AllowUserAutostop)
	require.Zero(t, after.DefaultTTL)
	ws, err := env.db.GetWorkspaceByID(ctx, workspace.ID)
	require.NoError(t, err)
	require.Equal(t, sql.NullInt64{}, ws.Ttl)
	build, err := env.db.GetWorkspaceBuildByID(ctx, workspace.LatestBuild.ID)
	require.NoError(t, err)
	require.True(t, build.Deadline.IsZero(), "deadline %v", build.Deadline)

	// The workspace API reports the template's TTL of zero and no
	// deadline.
	sdkWorkspace, err := env.owner.Workspace(ctx, workspace.ID)
	require.NoError(t, err)
	require.Zero(t, ptr.NilToEmpty(sdkWorkspace.TTLMillis))
	require.True(t, sdkWorkspace.LatestBuild.Deadline.IsZero())

	// Managing it again changes nothing.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/templates/"+env.unmanaged.ID.String(), admissionapi.PutTemplateRequest{ProfileParameter: "profile"}, &tpl))
	again, err := env.db.GetTemplateByID(ctx, env.unmanaged.ID)
	require.NoError(t, err)
	require.Equal(t, after.UpdatedAt, again.UpdatedAt)
}
