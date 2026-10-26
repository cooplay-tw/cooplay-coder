package codersdk_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/codersdk"
)

// TestWorkspaceBuildAdmissionJSON pins the JSON shape of the build admission.
// External clients parse these exact fields, so a change here is a breaking
// API change.
func TestWorkspaceBuildAdmissionJSON(t *testing.T) {
	t.Parallel()

	queuedAt := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	grantedAt := queuedAt.Add(time.Minute)
	position := 3

	keys := func(t *testing.T, v any) (map[string]any, []string) {
		t.Helper()
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		var names []string
		for name := range m {
			names = append(names, name)
		}
		slices.Sort(names)
		return m, names
	}

	t.Run("Waiting", func(t *testing.T) {
		t.Parallel()
		m, names := keys(t, codersdk.WorkspaceBuildAdmission{
			Status:        codersdk.WorkspaceBuildAdmissionStatusWaiting,
			Reason:        "waiting_for_capacity",
			QueuePosition: &position,
			QueuedAt:      queuedAt,
		})
		require.Equal(t, []string{"queue_position", "queued_at", "reason", "status"}, names)
		require.Equal(t, "waiting", m["status"])
		require.Equal(t, "waiting_for_capacity", m["reason"])
		require.EqualValues(t, 3, m["queue_position"])
		require.Equal(t, "2026-10-26T09:00:00Z", m["queued_at"])
	})

	t.Run("Granted", func(t *testing.T) {
		t.Parallel()
		m, names := keys(t, codersdk.WorkspaceBuildAdmission{
			Status:    codersdk.WorkspaceBuildAdmissionStatusGranted,
			QueuedAt:  queuedAt,
			GrantedAt: &grantedAt,
		})
		require.Equal(t, []string{"granted_at", "queued_at", "reason", "status"}, names)
		require.Equal(t, "granted", m["status"])
		require.Empty(t, m["reason"])
		require.Equal(t, "2026-10-26T09:01:00Z", m["granted_at"])
	})

	t.Run("BuildWithoutAdmission", func(t *testing.T) {
		t.Parallel()
		m, _ := keys(t, codersdk.WorkspaceBuild{})
		require.NotContains(t, m, "admission")
	})

	t.Run("BuildWithAdmission", func(t *testing.T) {
		t.Parallel()
		m, _ := keys(t, codersdk.WorkspaceBuild{
			Admission: &codersdk.WorkspaceBuildAdmission{
				Status:   codersdk.WorkspaceBuildAdmissionStatusRejected,
				Reason:   "quota: profile not allowed",
				QueuedAt: queuedAt,
			},
		})
		admission, ok := m["admission"].(map[string]any)
		require.True(t, ok, "admission must be an object")
		require.Equal(t, "rejected", admission["status"])
		require.Equal(t, "quota: profile not allowed", admission["reason"])
	})
}
