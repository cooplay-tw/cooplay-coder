package jobreaper_test

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
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/jobreaper"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/testutil"
)

// candidateStore returns fixed reaper candidates so tests exercise the
// post-lock recheck as if the admission changed after candidate selection.
type candidateStore struct {
	database.Store
	candidates []uuid.UUID
}

func (s candidateStore) GetProvisionerJobsToBeReaped(ctx context.Context, _ database.GetProvisionerJobsToBeReapedParams) ([]database.ProvisionerJob, error) {
	jobs := make([]database.ProvisionerJob, 0, len(s.candidates))
	for _, id := range s.candidates {
		job, err := s.GetProvisionerJobByID(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s candidateStore) InTx(f func(database.Store) error, opts *database.TxOptions) error {
	return s.Store.InTx(func(tx database.Store) error {
		return f(candidateStore{Store: tx, candidates: s.candidates})
	}, opts)
}

type managedStartJob struct {
	job database.ProvisionerJob
}

// newManagedStart creates a pending START of a managed template whose job
// was last updated at updatedAt, with a waiting admission.
func newManagedStart(ctx context.Context, t *testing.T, db database.Store, updatedAt time.Time) managedStartJob {
	t.Helper()
	org := dbgen.Organization(t, db, database.Organization{})
	owner := dbgen.User(t, db, database.User{})
	importJob := dbgen.ProvisionerJob(t, db, nil, database.ProvisionerJob{
		OrganizationID: org.ID,
		Type:           database.ProvisionerJobTypeTemplateVersionImport,
		StartedAt:      sql.NullTime{Time: dbtime.Now(), Valid: true},
		CompletedAt:    sql.NullTime{Time: dbtime.Now(), Valid: true},
	})
	version := dbgen.TemplateVersion(t, db, database.TemplateVersion{OrganizationID: org.ID, CreatedBy: owner.ID, JobID: importJob.ID})
	tpl := dbgen.Template(t, db, database.Template{OrganizationID: org.ID, CreatedBy: owner.ID, ActiveVersionID: version.ID})
	_, err := db.UpsertWorkspaceAdmissionTemplate(ctx, database.UpsertWorkspaceAdmissionTemplateParams{
		ProfileParameter: "profile",
		Now:              dbtime.Now(),
		TemplateID:       tpl.ID,
	})
	require.NoError(t, err)
	ws := dbgen.Workspace(t, db, database.WorkspaceTable{OrganizationID: org.ID, OwnerID: owner.ID, TemplateID: tpl.ID})
	job, err := db.InsertProvisionerJob(ctx, database.InsertProvisionerJobParams{
		ID:             uuid.New(),
		CreatedAt:      updatedAt,
		UpdatedAt:      updatedAt,
		OrganizationID: org.ID,
		InitiatorID:    owner.ID,
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
	build := dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
		WorkspaceID:       ws.ID,
		TemplateVersionID: version.ID,
		JobID:             job.ID,
		Transition:        database.WorkspaceTransitionStart,
		InitiatorID:       owner.ID,
		BuildNumber:       1,
	})
	_, err = db.InsertWorkspaceBuildAdmission(ctx, database.InsertWorkspaceBuildAdmissionParams{
		JobID:             job.ID,
		WorkspaceBuildID:  build.ID,
		WorkspaceID:       ws.ID,
		OrganizationID:    org.ID,
		OwnerID:           owner.ID,
		TemplateID:        tpl.ID,
		TemplateVersionID: version.ID,
		Profile:           "small",
		Reason:            workspaceadmission.ReasonWaitingForCapacity,
		QueuedAt:          updatedAt,
	})
	require.NoError(t, err)
	return managedStartJob{job: job}
}

func grantAdmission(ctx context.Context, t *testing.T, db database.Store, jobID uuid.UUID, grantedAt time.Time) {
	t.Helper()
	_, err := db.GrantWorkspaceBuildAdmission(ctx, database.GrantWorkspaceBuildAdmissionParams{
		ReservationID:  uuid.New(),
		Generation:     1,
		OperationID:    uuid.New(),
		GrantExpiresAt: grantedAt.Add(time.Hour),
		Now:            grantedAt,
		JobID:          jobID,
	})
	require.NoError(t, err)
}

func runDetectorOnce(ctx context.Context, t *testing.T, db database.Store, ps pubsub.Pubsub, now time.Time) jobreaper.Stats {
	t.Helper()
	log := testutil.Logger(t)
	tickCh := make(chan time.Time)
	statsCh := make(chan jobreaper.Stats)
	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), ps, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	defer func() {
		detector.Close()
		detector.Wait()
	}()
	testutil.RequireSend(ctx, t, tickCh, now)
	return testutil.RequireReceive(ctx, t, statsCh)
}

func TestDetectorWorkspaceAdmission(t *testing.T) {
	t.Parallel()

	t.Run("WaitingForCapacityNeverReaped", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		db, ps := dbtestutil.NewDB(t)
		now := time.Now()
		managed := newManagedStart(ctx, t, db, now.Add(-6*time.Hour))

		stats := runDetectorOnce(ctx, t, db, ps, now)
		require.NoError(t, stats.Error)
		require.Empty(t, stats.TerminatedJobIDs)

		// Even when the job is a candidate, the recheck under the job row
		// lock keeps a capacity wait alive.
		stats = runDetectorOnce(ctx, t, candidateStore{Store: db, candidates: []uuid.UUID{managed.job.ID}}, ps, now)
		require.NoError(t, stats.Error)
		require.Empty(t, stats.TerminatedJobIDs)

		job, err := db.GetProvisionerJobByID(ctx, managed.job.ID)
		require.NoError(t, err)
		require.Equal(t, database.ProvisionerJobStatusPending, job.JobStatus)
		require.True(t, job.UpdatedAt.Equal(managed.job.UpdatedAt), "the reaper must not bump timestamps")
	})

	t.Run("GrantedMeasuredFromGrant", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		db, ps := dbtestutil.NewDB(t)
		now := time.Now()
		managed := newManagedStart(ctx, t, db, now.Add(-6*time.Hour))
		grantAdmission(ctx, t, db, managed.job.ID, now.Add(-time.Minute))

		stats := runDetectorOnce(ctx, t, db, ps, now)
		require.NoError(t, stats.Error)
		require.Empty(t, stats.TerminatedJobIDs)

		stats = runDetectorOnce(ctx, t, candidateStore{Store: db, candidates: []uuid.UUID{managed.job.ID}}, ps, now)
		require.NoError(t, stats.Error)
		require.Empty(t, stats.TerminatedJobIDs)

		admission, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, managed.job.ID)
		require.NoError(t, err)
		require.Equal(t, database.WorkspaceAdmissionStatusGranted, admission.Status)
	})

	t.Run("StaleGrantReapedAndRevoked", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		db, ps := dbtestutil.NewDB(t)
		now := time.Now()
		managed := newManagedStart(ctx, t, db, now.Add(-6*time.Hour))
		grantAdmission(ctx, t, db, managed.job.ID, now.Add(-time.Hour))

		stats := runDetectorOnce(ctx, t, db, ps, now)
		require.NoError(t, stats.Error)
		require.Equal(t, []uuid.UUID{managed.job.ID}, stats.TerminatedJobIDs)
		requireTerminatedJob(ctx, t, db, managed.job.ID, now, jobreaper.Pending)

		admission, err := db.GetWorkspaceBuildAdmissionByJobID(ctx, managed.job.ID)
		require.NoError(t, err)
		require.Equal(t, database.WorkspaceAdmissionStatusRevoked, admission.Status)
		require.Equal(t, workspaceadmission.ReasonProvisionerTimeout, admission.Reason)
	})

	t.Run("UnmanagedPendingStillReaped", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		db, ps := dbtestutil.NewDB(t)
		now := time.Now()
		org := dbgen.Organization(t, db, database.Organization{})
		user := dbgen.User(t, db, database.User{})
		job := dbgen.ProvisionerJob(t, db, nil, database.ProvisionerJob{
			CreatedAt:      now.Add(-time.Hour),
			UpdatedAt:      now.Add(-time.Hour),
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
		})
		_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{OrganizationID: org.ID, CreatedBy: user.ID, JobID: job.ID})

		stats := runDetectorOnce(ctx, t, db, ps, now)
		require.NoError(t, stats.Error)
		require.Equal(t, []uuid.UUID{job.ID}, stats.TerminatedJobIDs)
		requireTerminatedJob(ctx, t, db, job.ID, now, jobreaper.Pending)
	})
}
