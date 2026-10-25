package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/provisionerdserver"
	"github.com/coder/coder/v2/testutil"
)

const admissionProfileParameter = "profile"

// admissionFixture is a managed template with one owner, used to create
// workspace builds whose admission state the tests control.
type admissionFixture struct {
	db       database.Store
	org      database.Organization
	owner    database.User
	template database.Template
	version  database.TemplateVersion
}

// newManagedAdmissionFixture is newAdmissionFixture with a managed template.
func newManagedAdmissionFixture(t *testing.T, db database.Store) admissionFixture {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newAdmissionFixture(t, db)
	_, err := db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: admissionProfileParameter,
		Now:              dbtime.Now(),
		TemplateID:       f.template.ID,
	})
	require.NoError(t, err)
	return f
}

func newAdmissionFixture(t *testing.T, db database.Store) admissionFixture {
	t.Helper()

	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	importJob := dbgen.ProvisionerJob(t, db, nil, database.ProvisionerJob{
		OrganizationID: org.ID,
		Type:           database.ProvisionerJobTypeTemplateVersionImport,
		StartedAt:      sql.NullTime{Time: dbtime.Now(), Valid: true},
		CompletedAt:    sql.NullTime{Time: dbtime.Now(), Valid: true},
	})
	version := dbgen.TemplateVersion(t, db, database.TemplateVersion{
		OrganizationID: org.ID,
		CreatedBy:      owner.ID,
		JobID:          importJob.ID,
	})
	tpl := dbgen.Template(t, db, database.Template{
		OrganizationID:  org.ID,
		CreatedBy:       owner.ID,
		ActiveVersionID: version.ID,
	})
	return admissionFixture{db: db, org: org, owner: owner, template: tpl, version: version}
}

type admissionBuild struct {
	workspace database.WorkspaceTable
	build     database.WorkspaceBuild
	job       database.ProvisionerJob
}

// build creates a pending workspace build job whose last update is at
// updatedAt.
func (f admissionFixture) build(t *testing.T, transition database.WorkspaceTransition, profile string, updatedAt time.Time) admissionBuild {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)

	ws := dbgen.Workspace(t, f.db, database.WorkspaceTable{
		OrganizationID: f.org.ID,
		OwnerID:        f.owner.ID,
		TemplateID:     f.template.ID,
	})
	job, err := f.db.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
		ID:             uuid.New(),
		CreatedAt:      updatedAt,
		UpdatedAt:      updatedAt,
		OrganizationID: f.org.ID,
		InitiatorID:    f.owner.ID,
		Provisioner:    database.ProvisionerTypeEcho,
		StorageMethod:  database.ProvisionerStorageMethodFile,
		FileID:         uuid.New(),
		Type:           database.ProvisionerJobTypeWorkspaceBuild,
		Input:          json.RawMessage(`{}`),
		Tags:           database.StringMap{},
		TraceMetadata:  pqtype.NullRawMessage{},
		LogsOverflowed: false,
	})
	require.NoError(t, err)
	build := dbgen.WorkspaceBuild(t, f.db, database.WorkspaceBuild{
		WorkspaceID:       ws.ID,
		TemplateVersionID: f.version.ID,
		JobID:             job.ID,
		Transition:        transition,
		InitiatorID:       f.owner.ID,
		BuildNumber:       1,
	})
	if profile != "" {
		dbgen.WorkspaceBuildParameters(t, f.db, []database.WorkspaceBuildParameter{{
			WorkspaceBuildID: build.ID,
			Name:             admissionProfileParameter,
			Value:            profile,
		}})
	}
	return admissionBuild{workspace: ws, build: build, job: job}
}

func (f admissionFixture) waiting(t *testing.T, b admissionBuild, profile string) database.WorkspaceBuildAdmission {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	admission, err := f.db.InsertWorkspaceBuildAdmission(ctx, database.InsertWorkspaceBuildAdmissionParams{
		JobID:             b.job.ID,
		WorkspaceBuildID:  b.build.ID,
		WorkspaceID:       b.workspace.ID,
		OrganizationID:    f.org.ID,
		OwnerID:           f.owner.ID,
		TemplateID:        f.template.ID,
		TemplateVersionID: f.version.ID,
		Profile:           profile,
		Reason:            "waiting_for_capacity",
		QueuedAt:          b.job.CreatedAt,
	})
	require.NoError(t, err)
	return admission
}

func (f admissionFixture) grant(t *testing.T, jobID uuid.UUID, grantedAt, expiresAt time.Time) database.WorkspaceBuildAdmission {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitLong)
	admission, err := f.db.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
		ReservationID:  uuid.New(),
		Generation:     1,
		OperationID:    uuid.New(),
		GrantExpiresAt: expiresAt,
		Now:            grantedAt,
		JobID:          jobID,
	})
	require.NoError(t, err)
	return admission
}

func acquireJob(ctx context.Context, t *testing.T, db database.Store, orgID uuid.UUID) (database.ProvisionerJob, error) {
	t.Helper()
	return db.AcquireProvisionerJob(ctx, database.AcquireProvisionerJobParams{
		OrganizationID:  orgID,
		StartedAt:       sql.NullTime{Time: dbtime.Now(), Valid: true},
		WorkerID:        uuid.NullUUID{UUID: uuid.New(), Valid: true},
		Types:           []database.ProvisionerType{database.ProvisionerTypeEcho},
		ProvisionerTags: json.RawMessage(`{}`),
	})
}

func TestAcquireProvisionerJobWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	now := dbtime.Now()
	cases := []struct {
		name string
		// setup prepares the admission of b and reports whether the job
		// must be acquirable.
		setup func(t *testing.T, f admissionFixture, b admissionBuild) bool
	}{
		{
			name: "NoAdmissionRecord",
			setup: func(*testing.T, admissionFixture, admissionBuild) bool {
				return false
			},
		},
		{
			name: "Waiting",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				f.waiting(t, b, "small")
				return false
			},
		},
		{
			name: "GrantExpired",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				f.waiting(t, b, "small")
				f.grant(t, b.job.ID, now.Add(-time.Hour), now.Add(-time.Minute))
				return false
			},
		},
		{
			name: "ProfileMismatch",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				f.waiting(t, b, "large")
				f.grant(t, b.job.ID, now, now.Add(time.Hour))
				return false
			},
		},
		{
			name: "TemplateVersionMismatch",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				ctx := testutil.Context(t, testutil.WaitLong)
				otherJob := dbgen.ProvisionerJob(t, f.db, nil, database.ProvisionerJob{
					OrganizationID: f.org.ID,
					Type:           database.ProvisionerJobTypeTemplateVersionImport,
					StartedAt:      sql.NullTime{Time: now, Valid: true},
					CompletedAt:    sql.NullTime{Time: now, Valid: true},
				})
				other := dbgen.TemplateVersion(t, f.db, database.TemplateVersion{
					OrganizationID: f.org.ID,
					CreatedBy:      f.owner.ID,
					JobID:          otherJob.ID,
				})
				_, err := f.db.InsertWorkspaceBuildAdmission(ctx, database.InsertWorkspaceBuildAdmissionParams{
					JobID:             b.job.ID,
					WorkspaceBuildID:  b.build.ID,
					WorkspaceID:       b.workspace.ID,
					OrganizationID:    f.org.ID,
					OwnerID:           f.owner.ID,
					TemplateID:        f.template.ID,
					TemplateVersionID: other.ID,
					Profile:           "small",
					Reason:            "",
					QueuedAt:          now,
				})
				require.NoError(t, err)
				f.grant(t, b.job.ID, now, now.Add(time.Hour))
				return false
			},
		},
		{
			name: "OwnerMismatch",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				ctx := testutil.Context(t, testutil.WaitLong)
				other := dbgen.User(t, f.db, database.User{})
				_, err := f.db.InsertWorkspaceBuildAdmission(ctx, database.InsertWorkspaceBuildAdmissionParams{
					JobID:             b.job.ID,
					WorkspaceBuildID:  b.build.ID,
					WorkspaceID:       b.workspace.ID,
					OrganizationID:    f.org.ID,
					OwnerID:           other.ID,
					TemplateID:        f.template.ID,
					TemplateVersionID: f.version.ID,
					Profile:           "small",
					Reason:            "",
					QueuedAt:          now,
				})
				require.NoError(t, err)
				f.grant(t, b.job.ID, now, now.Add(time.Hour))
				return false
			},
		},
		{
			name: "Requeued",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				ctx := testutil.Context(t, testutil.WaitLong)
				f.waiting(t, b, "small")
				f.grant(t, b.job.ID, now, now.Add(time.Hour))
				_, err := f.db.RequeueWorkspaceBuildAdmission(ctx, database.RequeueWorkspaceBuildAdmissionParams{
					Reason: "waiting_for_capacity",
					Now:    now,
					JobID:  b.job.ID,
				})
				require.NoError(t, err)
				return false
			},
		},
		{
			name: "Granted",
			setup: func(t *testing.T, f admissionFixture, b admissionBuild) bool {
				f.waiting(t, b, "small")
				f.grant(t, b.job.ID, now, now.Add(time.Hour))
				return true
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, _ := dbtestutil.NewDB(t)
			ctx := testutil.Context(t, testutil.WaitLong)
			f := newManagedAdmissionFixture(t, db)
			b := f.build(t, database.WorkspaceTransitionStart, "small", now)

			acquirable := tc.setup(t, f, b)

			job, err := acquireJob(ctx, t, db, f.org.ID)
			if !acquirable {
				require.ErrorIs(t, err, sql.ErrNoRows)
				pending, err := db.GetProvisionerJobByID(ctx, b.job.ID)
				require.NoError(t, err)
				require.False(t, pending.StartedAt.Valid)
				return
			}
			require.NoError(t, err)
			require.Equal(t, b.job.ID, job.ID)
			admission, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, b.job.ID)
			require.NoError(t, err)
			require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admission.Status)
			require.True(t, admission.AcquiredAt.Valid)

			// An acquired job is never acquired again.
			_, err = acquireJob(ctx, t, db, f.org.ID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}

	t.Run("UnmanagedTemplate", func(t *testing.T) {
		t.Parallel()
		db, _ := dbtestutil.NewDB(t)
		ctx := testutil.Context(t, testutil.WaitLong)
		f := newAdmissionFixture(t, db)
		b := f.build(t, database.WorkspaceTransitionStart, "", now)

		job, err := acquireJob(ctx, t, db, f.org.ID)
		require.NoError(t, err)
		require.Equal(t, b.job.ID, job.ID)
	})

	t.Run("TemplateImport", func(t *testing.T) {
		t.Parallel()
		db, _ := dbtestutil.NewDB(t)
		ctx := testutil.Context(t, testutil.WaitLong)
		f := newManagedAdmissionFixture(t, db)
		importJob := dbgen.ProvisionerJob(t, db, nil, database.ProvisionerJob{
			OrganizationID: f.org.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
			Tags:           database.StringMap{},
		})

		job, err := acquireJob(ctx, t, db, f.org.ID)
		require.NoError(t, err)
		require.Equal(t, importJob.ID, job.ID)
	})

	t.Run("ManagedStopNeedsNoAdmission", func(t *testing.T) {
		t.Parallel()
		db, _ := dbtestutil.NewDB(t)
		ctx := testutil.Context(t, testutil.WaitLong)
		f := newManagedAdmissionFixture(t, db)
		b := f.build(t, database.WorkspaceTransitionStop, "", now)

		job, err := acquireJob(ctx, t, db, f.org.ID)
		require.NoError(t, err)
		require.Equal(t, b.job.ID, job.ID)
	})
}

// TestAcquireProvisionerJobCapacityReleaseFirst verifies that STOP and DELETE
// builds are acquired before older START builds and other jobs, and that the
// queue positions reflect that order.
func TestAcquireProvisionerJobCapacityReleaseFirst(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	_ = dbgen.ProvisionerDaemon(t, db, database.ProvisionerDaemon{})
	f := newManagedAdmissionFixture(t, db)
	now := dbtime.Now()

	managedStart := f.build(t, database.WorkspaceTransitionStart, "small", now.Add(-4*time.Minute))
	f.waiting(t, managedStart, "small")
	f.grant(t, managedStart.job.ID, now, now.Add(time.Hour))
	unmanaged := newAdmissionFixture(t, db)
	unmanagedStart := unmanaged.build(t, database.WorkspaceTransitionStart, "", now.Add(-3*time.Minute))
	importJob, err := db.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
		ID:             uuid.New(),
		CreatedAt:      now.Add(-2 * time.Minute),
		UpdatedAt:      now.Add(-2 * time.Minute),
		OrganizationID: f.org.ID,
		InitiatorID:    f.owner.ID,
		Provisioner:    database.ProvisionerTypeEcho,
		StorageMethod:  database.ProvisionerStorageMethodFile,
		FileID:         uuid.New(),
		Type:           database.ProvisionerJobTypeTemplateVersionImport,
		Input:          json.RawMessage(`{}`),
		Tags:           database.StringMap{},
		TraceMetadata:  pqtype.NullRawMessage{},
		LogsOverflowed: false,
	})
	require.NoError(t, err)
	stop := f.build(t, database.WorkspaceTransitionStop, "", now.Add(-time.Minute))
	deleteBuild := f.build(t, database.WorkspaceTransitionDelete, "", now)

	expected := []uuid.UUID{stop.job.ID, deleteBuild.job.ID, managedStart.job.ID}
	// The unmanaged START and the import belong to another organization or
	// were created later, so check them separately.
	queued, err := db.GetProvisionerJobsByIDsWithQueuePosition(ctx, database.GetProvisionerJobsByIDsWithQueuePositionParams{
		IDs:             append(expected, importJob.ID),
		StaleIntervalMS: provisionerdserver.StaleInterval.Milliseconds(),
	})
	require.NoError(t, err)
	positions := map[uuid.UUID]int64{}
	for _, q := range queued {
		positions[q.ProvisionerJob.ID] = q.QueuePosition
	}
	require.Less(t, positions[stop.job.ID], positions[managedStart.job.ID])
	require.Less(t, positions[deleteBuild.job.ID], positions[managedStart.job.ID])
	require.Less(t, positions[managedStart.job.ID], positions[importJob.ID])

	for _, id := range expected {
		job, err := acquireJob(ctx, t, db, f.org.ID)
		require.NoError(t, err)
		require.Equal(t, id, job.ID)
	}
	job, err := acquireJob(ctx, t, db, f.org.ID)
	require.NoError(t, err)
	require.Equal(t, importJob.ID, job.ID)
	job, err = acquireJob(ctx, t, db, unmanaged.org.ID)
	require.NoError(t, err)
	require.Equal(t, unmanagedStart.job.ID, job.ID)
}

// TestQueuePositionWorkspaceAdmission verifies that managed START builds that
// wait for capacity never take a provisioner queue position.
func TestQueuePositionWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	_ = dbgen.ProvisionerDaemon(t, db, database.ProvisionerDaemon{})
	f := newManagedAdmissionFixture(t, db)
	now := dbtime.Now()

	waiting := f.build(t, database.WorkspaceTransitionStart, "small", now.Add(-2*time.Minute))
	f.waiting(t, waiting, "small")
	granted := f.build(t, database.WorkspaceTransitionStart, "small", now.Add(-time.Minute))
	f.waiting(t, granted, "small")
	f.grant(t, granted.job.ID, now, now.Add(time.Hour))

	queued, err := db.GetProvisionerJobsByIDsWithQueuePosition(ctx, database.GetProvisionerJobsByIDsWithQueuePositionParams{
		IDs:             []uuid.UUID{waiting.job.ID, granted.job.ID},
		StaleIntervalMS: provisionerdserver.StaleInterval.Milliseconds(),
	})
	require.NoError(t, err)
	require.Len(t, queued, 2)
	for _, q := range queued {
		switch q.ProvisionerJob.ID {
		case waiting.job.ID:
			require.Zero(t, q.QueuePosition)
		case granted.job.ID:
			require.EqualValues(t, 1, q.QueuePosition)
			require.EqualValues(t, 1, q.QueueSize)
		}
	}

	rows, err := db.GetProvisionerJobsByOrganizationAndStatusWithQueuePositionAndProvisioner(ctx, database.GetProvisionerJobsByOrganizationAndStatusWithQueuePositionAndProvisionerParams{
		OrganizationID: f.org.ID,
		IDs:            []uuid.UUID{waiting.job.ID, granted.job.ID},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		switch row.ProvisionerJob.ID {
		case waiting.job.ID:
			require.Zero(t, row.QueuePosition)
		case granted.job.ID:
			require.EqualValues(t, 1, row.QueuePosition)
			require.EqualValues(t, 1, row.QueueSize)
		}
	}
}

func TestGetProvisionerJobsToBeReapedWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newManagedAdmissionFixture(t, db)
	unmanaged := newAdmissionFixture(t, db)
	now := dbtime.Now()
	old := now.Add(-2 * time.Hour)

	waiting := f.build(t, database.WorkspaceTransitionStart, "small", old)
	f.waiting(t, waiting, "small")

	recentlyGranted := f.build(t, database.WorkspaceTransitionStart, "small", old)
	f.waiting(t, recentlyGranted, "small")
	f.grant(t, recentlyGranted.job.ID, now.Add(-time.Minute), now.Add(time.Hour))

	staleGrant := f.build(t, database.WorkspaceTransitionStart, "small", old)
	f.waiting(t, staleGrant, "small")
	f.grant(t, staleGrant.job.ID, now.Add(-time.Hour), now.Add(time.Hour))

	unmanagedPending := unmanaged.build(t, database.WorkspaceTransitionStart, "", old)
	managedStop := f.build(t, database.WorkspaceTransitionStop, "", old)
	missingAdmission := f.build(t, database.WorkspaceTransitionStart, "small", old)

	jobs, err := db.GetProvisionerJobsToBeReaped(ctx, database.GetProvisionerJobsToBeReapedParams{
		PendingSince: now.Add(-30 * time.Minute),
		HungSince:    now.Add(-5 * time.Minute),
		MaxJobs:      100,
	})
	require.NoError(t, err)
	reaped := map[uuid.UUID]bool{}
	for _, job := range jobs {
		reaped[job.ID] = true
	}
	require.False(t, reaped[waiting.job.ID], "waiting for capacity must never be reaped")
	require.False(t, reaped[recentlyGranted.job.ID], "granted jobs are measured from the grant")
	require.True(t, reaped[staleGrant.job.ID])
	require.True(t, reaped[unmanagedPending.job.ID])
	require.True(t, reaped[managedStop.job.ID])
	require.True(t, reaped[missingAdmission.job.ID])
}

func TestWorkspaceAdmissionGrantConstraints(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newManagedAdmissionFixture(t, db)
	now := dbtime.Now()
	b := f.build(t, database.WorkspaceTransitionStart, "small", now)
	f.waiting(t, b, "small")

	// Only rejected, canceled and revoked are valid end states.
	_, err := db.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
		Status: database.WorkspaceAdmissionStatusGranted,
		Reason: "",
		Now:    now,
		JobID:  b.job.ID,
	})
	require.ErrorIs(t, err, sql.ErrNoRows)

	first := f.grant(t, b.job.ID, now.Add(-time.Minute), now.Add(time.Minute))
	refreshed, err := db.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
		ReservationID:  first.ReservationID.UUID,
		Generation:     2,
		OperationID:    first.OperationID.UUID,
		GrantExpiresAt: now.Add(time.Hour),
		Now:            now,
		JobID:          b.job.ID,
	})
	require.NoError(t, err)
	require.True(t, refreshed.GrantedAt.Time.Equal(first.GrantedAt.Time), "refreshing a grant keeps the first grant time")
	require.EqualValues(t, 2, refreshed.Generation.Int64)

	ended, err := db.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
		Status: database.WorkspaceAdmissionStatusCanceled,
		Reason: "canceled_before_start",
		Now:    now,
		JobID:  b.job.ID,
	})
	require.NoError(t, err)
	require.Equal(t, database.WorkspaceAdmissionStatusCanceled, ended.Status)

	// Ended admissions can neither be granted again nor requeued.
	_, err = db.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
		ReservationID:  uuid.New(),
		Generation:     3,
		OperationID:    uuid.New(),
		GrantExpiresAt: now.Add(time.Hour),
		Now:            now,
		JobID:          b.job.ID,
	})
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = db.RequeueWorkspaceBuildAdmission(ctx, database.RequeueWorkspaceBuildAdmissionParams{
		Reason: "",
		Now:    now,
		JobID:  b.job.ID,
	})
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestAcquireProvisionerJobRevokeRace races acquisition against returning the
// grant to the capacity queue under the job row lock. Either the provisioner
// acquires the job and the revoke observes it, or the revoke wins and the job
// is never acquired.
func TestAcquireProvisionerJobRevokeRace(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	f := newManagedAdmissionFixture(t, db)
	now := dbtime.Now()

	for i := range 25 {
		ctx := testutil.Context(t, testutil.WaitLong)
		b := f.build(t, database.WorkspaceTransitionStart, "small", now)
		f.waiting(t, b, "small")
		f.grant(t, b.job.ID, now, now.Add(time.Hour))

		var (
			acquired    database.ProvisionerJob
			acquireErr  error
			sawAcquired bool
			requeued    bool
			revokeErr   error
			start       = make(chan struct{})
			done        = make(chan struct{}, 2)
		)
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			acquired, acquireErr = acquireJob(ctx, t, db, f.org.ID)
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			revokeErr = db.InTx(func(tx database.Store) error {
				job, err := tx.GetProvisionerJobByIDWithLock(ctx, b.job.ID)
				if err != nil {
					return err
				}
				if job.StartedAt.Valid {
					sawAcquired = true
					return nil
				}
				_, err = tx.RequeueWorkspaceBuildAdmission(ctx, database.RequeueWorkspaceBuildAdmissionParams{
					Reason: "waiting_for_capacity",
					Now:    dbtime.Now(),
					JobID:  b.job.ID,
				})
				requeued = err == nil
				return err
			}, nil)
		}()
		close(start)
		<-done
		<-done

		require.NoError(t, revokeErr, "iteration %d", i)
		admission, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, b.job.ID)
		require.NoError(t, err)
		job, err := db.GetProvisionerJobByID(ctx, b.job.ID)
		require.NoError(t, err)
		if acquireErr == nil {
			require.Equal(t, b.job.ID, acquired.ID)
			require.True(t, sawAcquired, "iteration %d: revoke must observe the acquisition", i)
			require.False(t, requeued, "iteration %d", i)
			require.Equal(t, database.WorkspaceAdmissionStatusAcquired, admission.Status)
		} else {
			require.ErrorIs(t, acquireErr, sql.ErrNoRows, "iteration %d", i)
			require.True(t, requeued, "iteration %d", i)
			require.False(t, job.StartedAt.Valid, "iteration %d: a requeued job must not start", i)
			require.Equal(t, database.WorkspaceAdmissionStatusWaiting, admission.Status)
		}

		// Finish the job so later iterations only see their own job.
		err = db.UpdateProvisionerJobWithCompleteWithStartedAtByID(ctx, database.UpdateProvisionerJobWithCompleteWithStartedAtByIDParams{
			ID:          b.job.ID,
			UpdatedAt:   dbtime.Now(),
			CompletedAt: sql.NullTime{Time: dbtime.Now(), Valid: true},
			Error:       sql.NullString{},
			ErrorCode:   sql.NullString{},
			StartedAt:   sql.NullTime{Time: dbtime.Now(), Valid: true},
		})
		require.NoError(t, err)
	}
}
