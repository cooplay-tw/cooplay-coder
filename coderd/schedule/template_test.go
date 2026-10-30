package schedule_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/schedule"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissiontest"
	"github.com/coder/coder/v2/testutil"
)

func TestAGPLTemplateScheduleStoreManagedTemplate(t *testing.T) {
	t.Parallel()

	db, _ := dbtestutil.NewDB(t)
	ctx := testutil.Context(t, testutil.WaitShort)
	org := dbgen.Organization(t, db, database.Organization{})
	user := dbgen.User(t, db, database.User{})
	template := dbgen.Template(t, db, database.Template{
		OrganizationID: org.ID,
		CreatedBy:      user.ID,
	})
	require.NoError(t, db.UpdateTemplateScheduleByID(ctx, database.UpdateTemplateScheduleByIDParams{
		ID:                       template.ID,
		UpdatedAt:                dbtime.Now(),
		AllowUserAutostart:       true,
		AllowUserAutostop:        true,
		DefaultTTL:               int64(time.Hour),
		ActivityBump:             int64(30 * time.Minute),
		AutostopRequirementWeeks: template.AutostopRequirementWeeks,
	}))
	store := schedule.NewAGPLTemplateScheduleStore()

	opts, err := store.Get(ctx, db, template.ID)
	require.NoError(t, err)
	require.True(t, opts.UserAutostopEnabled)
	require.Equal(t, time.Hour, opts.DefaultTTL)

	// A managed template never stops its workspaces on a schedule; the
	// rest of the schedule is unchanged.
	admissiontest.Manage(t, db, template.ID, admissiontest.ProfileParameter)
	opts, err = store.Get(ctx, db, template.ID)
	require.NoError(t, err)
	require.False(t, opts.UserAutostopEnabled)
	require.Zero(t, opts.DefaultTTL)
	require.True(t, opts.UserAutostartEnabled)
	require.Equal(t, 30*time.Minute, opts.ActivityBump)
}
