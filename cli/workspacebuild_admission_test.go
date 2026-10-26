package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/cli/clitest"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/coder/v2/testutil/expecter"
)

// TestWorkspaceBuildCapacityWait verifies that commands waiting on a build of
// a template managed by workspace admission report the capacity wait.
func TestWorkspaceBuildCapacityWait(t *testing.T) {
	t.Parallel()

	db, ps := dbtestutil.NewDB(t)
	client := coderdtest.New(t, &coderdtest.Options{
		IncludeProvisionerDaemon: true,
		Database:                 db,
		Pubsub:                   ps,
	})
	owner := coderdtest.CreateFirstUser(t, client)
	member, memberUser := coderdtest.CreateAnotherUser(t, client, owner.OrganizationID)
	version := coderdtest.CreateTemplateVersion(t, client, owner.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, client, version.ID)
	template := coderdtest.CreateTemplate(t, client, owner.OrganizationID, version.ID)
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	admissiontest.Entitle(t, db, memberUser.ID, template.ID, "small")

	create := func(t *testing.T, name string) (*expecter.Expecter, chan error) {
		t.Helper()
		inv, root := clitest.New(t, "create", name,
			"--template", template.Name,
			"--parameter", admissiontest.ProfileParameter+"=small",
			"-y",
		)
		clitest.SetupConfig(t, member, root)
		stdout := expecter.NewAttachedToInvocation(t, inv)
		done := make(chan error, 1)
		go func() {
			done <- inv.Run()
		}()
		return stdout, done
	}

	t.Run("Granted", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		stdout, done := create(t, "capacity-granted")
		stdout.ExpectMatch(ctx, "Waiting for capacity")

		workspace, err := member.WorkspaceByOwnerAndName(ctx, codersdk.Me, "capacity-granted", codersdk.WorkspaceOptions{})
		require.NoError(t, err)
		admissiontest.Grant(t, db, ps, workspace.LatestBuild.Job.ID)

		stdout.ExpectMatch(ctx, "has been created")
		require.NoError(t, testutil.RequireReceive(ctx, t, done))
	})

	t.Run("Rejected", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		stdout, done := create(t, "capacity-rejected")
		stdout.ExpectMatch(ctx, "Waiting for capacity")

		workspace, err := member.WorkspaceByOwnerAndName(ctx, codersdk.Me, "capacity-rejected", codersdk.WorkspaceOptions{})
		require.NoError(t, err)
		admissiontest.Reject(t, db, ps, workspace.LatestBuild.Job.ID, "quota: profile small is full")

		stdout.ExpectMatch(ctx, "Capacity request rejected: quota: profile small is full")
		err = testutil.RequireReceive(ctx, t, done)
		require.ErrorContains(t, err, "admission rejected: quota: profile small is full")
	})
}
