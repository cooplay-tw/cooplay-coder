package provisionerdserver_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/provisionerdserver"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/provisionerd/proto"
	sdkproto "github.com/coder/coder/v2/provisionersdk/proto"
	"github.com/coder/coder/v2/testutil"
)

// TestAcquireJobOwnerTokensWorkspaceAdmission verifies that builds of
// templates managed by workspace admission never carry the owner's session
// token or OIDC access token, while other templates keep receiving them.
func TestAcquireJobOwnerTokensWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	const profile = "small"

	type env struct {
		srv       proto.DRPCProvisionerDaemonServer
		db        database.Store
		pd        database.ProvisionerDaemon
		user      database.User
		link      database.UserLink
		version   database.TemplateVersion
		workspace database.WorkspaceTable
		file      database.File
	}
	newEnv := func(t *testing.T, managed bool) env {
		t.Helper()
		srv, db, _, pd := setup(t, false, nil)
		user := dbgen.User(t, db, database.User{})
		link := dbgen.UserLink(t, db, database.UserLink{
			LoginType:        database.LoginTypeOIDC,
			UserID:           user.ID,
			OAuthExpiry:      dbtime.Now().Add(time.Hour),
			OAuthAccessToken: "owner-oidc-access-token",
		})
		template := dbgen.Template(t, db, database.Template{
			Provisioner:    database.ProvisionerTypeEcho,
			OrganizationID: pd.OrganizationID,
			CreatedBy:      user.ID,
		})
		version := dbgen.TemplateVersion(t, db, database.TemplateVersion{
			CreatedBy:      user.ID,
			OrganizationID: pd.OrganizationID,
			TemplateID:     uuid.NullUUID{UUID: template.ID, Valid: true},
		})
		workspace := dbgen.Workspace(t, db, database.WorkspaceTable{
			TemplateID:     template.ID,
			OwnerID:        user.ID,
			OrganizationID: pd.OrganizationID,
		})
		if managed {
			admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
		}
		return env{
			srv:       srv,
			db:        db,
			pd:        pd,
			user:      user,
			link:      link,
			version:   version,
			workspace: workspace,
			file:      dbgen.File(t, db, database.File{CreatedBy: user.ID}),
		}
	}

	// build creates a pending build job. A managed START also gets a
	// granted admission so that a provisioner may acquire it.
	build := func(t *testing.T, e env, number int32, transition database.WorkspaceTransition, managed bool) {
		t.Helper()
		ctx := testutil.Context(t, testutil.WaitShort)
		buildID := uuid.New()
		job := dbgen.ProvisionerJob(t, e.db, nil, database.ProvisionerJob{
			OrganizationID: e.pd.OrganizationID,
			InitiatorID:    e.user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         e.file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input: must(json.Marshal(provisionerdserver.WorkspaceProvisionJob{
				WorkspaceBuildID: buildID,
			})),
			Tags: e.pd.Tags,
		})
		dbgen.WorkspaceBuild(t, e.db, database.WorkspaceBuild{
			ID:                buildID,
			WorkspaceID:       e.workspace.ID,
			BuildNumber:       number,
			JobID:             job.ID,
			TemplateVersionID: e.version.ID,
			Transition:        transition,
			Reason:            database.BuildReasonInitiator,
		})
		dbgen.WorkspaceBuildParameters(t, e.db, []database.WorkspaceBuildParameter{{
			WorkspaceBuildID: buildID,
			Name:             admissiontest.ProfileParameter,
			Value:            profile,
		}})
		if managed && transition == database.WorkspaceTransitionStart {
			_, err := e.db.InsertWorkspaceBuildAdmission(ctx, database.InsertWorkspaceBuildAdmissionParams{
				JobID:             job.ID,
				WorkspaceBuildID:  buildID,
				WorkspaceID:       e.workspace.ID,
				OrganizationID:    e.pd.OrganizationID,
				OwnerID:           e.user.ID,
				TemplateID:        e.workspace.TemplateID,
				TemplateVersionID: e.version.ID,
				Profile:           profile,
				Reason:            workspaceadmission.ReasonWaitingForCapacity,
				QueuedAt:          dbtime.Now(),
			})
			require.NoError(t, err)
			admissiontest.Grant(t, e.db, nil, job.ID)
		}
	}

	acquire := func(t *testing.T, e env) *sdkproto.Metadata {
		t.Helper()
		ctx := testutil.Context(t, testutil.WaitShort)
		acquired, err := e.srv.AcquireJob(ctx, nil)
		require.NoError(t, err)
		wb, ok := acquired.Type.(*proto.AcquiredJob_WorkspaceBuild_)
		require.True(t, ok, "acquired job is not a workspace build")
		return wb.WorkspaceBuild.Metadata
	}

	apiKeyIDs := func(t *testing.T, e env) map[string]string {
		t.Helper()
		ctx := testutil.Context(t, testutil.WaitShort)
		keys, err := e.db.GetAPIKeysByUserID(ctx, database.GetAPIKeysByUserIDParams{
			LoginType:      e.user.LoginType,
			UserID:         e.user.ID,
			IncludeExpired: true,
		})
		require.NoError(t, err)
		ids := map[string]string{}
		for _, key := range keys {
			ids[key.ID] = key.TokenName
		}
		return ids
	}

	t.Run("Managed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, true)
		tokenName := provisionerdserver.WorkspaceSessionTokenName(e.user.ID, e.workspace.ID)
		// A session token minted before the template became managed.
		stale, _ := dbgen.APIKey(t, e.db, database.APIKey{
			UserID:    e.user.ID,
			LoginType: e.user.LoginType,
			TokenName: tokenName,
		})
		before := apiKeyIDs(t, e)
		require.Contains(t, before, stale.ID)

		build(t, e, 1, database.WorkspaceTransitionStart, true)
		metadata := acquire(t, e)
		require.Equal(t, sdkproto.WorkspaceTransition_START, metadata.WorkspaceTransition)
		require.Empty(t, metadata.WorkspaceOwnerSessionToken)
		require.Empty(t, metadata.WorkspaceOwnerOidcAccessToken)
		// Other owner metadata is unchanged.
		require.Equal(t, e.user.Username, metadata.WorkspaceOwner)

		// No API key was minted for the owner, and the stale workspace
		// session token is gone.
		after := apiKeyIDs(t, e)
		for id := range after {
			require.Contains(t, before, id, "acquiring a managed build created API key %s", id)
		}
		require.NotContains(t, after, stale.ID)

		build(t, e, 2, database.WorkspaceTransitionStop, true)
		metadata = acquire(t, e)
		require.Equal(t, sdkproto.WorkspaceTransition_STOP, metadata.WorkspaceTransition)
		require.Empty(t, metadata.WorkspaceOwnerSessionToken)
		require.Empty(t, metadata.WorkspaceOwnerOidcAccessToken)
	})

	t.Run("NotManaged", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, false)
		tokenName := provisionerdserver.WorkspaceSessionTokenName(e.user.ID, e.workspace.ID)
		before := apiKeyIDs(t, e)

		build(t, e, 1, database.WorkspaceTransitionStart, false)
		metadata := acquire(t, e)
		require.NotEmpty(t, metadata.WorkspaceOwnerSessionToken)
		require.Equal(t, e.link.OAuthAccessToken, metadata.WorkspaceOwnerOidcAccessToken)

		var minted []string
		for id, name := range apiKeyIDs(t, e) {
			if _, ok := before[id]; !ok {
				require.Equal(t, tokenName, name)
				minted = append(minted, id)
			}
		}
		require.Len(t, minted, 1, "acquiring a START build mints the owner session token")

		build(t, e, 2, database.WorkspaceTransitionStop, false)
		metadata = acquire(t, e)
		require.Empty(t, metadata.WorkspaceOwnerSessionToken)
		require.Equal(t, e.link.OAuthAccessToken, metadata.WorkspaceOwnerOidcAccessToken)
	})
}
