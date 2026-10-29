package admissionapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

func (e apiEnv) createBody(name string) admissionapi.CreateWorkspaceRequest {
	return admissionapi.CreateWorkspaceRequest{
		OperationID: uuid.New(),
		OwnerID:     e.memberID,
		TemplateID:  e.template.ID,
		Name:        name,
		Profile:     "small",
		Source:      admissionapi.CreateWorkspaceSource{Kind: admissionapi.CreateSourceEmployee},
	}
}

func (e apiEnv) grantBody(id string) admissionapi.GrantMirror {
	return admissionapi.GrantMirror{
		ID:             id,
		OwnerID:        e.memberID,
		OrganizationID: e.orgID,
		TemplateIDs:    []uuid.UUID{e.template.ID},
		NamePrefix:     "agent-",
		MaxWorkspaces:  2,
		AllowDelete:    true,
		ExpiresAt:      time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
	}
}

func (e apiEnv) underGrant(grantID, name string) admissionapi.CreateWorkspaceRequest {
	body := e.createBody(name)
	body.Source = admissionapi.CreateWorkspaceSource{
		Kind:                 admissionapi.CreateSourceAgentGrant,
		GrantID:              grantID,
		RunID:                "r-01jbx5870000000000000000r1",
		Harness:              "codex",
		InitiatorWorkspaceID: uuid.NewString(),
	}
	return body
}

func (e apiEnv) putGrant(ctx context.Context, t *testing.T, g admissionapi.GrantMirror) admissionapi.GrantMirror {
	t.Helper()
	var grant admissionapi.GrantMirror
	require.Equal(t, http.StatusOK, e.call(ctx, t, http.MethodPut, "/v1/grants/"+g.ID, g, &grant))
	return grant
}

// callRaw sends an authenticated request and returns the status and the raw
// JSON object of the response.
func (e apiEnv) callRaw(ctx context.Context, t *testing.T, method, path string, body any) (int, map[string]json.RawMessage) {
	t.Helper()
	var raw map[string]json.RawMessage
	status := e.call(ctx, t, method, path, body, &raw)
	return status, raw
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestCreateWorkspace(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)

	t.Run("CreateAndRepeat", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		body := env.createBody("created-1")
		status, raw := env.callRaw(ctx, t, http.MethodPost, "/v1/workspaces", body)
		require.Equal(t, http.StatusCreated, status, raw)
		require.Equal(t, []string{"admission", "build_id", "created", "job_id", "workspace_id"}, sortedKeys(raw))
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/workspaces", body, &created))
		require.False(t, created.Created)
		require.Equal(t, "waiting", created.Admission.Status)
		require.Equal(t, "small", created.Admission.Profile)
		require.Equal(t, env.memberID, created.Admission.OwnerID)
		require.Equal(t, created.JobID, created.Admission.JobID)
		require.Equal(t, created.BuildID, created.Admission.WorkspaceBuildID)

		// The workspace belongs to the owner, who initiated its START.
		workspace, err := env.member.Workspace(ctx, created.WorkspaceID)
		require.NoError(t, err)
		require.Equal(t, "created-1", workspace.Name)
		require.Equal(t, env.memberID, workspace.OwnerID)
		require.Equal(t, created.BuildID, workspace.LatestBuild.ID)
		require.Equal(t, codersdk.WorkspaceTransitionStart, workspace.LatestBuild.Transition)
		require.Equal(t, env.memberID, workspace.LatestBuild.InitiatorID)
		require.Equal(t, codersdk.BuildReasonInitiator, workspace.LatestBuild.Reason)
		require.NotNil(t, workspace.LatestBuild.Admission)
		require.Equal(t, "waiting", string(workspace.LatestBuild.Admission.Status))
		params, err := env.member.WorkspaceBuildParameters(ctx, created.BuildID)
		require.NoError(t, err)
		require.Contains(t, params, codersdk.WorkspaceBuildParameter{Name: admissiontest.ProfileParameter, Value: "small"})

		// The operation records its source.
		op, err := env.db.GetWorkspaceAdmissionCreateOperationByOperationID(ctx, body.OperationID)
		require.NoError(t, err)
		require.Equal(t, created.WorkspaceID, op.WorkspaceID)
		require.False(t, op.GrantID.Valid)
		require.JSONEq(t, `{"kind":"employee","grant_id":"","run_id":"","harness":"","initiator_workspace_id":"","clone_of_workspace_id":""}`, string(op.Source))

		// The same operation for another owner conflicts.
		other := body
		other.OwnerID = env.ownerID
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/workspaces", other, &errResp))
		require.Equal(t, admissionapi.CodeOperationConflict, errResp.Code)

		// Another operation with the same name conflicts on the name.
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.createBody("created-1"), &errResp))
		require.Equal(t, admissionapi.CodeNameConflict, errResp.Code)
	})

	t.Run("Profiles", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		// A profile parameter must agree with the profile.
		mismatch := env.createBody("mismatch")
		mismatch.RichParameterValues = []admissionapi.RichParameterValue{{Name: admissiontest.ProfileParameter, Value: "large"}}
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", mismatch, &errResp))
		require.Equal(t, admissionapi.CodeProfileMismatch, errResp.Code)
		matching := env.createBody("matching")
		matching.Profile = "large"
		matching.RichParameterValues = []admissionapi.RichParameterValue{{Name: admissiontest.ProfileParameter, Value: "large"}}
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", matching, &created))
		require.Equal(t, "large", created.Admission.Profile)

		// The owner must be entitled to the profile and the template.
		xl := env.createBody("xl")
		xl.Profile = "xl"
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", xl, &errResp))
		require.Equal(t, admissionapi.CodeNotEntitled, errResp.Code)
		admin := env.createBody("admin-ws")
		admin.OwnerID = env.ownerID
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", admin, &errResp))
		require.Equal(t, admissionapi.CodeNotEntitled, errResp.Code)
	})

	t.Run("Templates", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		unmanaged := env.createBody("unmanaged")
		unmanaged.TemplateID = env.unmanaged.ID
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", unmanaged, &errResp))
		require.Equal(t, admissionapi.CodeTemplateNotManaged, errResp.Code)

		// A version of another template is refused; a version of the
		// template is used.
		wrongVersion := env.createBody("wrong-version")
		wrongVersion.TemplateVersionID = env.unmanaged.ActiveVersionID.String()
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", wrongVersion, &errResp))
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
		unknownVersion := env.createBody("unknown-version")
		unknownVersion.TemplateVersionID = uuid.NewString()
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", unknownVersion, &errResp))
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
		versioned := env.createBody("versioned")
		versioned.TemplateVersionID = env.template.ActiveVersionID.String()
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", versioned, &created))
		require.Equal(t, env.template.ActiveVersionID, created.Admission.TemplateVersionID)
	})

	t.Run("Owners", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		unknown := env.createBody("unknown-owner")
		unknown.OwnerID = uuid.New()
		require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, "/v1/workspaces", unknown, &errResp))
		require.Equal(t, admissionapi.CodeNotFound, errResp.Code)

		// Suspended owners are refused until they are active again.
		_, user := coderdtest.CreateAnotherUser(t, env.owner, env.orgID)
		admissiontest.Entitle(t, env.db, user.ID, env.template.ID, "small")
		body := env.createBody("suspended")
		body.OwnerID = user.ID
		_, err := env.owner.UpdateUserStatus(ctx, user.ID.String(), codersdk.UserStatusSuspended)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", body, &errResp))
		require.Equal(t, admissionapi.CodeOwnerInactive, errResp.Code)
		_, err = env.owner.UpdateUserStatus(ctx, user.ID.String(), codersdk.UserStatusActive)
		require.NoError(t, err)
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", body, &created))
		require.Equal(t, user.ID, created.Admission.OwnerID)
	})

	t.Run("InvalidRequests", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		for name, mutate := range map[string]func(*admissionapi.CreateWorkspaceRequest){
			"no operation":     func(b *admissionapi.CreateWorkspaceRequest) { b.OperationID = uuid.Nil },
			"no owner":         func(b *admissionapi.CreateWorkspaceRequest) { b.OwnerID = uuid.Nil },
			"no template":      func(b *admissionapi.CreateWorkspaceRequest) { b.TemplateID = uuid.Nil },
			"no profile":       func(b *admissionapi.CreateWorkspaceRequest) { b.Profile = "" },
			"long profile":     func(b *admissionapi.CreateWorkspaceRequest) { b.Profile = strings.Repeat("p", 65) },
			"bad name":         func(b *admissionapi.CreateWorkspaceRequest) { b.Name = "Not A Name!" },
			"bad kind":         func(b *admissionapi.CreateWorkspaceRequest) { b.Source.Kind = "robot" },
			"grant without id": func(b *admissionapi.CreateWorkspaceRequest) { b.Source.Kind = admissionapi.CreateSourceAgentGrant },
			"bad grant id":     func(b *admissionapi.CreateWorkspaceRequest) { b.Source.GrantID = "G-UPPER" },
			"bad run id":       func(b *admissionapi.CreateWorkspaceRequest) { b.Source.RunID = "run/1" },
			"bad harness":      func(b *admissionapi.CreateWorkspaceRequest) { b.Source.Harness = "Codex CLI" },
			"bad initiator":    func(b *admissionapi.CreateWorkspaceRequest) { b.Source.InitiatorWorkspaceID = "nope" },
			"bad clone source": func(b *admissionapi.CreateWorkspaceRequest) { b.Source.CloneOfWorkspaceID = "nope" },
			"bad version":      func(b *admissionapi.CreateWorkspaceRequest) { b.TemplateVersionID = "nope" },
			"empty parameter": func(b *admissionapi.CreateWorkspaceRequest) {
				b.RichParameterValues = []admissionapi.RichParameterValue{{Value: "x"}}
			},
		} {
			b := env.createBody("invalid")
			mutate(&b)
			require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", b, &errResp), name)
			require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code, name)
		}
		var unknownField map[string]any
		data, err := json.Marshal(env.createBody("unknown-field"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &unknownField))
		unknownField["ttl_ms"] = 1000
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/workspaces", unknownField, &errResp))
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	})
}

func TestCreateWorkspaceUnderGrant(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)

	t.Run("Quota", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse
		const grantID = "g-01jbx5870000000000000000q1"
		env.putGrant(ctx, t, env.grantBody(grantID))

		// Names must carry the prefix.
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "other-q1"), &errResp))
		require.Equal(t, admissionapi.CodeGrantScope, errResp.Code)

		// At most max_workspaces live workspaces, creates in progress
		// included.
		first := env.underGrant(grantID, "agent-q1")
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", first, &created))
		firstID := created.WorkspaceID
		op, err := env.db.GetWorkspaceAdmissionCreateOperationByOperationID(ctx, first.OperationID)
		require.NoError(t, err)
		require.Equal(t, grantID, op.GrantID.String)
		require.Equal(t, first.Source.RunID, op.RunID.String)
		var source admissionapi.CreateWorkspaceSource
		require.NoError(t, json.Unmarshal(op.Source, &source))
		require.Equal(t, first.Source, source)
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "agent-q2"), &created))
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "agent-q3"), &errResp))
		require.Equal(t, admissionapi.CodeGrantQuota, errResp.Code)

		// A repeated operation still answers with its workspace; the same
		// operation under another grant conflicts.
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/workspaces", first, &created))
		require.Equal(t, firstID, created.WorkspaceID)
		moved := first
		moved.Source.GrantID = "g-01jbx5870000000000000000q9"
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/workspaces", moved, &errResp))
		require.Equal(t, admissionapi.CodeOperationConflict, errResp.Code)

		// A deleted workspace frees its slot.
		require.NoError(t, env.db.UpdateWorkspaceDeletedByID(ctx, database.UpdateWorkspaceDeletedByIDParams{ID: firstID, Deleted: true}))
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "agent-q3"), &created))
	})

	t.Run("Templates", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		narrow := env.grantBody("g-01jbx5870000000000000000t1")
		narrow.TemplateIDs = []uuid.UUID{uuid.New()}
		env.putGrant(ctx, t, narrow)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(narrow.ID, "agent-t1"), &errResp))
		require.Equal(t, admissionapi.CodeGrantScope, errResp.Code)

		// An empty template list and prefix allow every entitled template
		// and name.
		anyTemplate := env.grantBody("g-01jbx5870000000000000000t2")
		anyTemplate.TemplateIDs = nil
		anyTemplate.NamePrefix = ""
		status, raw := env.callRaw(ctx, t, http.MethodPut, "/v1/grants/"+anyTemplate.ID, anyTemplate)
		require.Equal(t, http.StatusOK, status, raw)
		require.NotContains(t, raw, "template_ids")
		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(anyTemplate.ID, "any-t1"), &created))
	})

	t.Run("Denied", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		foreign := env.grantBody("g-01jbx5870000000000000000x1")
		foreign.OwnerID = env.ownerID
		env.putGrant(ctx, t, foreign)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(foreign.ID, "agent-x1"), &errResp))
		require.Equal(t, admissionapi.CodeGrantScope, errResp.Code)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant("g-01jbx5870000000000000000zz", "agent-x2"), &errResp))
		require.Equal(t, admissionapi.CodeGrantNotFound, errResp.Code)
		expired := env.grantBody("g-01jbx5870000000000000000x3")
		expired.ExpiresAt = time.Now().Add(-time.Minute).UTC()
		env.putGrant(ctx, t, expired)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(expired.ID, "agent-x3"), &errResp))
		require.Equal(t, admissionapi.CodeGrantExpired, errResp.Code)
		// An employee create that names a grant is checked against it too.
		employee := env.createBody("agent-x4")
		employee.Source.GrantID = expired.ID
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", employee, &errResp))
		require.Equal(t, admissionapi.CodeGrantExpired, errResp.Code)
	})

	t.Run("Revocation", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse
		mirror := env.grantBody("g-01jbx5870000000000000000r1")
		env.putGrant(ctx, t, mirror)

		status, raw := env.callRaw(ctx, t, http.MethodPost, "/v1/grants/"+mirror.ID+"/revoke", nil)
		require.Equal(t, http.StatusOK, status, raw)
		require.Contains(t, raw, "revoked_at")
		var revoked admissionapi.GrantMirror
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/grants/"+mirror.ID+"/revoke", map[string]any{}, &revoked))
		require.NotNil(t, revoked.RevokedAt)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(mirror.ID, "agent-r1"), &errResp))
		require.Equal(t, admissionapi.CodeGrantRevoked, errResp.Code)

		// A later mirror never clears the revocation.
		grant := env.putGrant(ctx, t, mirror)
		require.NotNil(t, grant.RevokedAt)
		require.True(t, revoked.RevokedAt.Equal(*grant.RevokedAt))

		require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, "/v1/grants/g-01jbx5870000000000000000nx/revoke", nil, &errResp))
		require.Equal(t, admissionapi.CodeNotFound, errResp.Code)
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/grants/"+mirror.ID+"/revoke", map[string]any{"reason": "x"}, &errResp))
		require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code)
	})

	t.Run("Mirror", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		// The mirror has exactly the keys of the controller type.
		mirror := env.grantBody("g-01jbx5870000000000000000m1")
		status, raw := env.callRaw(ctx, t, http.MethodPut, "/v1/grants/"+mirror.ID, mirror)
		require.Equal(t, http.StatusOK, status, raw)
		require.Equal(t, []string{"allow_delete", "expires_at", "id", "max_workspaces", "name_prefix", "organization_id", "owner_id", "template_ids"}, sortedKeys(raw))
		grant := env.putGrant(ctx, t, mirror)
		// The database may answer in another time zone.
		grant.ExpiresAt = grant.ExpiresAt.UTC()
		require.Equal(t, mirror, grant)
		// A mirror without id takes the one of the path.
		noID := mirror
		noID.ID = ""
		noID.MaxWorkspaces = 3
		var updated admissionapi.GrantMirror
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/grants/"+mirror.ID, noID, &updated))
		require.Equal(t, mirror.ID, updated.ID)
		require.Equal(t, 3, updated.MaxWorkspaces)

		// The owner of a grant never changes.
		moveOwner := mirror
		moveOwner.OwnerID = env.ownerID
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPut, "/v1/grants/"+mirror.ID, moveOwner, &errResp))
		require.Equal(t, admissionapi.CodeOperationConflict, errResp.Code)

		const invalidID = "g-01jbx5870000000000000000m2"
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, "/v1/grants/G-UPPER", mirror, &errResp))
		for name, mutate := range map[string]func(*admissionapi.GrantMirror){
			"other id":       func(g *admissionapi.GrantMirror) { g.ID = "g-other" },
			"no owner":       func(g *admissionapi.GrantMirror) { g.OwnerID = uuid.Nil },
			"no org":         func(g *admissionapi.GrantMirror) { g.OrganizationID = uuid.Nil },
			"negative max":   func(g *admissionapi.GrantMirror) { g.MaxWorkspaces = -1 },
			"no expiry":      func(g *admissionapi.GrantMirror) { g.ExpiresAt = time.Time{} },
			"nil template":   func(g *admissionapi.GrantMirror) { g.TemplateIDs = []uuid.UUID{uuid.Nil} },
			"long prefix":    func(g *admissionapi.GrantMirror) { g.NamePrefix = strings.Repeat("a", 33) },
			"unknown org":    func(g *admissionapi.GrantMirror) { g.OrganizationID = uuid.New() },
			"zero revoke at": func(g *admissionapi.GrantMirror) { g.RevokedAt = ptr.Ref(time.Time{}) },
		} {
			g := env.grantBody(invalidID)
			mutate(&g)
			require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, "/v1/grants/"+invalidID, g, &errResp), name)
			require.Equal(t, admissionapi.CodeInvalidRequest, errResp.Code, name)
		}
		unknownOwner := env.grantBody(invalidID)
		unknownOwner.OwnerID = uuid.New()
		require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPut, "/v1/grants/"+invalidID, unknownOwner, &errResp))
		require.Equal(t, admissionapi.CodeNotFound, errResp.Code)
	})
}

// TestCreateWorkspaceConcurrent checks that the grant row lock serializes
// creates under one grant, so the backstop limit holds under concurrency,
// and that concurrent retries of one operation create one workspace.
func TestCreateWorkspaceConcurrent(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	const n = 4

	t.Run("GrantQuota", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		const grantID = "g-01jbx5870000000000000000c1"
		mirror := env.grantBody(grantID)
		mirror.MaxWorkspaces = 1
		env.putGrant(ctx, t, mirror)

		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var resp map[string]any
				statuses[i] = env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "agent-c"+string(rune('a'+i))), &resp)
			}()
		}
		wg.Wait()
		sort.Ints(statuses)
		require.Equal(t, []int{http.StatusCreated, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}, statuses)
	})

	t.Run("SameOperation", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		same := env.createBody("same-op")
		ids := make([]uuid.UUID, n)
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var resp admissionapi.CreateWorkspaceResponse
				statuses[i] = env.call(ctx, t, http.MethodPost, "/v1/workspaces", same, &resp)
				ids[i] = resp.WorkspaceID
			}()
		}
		wg.Wait()
		sort.Ints(statuses)
		require.Equal(t, []int{http.StatusOK, http.StatusOK, http.StatusOK, http.StatusCreated}, statuses)
		for _, id := range ids {
			require.Equal(t, ids[0], id)
		}
	})
}

func TestDeleteWorkspace(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	deletePath := func(id uuid.UUID) string { return "/v1/workspaces/" + id.String() + "/delete" }

	t.Run("Employee", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse

		// Deleting cancels the pending START and creates the DELETE
		// together.
		workspace := env.waitingWorkspace(t, "small")
		startJobID := workspace.LatestBuild.Job.ID
		op := uuid.New()
		status, raw := env.callRaw(ctx, t, http.MethodPost, deletePath(workspace.ID), admissionapi.DeleteWorkspaceRequest{OperationID: op})
		require.Equal(t, http.StatusAccepted, status, raw)
		require.Equal(t, []string{"build_id", "job_id"}, sortedKeys(raw))
		var deleted admissionapi.DeleteWorkspaceResponse
		require.Equal(t, http.StatusAccepted, env.call(ctx, t, http.MethodPost, deletePath(workspace.ID), admissionapi.DeleteWorkspaceRequest{OperationID: op}, &deleted))
		require.Equal(t, database.WorkspaceAdmissionStatusCanceled, admissiontest.Admission(t, env.db, startJobID).Status)
		build, err := env.member.WorkspaceBuild(ctx, deleted.BuildID)
		require.NoError(t, err)
		require.Equal(t, codersdk.WorkspaceTransitionDelete, build.Transition)
		require.Equal(t, deleted.JobID, build.Job.ID)
		require.Equal(t, env.memberID, build.InitiatorID)
		require.Equal(t, codersdk.BuildReasonInitiator, build.Reason)
		delOp, err := env.db.GetWorkspaceAdmissionDeleteOperationByOperationID(ctx, op)
		require.NoError(t, err)
		require.Equal(t, startJobID, delOp.CanceledJobID.UUID)
		require.False(t, delOp.GrantID.Valid)

		// Another operation conflicts with the pending DELETE.
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, deletePath(workspace.ID), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New()}, &errResp))
		require.Equal(t, admissionapi.CodeActiveBuild, errResp.Code)
		// A deleted workspace cannot be deleted again.
		require.NoError(t, env.db.UpdateWorkspaceDeletedByID(ctx, database.UpdateWorkspaceDeletedByIDParams{ID: workspace.ID, Deleted: true}))
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, deletePath(workspace.ID), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New()}, &errResp))
		require.Equal(t, admissionapi.CodeWorkspaceDeleted, errResp.Code)
	})

	t.Run("Invalid", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse
		workspace := env.waitingWorkspace(t, "small")
		other := env.waitingWorkspace(t, "small")

		// An operation belongs to one workspace.
		op := uuid.New()
		var deleted admissionapi.DeleteWorkspaceResponse
		require.Equal(t, http.StatusAccepted, env.call(ctx, t, http.MethodPost, deletePath(workspace.ID), admissionapi.DeleteWorkspaceRequest{OperationID: op}, &deleted))
		require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, deletePath(other.ID), admissionapi.DeleteWorkspaceRequest{OperationID: op}, &errResp))
		require.Equal(t, admissionapi.CodeOperationConflict, errResp.Code)

		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, deletePath(other.ID), admissionapi.DeleteWorkspaceRequest{}, &errResp))
		require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, deletePath(other.ID), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New(), GrantID: "G!"}, &errResp))
		require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, deletePath(uuid.New()), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New()}, &errResp))
	})

	t.Run("UnderGrant", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		var errResp admissionapi.ErrorResponse
		const grantID = "g-01jbx5870000000000000000d1"
		const noDeleteID = "g-01jbx5870000000000000000d2"
		env.putGrant(ctx, t, env.grantBody(grantID))
		noDelete := env.grantBody(noDeleteID)
		noDelete.AllowDelete = false
		env.putGrant(ctx, t, noDelete)

		var created admissionapi.CreateWorkspaceResponse
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(grantID, "agent-d1"), &created))
		byGrant := created.WorkspaceID
		require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(noDeleteID, "agent-d2"), &created))
		byNoDelete := created.WorkspaceID
		byEmployee := env.waitingWorkspace(t, "small")

		// The grant must allow deletes and must have created the workspace.
		for _, c := range []struct {
			workspace uuid.UUID
			grant     string
			code      string
		}{
			{byNoDelete, noDeleteID, admissionapi.CodeGrantScope},
			{byEmployee.ID, grantID, admissionapi.CodeGrantScope},
			{byNoDelete, grantID, admissionapi.CodeGrantScope},
			{byGrant, "g-01jbx5870000000000000000zz", admissionapi.CodeGrantNotFound},
		} {
			require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, deletePath(c.workspace), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New(), GrantID: c.grant}, &errResp))
			require.Equal(t, c.code, errResp.Code)
		}

		op := uuid.New()
		var deleted admissionapi.DeleteWorkspaceResponse
		require.Equal(t, http.StatusAccepted, env.call(ctx, t, http.MethodPost, deletePath(byGrant), admissionapi.DeleteWorkspaceRequest{OperationID: op, GrantID: grantID}, &deleted))
		delOp, err := env.db.GetWorkspaceAdmissionDeleteOperationByOperationID(ctx, op)
		require.NoError(t, err)
		require.Equal(t, grantID, delOp.GrantID.String)

		// A revoked grant deletes nothing more, but a repeated operation
		// still answers with its build.
		var revoked admissionapi.GrantMirror
		require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil, &revoked))
		var again admissionapi.DeleteWorkspaceResponse
		require.Equal(t, http.StatusAccepted, env.call(ctx, t, http.MethodPost, deletePath(byGrant), admissionapi.DeleteWorkspaceRequest{OperationID: op, GrantID: grantID}, &again))
		require.Equal(t, deleted, again)
		require.Equal(t, http.StatusForbidden, env.call(ctx, t, http.MethodPost, deletePath(byGrant), admissionapi.DeleteWorkspaceRequest{OperationID: uuid.New(), GrantID: grantID}, &errResp))
		require.Equal(t, admissionapi.CodeGrantRevoked, errResp.Code)
	})
}
