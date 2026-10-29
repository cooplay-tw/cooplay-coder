package admissionapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/util/ptr"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/coderd/wspubsub"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

const testToken = "admission-test-token"

type apiEnv struct {
	db       database.Store
	ps       pubsub.Pubsub
	owner    *codersdk.Client
	orgID    uuid.UUID
	ownerID  uuid.UUID
	member   *codersdk.Client
	memberID uuid.UUID
	template codersdk.Template
	// unmanaged is an imported template that is not managed.
	unmanaged codersdk.Template
	url       string
	hashFile  string
	// agentInactiveTimeout is the agent inactivity timeout of the
	// server.
	agentInactiveTimeout time.Duration
}

func writeTokenHash(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	path := filepath.Join(t.TempDir(), "token.sha256")
	require.NoError(t, os.WriteFile(path, []byte("sha256:"+hex.EncodeToString(sum[:])+"\n"), 0o600))
	return path
}

// newAPIEnv starts coderd with a managed template and the admission API. No
// provisioner runs after the template import, so tests decide when jobs are
// acquired.
func newAPIEnv(t *testing.T, clock quartz.Clock) apiEnv {
	t.Helper()
	db, ps := dbtestutil.NewDB(t)
	owner, provisioner, api := coderdtest.NewWithAPI(t, &coderdtest.Options{
		Database:                 db,
		Pubsub:                   ps,
		IncludeProvisionerDaemon: true,
	})
	first := coderdtest.CreateFirstUser(t, owner)
	member, memberUser := coderdtest.CreateAnotherUser(t, owner, first.OrganizationID)
	version := coderdtest.CreateTemplateVersion(t, owner, first.OrganizationID, admissiontest.ProfileResponses())
	coderdtest.AwaitTemplateVersionJobCompleted(t, owner, version.ID)
	unmanagedVersion := coderdtest.CreateTemplateVersion(t, owner, first.OrganizationID, nil)
	coderdtest.AwaitTemplateVersionJobCompleted(t, owner, unmanagedVersion.ID)
	require.NoError(t, provisioner.Close())
	// The classic parameter flow keeps build creation cheap under the race
	// detector; admission does not depend on the parameter flow.
	classic := func(req *codersdk.CreateTemplateRequest) {
		req.UseClassicParameterFlow = ptr.Ref(true)
	}
	template := coderdtest.CreateTemplate(t, owner, first.OrganizationID, version.ID, classic)
	unmanaged := coderdtest.CreateTemplate(t, owner, first.OrganizationID, unmanagedVersion.ID, classic)
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	admissiontest.Entitle(t, db, memberUser.ID, template.ID, "small", "large")

	hashFile := writeTokenHash(t, testToken)
	server, err := admissionapi.New(admissionapi.Options{
		// Tests break the token hash file on purpose, which logs errors.
		Logger:                         slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}),
		Database:                       api.Database,
		Pubsub:                         api.Pubsub,
		FileCache:                      api.FileCache,
		BuildUsageChecker:              api.BuildUsageChecker,
		Experiments:                    api.Experiments,
		DeploymentValues:               api.DeploymentValues,
		TokenHashFile:                  hashFile,
		Clock:                          clock,
		AgentInactiveDisconnectTimeout: api.AgentInactiveDisconnectTimeout,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	return apiEnv{
		db:                   db,
		ps:                   ps,
		owner:                owner,
		orgID:                first.OrganizationID,
		ownerID:              first.UserID,
		member:               member,
		memberID:             memberUser.ID,
		template:             template,
		unmanaged:            unmanaged,
		url:                  srv.URL,
		hashFile:             hashFile,
		agentInactiveTimeout: api.AgentInactiveDisconnectTimeout,
	}
}

func (e apiEnv) waitingWorkspace(t *testing.T, profile string) codersdk.Workspace {
	t.Helper()
	return coderdtest.CreateWorkspace(t, e.member, e.template.ID, func(req *codersdk.CreateWorkspaceRequest) {
		req.RichParameterValues = admissiontest.Profile(profile)
	})
}

// call sends an authenticated request and decodes a JSON response into out.
func (e apiEnv) call(ctx context.Context, t *testing.T, method, path string, body, out any) int {
	t.Helper()
	return callWithToken(ctx, t, e.url, testToken, method, path, body, out)
}

func callWithToken(ctx context.Context, t *testing.T, baseURL, token, method, path string, body, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	if out != nil {
		require.NoError(t, json.Unmarshal(data, out), "decode %s %s: %s", method, path, string(data))
	}
	return res.StatusCode
}

func grantBody(profile string) admissionapi.GrantRequest {
	return admissionapi.GrantRequest{
		ReservationID:   uuid.New(),
		Generation:      1,
		OperationID:     uuid.New(),
		Profile:         profile,
		GrantTTLSeconds: 600,
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusUnauthorized, callWithToken(ctx, t, env.url, "", http.MethodGet, "/healthz", nil, &errResp))
	require.Equal(t, admissionapi.CodeUnauthorized, errResp.Code)
	require.Equal(t, http.StatusUnauthorized, callWithToken(ctx, t, env.url, "wrong-token", http.MethodGet, "/v1/requests", nil, &errResp))
	require.Equal(t, admissionapi.CodeUnauthorized, errResp.Code)

	var health map[string]string
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/healthz", nil, &health))
	require.Equal(t, "ok", health["status"])

	// A broken hash file fails closed.
	require.NoError(t, os.WriteFile(env.hashFile, []byte("not-a-hash"), 0o600))
	require.Equal(t, http.StatusServiceUnavailable, env.call(ctx, t, http.MethodGet, "/healthz", nil, &errResp))
	require.Equal(t, admissionapi.CodeTokenUnavailable, errResp.Code)
	require.NoError(t, os.Remove(env.hashFile))
	require.Equal(t, http.StatusServiceUnavailable, env.call(ctx, t, http.MethodGet, "/healthz", nil, &errResp))

	// A rotated hash applies without a restart.
	rotated := writeTokenHash(t, "rotated-token")
	data, err := os.ReadFile(rotated)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(env.hashFile, data, 0o600))
	require.Equal(t, http.StatusUnauthorized, env.call(ctx, t, http.MethodGet, "/healthz", nil, &errResp))
	require.Equal(t, http.StatusOK, callWithToken(ctx, t, env.url, "rotated-token", http.MethodGet, "/healthz", nil, &health))
}

func TestNewRequiresTokenHash(t *testing.T) {
	t.Parallel()
	db, ps := dbtestutil.NewDB(t)
	_, _, api := coderdtest.NewWithAPI(t, &coderdtest.Options{Database: db, Pubsub: ps})

	for name, path := range map[string]string{
		"Empty":   "",
		"Missing": filepath.Join(t.TempDir(), "missing"),
		"Invalid": func() string {
			p := filepath.Join(t.TempDir(), "invalid")
			require.NoError(t, os.WriteFile(p, []byte("sha256:abc"), 0o600))
			return p
		}(),
	} {
		_, err := admissionapi.New(admissionapi.Options{
			Logger:            testutil.Logger(t),
			Database:          api.Database,
			Pubsub:            api.Pubsub,
			BuildUsageChecker: api.BuildUsageChecker,
			TokenHashFile:     path,
		})
		require.Error(t, err, name)
	}
}

// TestNotOnMainListener verifies that the admission routes are not served by
// the employee-facing API, even with the service token.
func TestNotOnMainListener(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	workspace := env.waitingWorkspace(t, "small")
	jobID := workspace.LatestBuild.Job.ID
	mainURL := env.owner.URL.String()

	for _, prefix := range []string{"", "/api/v2"} {
		for _, token := range []string{testToken, env.owner.SessionToken()} {
			var granted admissionapi.Request
			_ = callWithTokenRaw(ctx, t, mainURL, token, http.MethodPost, prefix+"/v1/requests/"+jobID.String()+"/grant", grantBody("small"), &granted)
			require.NotEqual(t, "granted", granted.Status, prefix)
			var list admissionapi.ListRequestsResponse
			_ = callWithTokenRaw(ctx, t, mainURL, token, http.MethodGet, prefix+"/v1/requests", nil, &list)
			require.Empty(t, list.Requests, prefix)
		}
	}
	admission := admissiontest.Admission(t, env.db, jobID)
	require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
}

// callWithTokenRaw is callWithToken for responses that may not be JSON.
// It decodes JSON bodies into out and ignores other bodies.
func callWithTokenRaw(ctx context.Context, t *testing.T, baseURL, token, method, path string, body, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	_ = json.Unmarshal(data, out)
	return res.StatusCode
}

func TestRequestLifecycle(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	workspace := env.waitingWorkspace(t, "small")
	jobID := workspace.LatestBuild.Job.ID
	path := "/v1/requests/" + jobID.String()

	var list admissionapi.ListRequestsResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests?status=waiting,granted", nil, &list))
	require.Len(t, list.Requests, 1)
	require.Equal(t, jobID, list.Requests[0].JobID)
	require.Nil(t, list.NextAfter)

	var req admissionapi.Request
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &req))
	require.Equal(t, "waiting", req.Status)
	require.Equal(t, "small", req.Profile)
	require.Equal(t, "pending", req.Job.Status)
	require.Nil(t, req.Job.StartedAt)
	require.False(t, req.WorkspaceDeleted)
	require.Equal(t, env.memberID, req.OwnerID)

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodGet, "/v1/requests/"+uuid.NewString(), nil, &errResp))
	require.Equal(t, admissionapi.CodeNotFound, errResp.Code)

	// Grant validation.
	bad := grantBody("small")
	bad.ReservationID = uuid.Nil
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, path+"/grant", bad, &errResp))
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, path+"/grant", map[string]any{"unknown": true}, &errResp))
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/grant", grantBody("large"), &errResp))
	require.Equal(t, admissionapi.CodeProfileMismatch, errResp.Code)
	require.NotNil(t, errResp.Request)
	require.Equal(t, "waiting", errResp.Request.Status)

	// Grant and refresh.
	grant := grantBody("small")
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/grant", grant, &req))
	require.Equal(t, "granted", req.Status)
	require.Equal(t, grant.ReservationID, *req.ReservationID)
	require.Equal(t, grant.OperationID, *req.OperationID)
	require.EqualValues(t, 1, *req.Generation)
	require.NotNil(t, req.GrantExpiresAt)
	firstExpiry := *req.GrantExpiresAt
	refresh := grant
	refresh.Generation = 2
	refresh.GrantTTLSeconds = 1200
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/grant", refresh, &req))
	require.True(t, req.GrantExpiresAt.After(firstExpiry))
	stale := grant
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/grant", stale, &errResp))
	require.Equal(t, admissionapi.CodeStaleGeneration, errResp.Code)
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/grant", grantBody("small"), &errResp))
	require.Equal(t, admissionapi.CodeAlreadyGranted, errResp.Code)

	// Revoke returns the request to the capacity queue.
	var revoke admissionapi.RevokeResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/revoke", admissionapi.RevokeRequest{ReservationID: uuid.New()}, &errResp))
	require.Equal(t, admissionapi.CodeReservationMismatch, errResp.Code)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/revoke", admissionapi.RevokeRequest{ReservationID: grant.ReservationID, Reason: "capacity_reclaimed"}, &revoke))
	require.False(t, revoke.Acquired)
	require.Equal(t, "waiting", revoke.Request.Status)
	require.Equal(t, "capacity_reclaimed", revoke.Request.Reason)
	require.Nil(t, revoke.Request.ReservationID)
	_, err := admissiontest.Acquire(ctx, env.db, env.orgID)
	require.ErrorIs(t, err, sql.ErrNoRows, "a revoked grant is not acquirable")

	// Grant again, then a provisioner acquires the job.
	grant = grantBody("small")
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/grant", grant, &req))
	acquired, err := admissiontest.Acquire(ctx, env.db, env.orgID)
	require.NoError(t, err)
	require.Equal(t, jobID, acquired.ID)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, path, nil, &req))
	require.Equal(t, "acquired", req.Status)
	require.Equal(t, "running", req.Job.Status)

	// Once acquired, revoke reports it and changes nothing.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/revoke", admissionapi.RevokeRequest{ReservationID: grant.ReservationID}, &revoke))
	require.True(t, revoke.Acquired)
	require.Equal(t, "acquired", revoke.Request.Status)
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/grant", grant, &errResp))
	require.Equal(t, admissionapi.CodeAlreadyAcquired, errResp.Code)
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/reject", admissionapi.RejectRequest{Code: "x", Reason: "y"}, &errResp))
	require.Equal(t, admissionapi.CodeAlreadyAcquired, errResp.Code)
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	first := env.waitingWorkspace(t, "small")
	second := env.waitingWorkspace(t, "large")

	var page admissionapi.ListRequestsResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests?limit=1", nil, &page))
	require.Len(t, page.Requests, 1)
	require.Equal(t, first.LatestBuild.Job.ID, page.Requests[0].JobID)
	require.NotNil(t, page.NextAfter)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests?limit=1&after="+page.NextAfter.String(), nil, &page))
	require.Len(t, page.Requests, 1)
	require.Equal(t, second.LatestBuild.Job.ID, page.Requests[0].JobID)

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodGet, "/v1/requests?status=bogus", nil, &errResp))
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodGet, "/v1/requests?limit=0", nil, &errResp))
}

func TestReject(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	workspace := env.waitingWorkspace(t, "small")
	jobID := workspace.LatestBuild.Job.ID
	path := "/v1/requests/" + jobID.String()

	var req admissionapi.Request
	body := admissionapi.RejectRequest{Code: "quota_exceeded", Reason: "the profile exceeds the user quota"}
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/reject", body, &req))
	require.Equal(t, "rejected", req.Status)
	require.Equal(t, "failed", req.Job.Status)
	require.Equal(t, "admission rejected: quota_exceeded: the profile exceeds the user quota", req.Job.Error)

	build, err := env.member.WorkspaceBuild(ctx, workspace.LatestBuild.ID)
	require.NoError(t, err)
	require.Equal(t, codersdk.ProvisionerJobFailed, build.Job.Status)
	require.Contains(t, build.Job.Error, "admission rejected")

	// Rejecting again is idempotent; granting is not possible.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/reject", body, &req))
	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path+"/grant", grantBody("small"), &errResp))
	require.Equal(t, admissionapi.CodeJobNotPending, errResp.Code)
	_, err = admissiontest.Acquire(ctx, env.db, env.orgID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestDisplay(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	a := env.waitingWorkspace(t, "small").LatestBuild.Job.ID
	b := env.waitingWorkspace(t, "small").LatestBuild.Job.ID
	granted := env.waitingWorkspace(t, "small").LatestBuild.Job.ID
	var req admissionapi.Request
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/requests/"+granted.String()+"/grant", grantBody("small"), &req))

	position := int32(3)
	var resp admissionapi.DisplayResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/requests/display", []admissionapi.DisplayUpdate{
		{JobID: a, Reason: "third in line", QueuePosition: &position},
		{JobID: b, Reason: "waiting for a large host"},
		{JobID: granted, Reason: "ignored"},
		{JobID: uuid.New(), Reason: "missing"},
	}, &resp))
	require.ElementsMatch(t, []uuid.UUID{a, b}, resp.Updated)
	require.Len(t, resp.Skipped, 2)

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests/"+a.String(), nil, &req))
	require.Equal(t, "third in line", req.Reason)
	require.EqualValues(t, 3, *req.QueuePosition)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests/"+b.String(), nil, &req))
	require.Nil(t, req.QueuePosition)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/requests/"+granted.String(), nil, &req))
	require.Equal(t, "granted", req.Status)
	require.Empty(t, req.Reason)

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPost, "/v1/requests/display", []admissionapi.DisplayUpdate{
		{JobID: a}, {JobID: a},
	}, &errResp))
}

// TestAdmissionChangesNotifyWorkspaceWatchers verifies that changes users
// see on the workspace page refresh the watchers of that workspace.
func TestAdmissionChangesNotifyWorkspaceWatchers(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	workspace := env.waitingWorkspace(t, "small")
	path := "/v1/requests/" + workspace.LatestBuild.Job.ID.String()

	events := make(chan uuid.UUID, 16)
	cancel, err := env.ps.SubscribeWithErr(wspubsub.WorkspaceEventChannel(env.memberID), wspubsub.HandleWorkspaceEvent(
		func(_ context.Context, e wspubsub.WorkspaceEvent, err error) {
			if err == nil && e.Kind == wspubsub.WorkspaceEventKindStateChange {
				events <- e.WorkspaceID
			}
		}))
	require.NoError(t, err)
	defer cancel()

	position := int32(2)
	var display admissionapi.DisplayResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/requests/display", []admissionapi.DisplayUpdate{
		{JobID: workspace.LatestBuild.Job.ID, Reason: "second in line", QueuePosition: &position},
	}, &display))
	require.Equal(t, workspace.ID, testutil.RequireReceive(ctx, t, events), "display update")

	grant := grantBody("small")
	var req admissionapi.Request
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/grant", grant, &req))
	require.Equal(t, workspace.ID, testutil.RequireReceive(ctx, t, events), "grant")

	var revoke admissionapi.RevokeResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path+"/revoke", admissionapi.RevokeRequest{ReservationID: grant.ReservationID}, &revoke))
	require.Equal(t, workspace.ID, testutil.RequireReceive(ctx, t, events), "revoke")

	// The member sees the requeued admission on the workspace.
	got, err := env.member.Workspace(ctx, workspace.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LatestBuild.Admission)
	require.Equal(t, codersdk.WorkspaceBuildAdmissionStatusWaiting, got.LatestBuild.Admission.Status)
}

func TestWatch(t *testing.T) {
	t.Parallel()
	clock := quartz.NewMock(t)
	env := newAPIEnv(t, clock)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	var watch admissionapi.WatchResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/watch", nil, &watch))
	require.True(t, watch.Changed)
	cursor := watch.Cursor

	// Without changes, the long poll ends at the timeout.
	trap := clock.Trap().NewTimer("admissionapi", "watch")
	defer trap.Close()
	result := make(chan admissionapi.WatchResponse, 1)
	go func() {
		var resp admissionapi.WatchResponse
		_ = callWithToken(ctx, t, env.url, testToken, http.MethodGet, "/v1/watch?cursor="+cursor, nil, &resp)
		result <- resp
	}()
	trap.MustWait(ctx).MustRelease(ctx)
	clock.Advance(admissionapi.MaxWatchTimeout).MustWait(ctx)
	timedOut := testutil.RequireReceive(ctx, t, result)
	require.False(t, timedOut.Changed)
	require.Equal(t, cursor, timedOut.Cursor)

	// A new START wakes the long poll early.
	go func() {
		var resp admissionapi.WatchResponse
		_ = callWithToken(ctx, t, env.url, testToken, http.MethodGet, "/v1/watch?cursor="+cursor, nil, &resp)
		result <- resp
	}()
	trap.MustWait(ctx).MustRelease(ctx)
	env.waitingWorkspace(t, "small")
	woken := testutil.RequireReceive(ctx, t, result)
	require.True(t, woken.Changed)
	require.NotEqual(t, cursor, woken.Cursor)

	// A stale cursor returns immediately.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/watch?cursor="+cursor, nil, &watch))
	require.True(t, watch.Changed)
	require.Equal(t, woken.Cursor, watch.Cursor)
}

func TestTemplatesAndEntitlements(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	var errResp admissionapi.ErrorResponse

	var templates admissionapi.ListTemplatesResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/templates", nil, &templates))
	require.Len(t, templates.Templates, 1)
	require.Equal(t, env.template.ID, templates.Templates[0].TemplateID)
	require.Equal(t, env.orgID, templates.Templates[0].OrganizationID)

	other := env.unmanaged
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodGet, "/v1/templates/"+other.ID.String(), nil, &errResp))
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPut, "/v1/templates/"+uuid.NewString(), admissionapi.PutTemplateRequest{ProfileParameter: "p"}, &errResp))
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, "/v1/templates/"+other.ID.String(), admissionapi.PutTemplateRequest{}, &errResp))

	// Entitlements for an unmanaged template fail and keep the old ones.
	userPath := "/v1/entitlements/" + env.memberID.String()
	require.Equal(t, http.StatusBadRequest, env.call(ctx, t, http.MethodPut, userPath, admissionapi.PutEntitlementsRequest{
		Templates: []admissionapi.TemplateEntitlement{
			{TemplateID: env.template.ID, AllowedProfiles: []string{"small"}},
			{TemplateID: other.ID, AllowedProfiles: []string{"small"}},
		},
	}, &errResp))
	require.Equal(t, admissionapi.CodeTemplateNotManaged, errResp.Code)
	var ents admissionapi.Entitlements
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, userPath, nil, &ents))
	require.Len(t, ents.Templates, 1)
	require.ElementsMatch(t, []string{"small", "large"}, ents.Templates[0].AllowedProfiles)

	var tpl admissionapi.Template
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, "/v1/templates/"+other.ID.String(), admissionapi.PutTemplateRequest{ProfileParameter: "size"}, &tpl))
	require.Equal(t, "size", tpl.ProfileParameter)

	// Replace all entitlements of the user.
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPut, userPath, admissionapi.PutEntitlementsRequest{
		Templates: []admissionapi.TemplateEntitlement{{TemplateID: other.ID, AllowedProfiles: []string{"xl"}}},
	}, &ents))
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, userPath, nil, &ents))
	require.Len(t, ents.Templates, 1)
	require.Equal(t, other.ID, ents.Templates[0].TemplateID)
	require.Equal(t, []string{"xl"}, ents.Templates[0].AllowedProfiles)

	// The member lost the entitlement for the first template.
	_, err := env.member.CreateUserWorkspace(ctx, codersdk.Me, codersdk.CreateWorkspaceRequest{
		TemplateID:          env.template.ID,
		Name:                "noentitlement",
		RichParameterValues: admissiontest.Profile("small"),
	})
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusForbidden, sdkErr.StatusCode())

	// Deleting a managed template removes its entitlements.
	var deleted admissionapi.DeleteTemplateResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodDelete, "/v1/templates/"+other.ID.String(), nil, &deleted))
	require.True(t, deleted.Deleted)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodDelete, "/v1/templates/"+other.ID.String(), nil, &deleted))
	require.False(t, deleted.Deleted)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, userPath, nil, &ents))
	require.Empty(t, ents.Templates)
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodGet, "/v1/entitlements/"+uuid.NewString(), nil, &errResp))
}

func TestUsers(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	dbgen.UserLink(t, env.db, database.UserLink{
		UserID:    env.memberID,
		LoginType: database.LoginTypeOIDC,
		LinkedID:  "https://issuer.example.com||subject-123",
	})

	var users admissionapi.ListUsersResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/users", nil, &users))
	var member *admissionapi.User
	for i := range users.Users {
		if users.Users[i].ID == env.memberID {
			member = &users.Users[i]
		}
	}
	require.NotNil(t, member)
	require.Equal(t, "https://issuer.example.com", member.OIDCIssuer)
	require.Equal(t, "subject-123", member.OIDCSubject)
	require.Equal(t, "active", member.Status)

	// Suspension revokes existing sessions immediately.
	_, err := env.member.User(ctx, codersdk.Me)
	require.NoError(t, err)
	var user admissionapi.User
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/users/"+env.memberID.String()+"/suspend", admissionapi.UserStatusRequest{Reason: "left the company"}, &user))
	require.Equal(t, "suspended", user.Status)
	_, err = env.member.User(ctx, codersdk.Me)
	var sdkErr *codersdk.Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, http.StatusUnauthorized, sdkErr.StatusCode())

	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/users/"+env.memberID.String()+"/activate", nil, &user))
	require.Equal(t, "active", user.Status)

	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/users/"+database.PrebuildsSystemUserID.String()+"/suspend", nil, &errResp))
	require.Equal(t, admissionapi.CodeSystemUser, errResp.Code)
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, "/v1/users/"+uuid.NewString()+"/suspend", nil, &errResp))
}

func TestStopWorkspace(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)
	workspace := env.waitingWorkspace(t, "small")
	startJobID := workspace.LatestBuild.Job.ID

	var list admissionapi.ListWorkspacesResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodGet, "/v1/workspaces", nil, &list))
	require.Len(t, list.Workspaces, 1)
	require.Equal(t, workspace.ID, list.Workspaces[0].ID)
	require.Equal(t, env.memberID, list.Workspaces[0].OwnerID)
	require.Equal(t, "start", list.Workspaces[0].LatestBuild.Transition)
	require.Equal(t, "waiting", list.Workspaces[0].LatestBuild.AdmissionStatus)
	require.Equal(t, "pending", list.Workspaces[0].LatestBuild.Job.Status)

	// Stopping cancels the pending START and creates the STOP together.
	op := uuid.New()
	path := "/v1/workspaces/" + workspace.ID.String() + "/stop"
	var stop admissionapi.StopWorkspaceResponse
	require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, path, admissionapi.StopWorkspaceRequest{OperationID: op, Reason: "quota"}, &stop))
	require.True(t, stop.Created)
	require.Equal(t, "stop", stop.Build.Transition)
	require.Equal(t, "pending", stop.Build.Job.Status)
	require.Empty(t, stop.Build.AdmissionStatus)
	require.NotNil(t, stop.CanceledJobID)
	require.Equal(t, startJobID, *stop.CanceledJobID)
	admission := admissiontest.Admission(t, env.db, startJobID)
	require.Equal(t, database.WorkspaceAdmissionStatusCanceled, admission.Status)
	startJob, err := env.db.GetProvisionerJobByID(ctx, startJobID)
	require.NoError(t, err)
	require.Equal(t, database.ProvisionerJobStatusCanceled, startJob.JobStatus)

	stopBuild, err := env.member.WorkspaceBuild(ctx, stop.Build.BuildID)
	require.NoError(t, err)
	require.Equal(t, codersdk.BuildReasonAutostop, stopBuild.Reason)

	// The same operation returns the original STOP build.
	var again admissionapi.StopWorkspaceResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, path, admissionapi.StopWorkspaceRequest{OperationID: op}, &again))
	require.False(t, again.Created)
	require.Equal(t, stop.Build.BuildID, again.Build.BuildID)
	require.Equal(t, startJobID, *again.CanceledJobID)

	// Another operation conflicts with the pending STOP.
	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, path, admissionapi.StopWorkspaceRequest{OperationID: uuid.New()}, &errResp))
	require.Equal(t, admissionapi.CodeActiveBuild, errResp.Code)
	require.NotNil(t, errResp.Job)
	require.Equal(t, "stop", errResp.Job.Transition)

	// The operation belongs to this workspace only.
	otherWorkspace := env.waitingWorkspace(t, "small")
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/workspaces/"+otherWorkspace.ID.String()+"/stop", admissionapi.StopWorkspaceRequest{OperationID: op}, &errResp))
	require.Equal(t, admissionapi.CodeOperationConflict, errResp.Code)

	// A START that a provisioner already runs is reported, not canceled.
	// Finish the pending STOP first, since STOP builds are acquired first.
	now := time.Now()
	require.NoError(t, env.db.UpdateProvisionerJobWithCompleteWithStartedAtByID(ctx, database.UpdateProvisionerJobWithCompleteWithStartedAtByIDParams{
		ID:          stop.Build.JobID,
		UpdatedAt:   now,
		CompletedAt: sql.NullTime{Time: now, Valid: true},
		Error:       sql.NullString{},
		ErrorCode:   sql.NullString{},
		StartedAt:   sql.NullTime{Time: now, Valid: true},
	}))
	var req admissionapi.Request
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/requests/"+otherWorkspace.LatestBuild.Job.ID.String()+"/grant", grantBody("small"), &req))
	acquired, err := admissiontest.Acquire(ctx, env.db, env.orgID)
	require.NoError(t, err)
	require.Equal(t, otherWorkspace.LatestBuild.Job.ID, acquired.ID)
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/workspaces/"+otherWorkspace.ID.String()+"/stop", admissionapi.StopWorkspaceRequest{OperationID: uuid.New()}, &errResp))
	require.Equal(t, admissionapi.CodeActiveBuild, errResp.Code)
	require.Equal(t, "running", errResp.Job.Job.Status)

	// Workspaces of unmanaged templates are not visible.
	unmanagedWorkspace := coderdtest.CreateWorkspace(t, env.owner, env.unmanaged.ID)
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodGet, "/v1/workspaces/"+unmanagedWorkspace.ID.String(), nil, &errResp))
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, "/v1/workspaces/"+unmanagedWorkspace.ID.String()+"/stop", admissionapi.StopWorkspaceRequest{OperationID: uuid.New()}, &errResp))
}

func TestCancelJob(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)
	ctx := testutil.Context(t, testutil.WaitSuperLong)

	waiting := env.waitingWorkspace(t, "small")
	var resp admissionapi.CancelJobResponse
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/jobs/"+waiting.LatestBuild.Job.ID.String()+"/cancel", nil, &resp))
	require.True(t, resp.CanceledBeforeStart)
	require.Equal(t, "canceled", resp.Job.Status)
	require.Equal(t, database.WorkspaceAdmissionStatusCanceled, admissiontest.Admission(t, env.db, waiting.LatestBuild.Job.ID).Status)
	var errResp admissionapi.ErrorResponse
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/jobs/"+waiting.LatestBuild.Job.ID.String()+"/cancel", nil, &errResp))
	require.Equal(t, admissionapi.CodeJobCompleted, errResp.Code)

	running := env.waitingWorkspace(t, "small")
	var req admissionapi.Request
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/requests/"+running.LatestBuild.Job.ID.String()+"/grant", grantBody("small"), &req))
	_, err := admissiontest.Acquire(ctx, env.db, env.orgID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, env.call(ctx, t, http.MethodPost, "/v1/jobs/"+running.LatestBuild.Job.ID.String()+"/cancel", nil, &resp))
	require.False(t, resp.CanceledBeforeStart)
	require.Equal(t, "canceling", resp.Job.Status)
	require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admissiontest.Admission(t, env.db, running.LatestBuild.Job.ID).Status)
	require.Equal(t, http.StatusConflict, env.call(ctx, t, http.MethodPost, "/v1/jobs/"+running.LatestBuild.Job.ID.String()+"/cancel", nil, &errResp))
	require.Equal(t, admissionapi.CodeJobCanceling, errResp.Code)

	// Jobs outside managed templates cannot be canceled here.
	version := coderdtest.CreateTemplateVersion(t, env.owner, env.orgID, nil)
	require.Equal(t, http.StatusNotFound, env.call(ctx, t, http.MethodPost, "/v1/jobs/"+version.Job.ID.String()+"/cancel", nil, &errResp))
}

// TestGrantCancelAcquireRace runs grant, the user cancel endpoint and a
// provisioner acquisition concurrently. A canceled request never starts, and
// a started job always has an acquired admission.
func TestGrantCancelAcquireRace(t *testing.T) {
	t.Parallel()
	env := newAPIEnv(t, nil)

	for i := range 10 {
		ctx := testutil.Context(t, testutil.WaitSuperLong)
		workspace := env.waitingWorkspace(t, "small")
		jobID := workspace.LatestBuild.Job.ID

		var (
			start = make(chan struct{})
			done  = make(chan struct{}, 3)
		)
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			_ = callWithToken(ctx, t, env.url, testToken, http.MethodPost, "/v1/requests/"+jobID.String()+"/grant", grantBody("small"), nil)
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			_ = env.owner.CancelWorkspaceBuild(ctx, workspace.LatestBuild.ID, codersdk.CancelWorkspaceBuildParams{})
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			// Keep trying until the grant or the cancel settles the job.
			for range 20 {
				if _, err := admissiontest.Acquire(ctx, env.db, env.orgID); err == nil {
					return
				}
			}
		}()
		close(start)
		for range 3 {
			<-done
		}

		job, err := env.db.GetProvisionerJobByID(ctx, jobID)
		require.NoError(t, err)
		admission := admissiontest.Admission(t, env.db, jobID)
		if job.StartedAt.Valid && job.WorkerID.Valid {
			require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admission.Status, "iteration %d", i)
		}
		if admission.Status == database.WorkspaceAdmissionStatusCanceled {
			require.False(t, job.WorkerID.Valid, "iteration %d: a canceled request must never start", i)
			require.Equal(t, database.ProvisionerJobStatusCanceled, job.JobStatus, "iteration %d", i)
		}
		require.NotEqual(t, database.WorkspaceAdmissionStatusGranted, admission.Status, fmt.Sprintf("iteration %d: the cancel must settle the request", i))

		// Finish any running job so the next iteration acquires its own.
		if !job.CompletedAt.Valid {
			now := time.Now()
			err = env.db.UpdateProvisionerJobWithCompleteByID(ctx, database.UpdateProvisionerJobWithCompleteByIDParams{
				ID:          jobID,
				UpdatedAt:   now,
				CompletedAt: sql.NullTime{Time: now, Valid: true},
				Error:       sql.NullString{},
				ErrorCode:   sql.NullString{},
			})
			require.NoError(t, err)
		}
	}
}
