package admissionapi_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/testutil"
)

type deleteRetryBarrier struct {
	operationID  uuid.UUID
	reads        atomic.Int32
	seen         [2]chan struct{}
	resume       [2]chan struct{}
	grantCalls   atomic.Int32
	grantLocked  chan struct{}
	grantWaiting chan struct{}
	releaseGrant chan struct{}
}

type deleteRetryStore struct {
	database.Store
	barrier *deleteRetryBarrier
}

func (db deleteRetryStore) InTx(fn func(database.Store) error, opts *database.TxOptions) error {
	return db.Store.InTx(func(tx database.Store) error {
		return fn(deleteRetryStore{Store: tx, barrier: db.barrier})
	}, opts)
}

func (db deleteRetryStore) GetWorkspaceAdmissionDeleteOperationByOperationID(ctx context.Context, id uuid.UUID) (database.WorkspaceAdmissionDeleteOperation, error) {
	op, err := db.Store.GetWorkspaceAdmissionDeleteOperationByOperationID(ctx, id)
	if id == db.barrier.operationID && errors.Is(err, sql.ErrNoRows) {
		if i := db.barrier.reads.Add(1) - 1; i < 2 {
			close(db.barrier.seen[i])
			select {
			case <-db.barrier.resume[i]:
			case <-ctx.Done():
				return op, ctx.Err()
			}
		}
	}
	return op, err
}

func (db deleteRetryStore) GetWorkspaceAdmissionGrantByIDWithLock(ctx context.Context, id string) (database.WorkspaceAdmissionGrant, error) {
	i := db.barrier.grantCalls.Add(1)
	if i == 2 {
		close(db.barrier.grantWaiting)
	}
	grant, err := db.Store.GetWorkspaceAdmissionGrantByIDWithLock(ctx, id)
	if i == 1 && err == nil {
		close(db.barrier.grantLocked)
		select {
		case <-db.barrier.releaseGrant:
		case <-ctx.Done():
			return grant, ctx.Err()
		}
	}
	return grant, err
}

func TestDeleteWorkspaceConcurrentRetry(t *testing.T) {
	t.Parallel()
	for _, underGrant := range []bool{false, true} {
		name := "Employee"
		if underGrant {
			name = "GrantLock"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			barrier := &deleteRetryBarrier{
				operationID: uuid.New(),
				seen:        [2]chan struct{}{make(chan struct{}), make(chan struct{})},
				resume:      [2]chan struct{}{make(chan struct{}), make(chan struct{})},
				grantLocked: make(chan struct{}), grantWaiting: make(chan struct{}), releaseGrant: make(chan struct{}),
			}
			env := newAPIEnv(t, nil, func(opts *admissionapi.Options) {
				opts.Database = deleteRetryStore{Store: opts.Database, barrier: barrier}
			})
			ctx := testutil.Context(t, testutil.WaitSuperLong)
			var workspaceID uuid.UUID
			body := admissionapi.DeleteWorkspaceRequest{OperationID: barrier.operationID}
			if underGrant {
				body.GrantID = "g-concurrent-delete"
				env.putGrant(ctx, t, env.grantBody(body.GrantID))
				var created admissionapi.CreateWorkspaceResponse
				require.Equal(t, http.StatusCreated, env.call(ctx, t, http.MethodPost, "/v1/workspaces", env.underGrant(body.GrantID, "agent-delete"), &created))
				workspaceID = created.WorkspaceID
			} else {
				workspaceID = env.waitingWorkspace(t, "small").ID
			}
			type result struct {
				status int
				body   admissionapi.DeleteWorkspaceResponse
			}
			call := func(ch chan<- result) {
				var resp admissionapi.DeleteWorkspaceResponse
				status := env.call(ctx, t, http.MethodPost, "/v1/workspaces/"+workspaceID.String()+"/delete", body, &resp)
				ch <- result{status: status, body: resp}
			}
			first, second := make(chan result, 1), make(chan result, 1)
			go call(first)
			testutil.TryReceive(ctx, t, barrier.seen[0])
			go call(second)
			testutil.TryReceive(ctx, t, barrier.seen[1])
			close(barrier.resume[0])
			if underGrant {
				testutil.TryReceive(ctx, t, barrier.grantLocked)
				close(barrier.resume[1])
				testutil.TryReceive(ctx, t, barrier.grantWaiting)
				require.Eventually(t, func() bool {
					locks, err := env.db.PGLocks(ctx)
					if err != nil {
						return false
					}
					for _, held := range locks {
						if held.RelationName == nil || *held.RelationName != "workspace_admission_grants" {
							continue
						}
						for _, waiting := range locks {
							if waiting.PID == held.PID && !waiting.Granted {
								return true
							}
						}
					}
					return false
				}, testutil.WaitShort, testutil.IntervalFast)
				close(barrier.releaseGrant)
			}
			a := testutil.TryReceive(ctx, t, first)
			require.Equal(t, http.StatusAccepted, a.status)
			if !underGrant {
				close(barrier.resume[1])
			}
			b := testutil.TryReceive(ctx, t, second)
			require.Equal(t, http.StatusAccepted, b.status)
			require.Equal(t, a.body, b.body)
			build, err := env.db.GetLatestWorkspaceBuildByWorkspaceID(ctx, workspaceID)
			require.NoError(t, err)
			require.Equal(t, a.body.BuildID, build.ID)
			require.EqualValues(t, 2, build.BuildNumber)
		})
	}
}
