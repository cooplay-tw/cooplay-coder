// Package workspaceadmission holds the shared rules of workspace admission.
//
// A template listed in workspace_admission_templates is managed. Every START
// build of a managed template is committed together with a waiting admission
// record. An external capacity controller later grants the record through the
// restricted admission service (see the admissionapi package), and only then
// may a provisioner acquire the job. Capacity waits never occupy provisioner
// workers and are never reported as HTTP 403.
package workspaceadmission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/codersdk"
)

const (
	// ReasonWaitingForCapacity is the waiting reason of a START build that
	// the controller has not granted yet.
	ReasonWaitingForCapacity = codersdk.WorkspaceBuildAdmissionReasonWaitingForCapacity
	// ReasonCanceled is recorded when a pending START is canceled before a
	// provisioner acquired it.
	ReasonCanceled = "canceled_before_start"
	// ReasonProvisionerTimeout is recorded when the job reaper fails a
	// granted START that no provisioner acquired in time.
	ReasonProvisionerTimeout = "provisioner_acquire_timeout"
	// ReasonStoppedByController is recorded when the admission service
	// cancels a pending START to stop the workspace.
	ReasonStoppedByController = "stopped_by_controller"
)

// EventChannel is the pubsub channel notified after admission records change.
// Notifications only wake watchers early; consumers must not rely on them.
const EventChannel = "workspace_admission:changed"

// Kinds of intent events published on EventChannel.
const (
	// EventKindWakeIntent is published after a wake intent was recorded.
	EventKindWakeIntent = "wake_intent"
	// EventKindKeepaliveIntent is published after a keepalive intent was
	// recorded.
	EventKindKeepaliveIntent = "keepalive_intent"
	// EventKindWorkspaceState is published after the controller stored or
	// cleared the power state of a workspace.
	EventKindWorkspaceState = "workspace_state"
)

// Event is the payload published on EventChannel.
type Event struct {
	// JobID is the build job whose admission changed. It is zero for intent
	// and power state events.
	JobID uuid.UUID `json:"job_id"`
	// Kind is empty for admission changes and one of the EventKind values
	// for intents and power states.
	Kind string `json:"kind,omitempty"`
	// WorkspaceID is the workspace of an intent or power state event.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
}

// Publish notifies admission watchers that the admission of jobID changed.
// Call it only after the change is committed.
func Publish(ps pubsub.Pubsub, jobID uuid.UUID) error {
	msg, err := json.Marshal(Event{JobID: jobID})
	if err != nil {
		return xerrors.Errorf("marshal workspace admission event: %w", err)
	}
	if err := ps.Publish(EventChannel, msg); err != nil {
		return xerrors.Errorf("publish workspace admission event: %w", err)
	}
	return nil
}

// PublishIntent notifies admission watchers that an intent or the power
// state of the given kind changed for a workspace. Call it only after the
// change is committed.
func PublishIntent(ps pubsub.Pubsub, kind string, workspaceID uuid.UUID) error {
	msg, err := json.Marshal(Event{Kind: kind, WorkspaceID: &workspaceID})
	if err != nil {
		return xerrors.Errorf("marshal workspace admission intent event: %w", err)
	}
	if err := ps.Publish(EventChannel, msg); err != nil {
		return xerrors.Errorf("publish workspace admission intent event: %w", err)
	}
	return nil
}

// CancelPending ends the admission of a job that is being canceled before
// any provisioner acquired it. The caller must hold the provisioner job row
// lock and must have verified that the job has no worker. It reports whether
// an admission record changed; jobs without a pending admission (for example
// builds of templates that are not managed) are left alone.
func CancelPending(ctx context.Context, store database.Store, jobID uuid.UUID, now time.Time) (bool, error) {
	_, err := store.UpdatePendingWorkspaceBuildAdmissionStatus(ctx, database.UpdatePendingWorkspaceBuildAdmissionStatusParams{
		JobID:  jobID,
		Status: database.WorkspaceAdmissionStatusCanceled,
		Reason: ReasonCanceled,
		Now:    now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, xerrors.Errorf("cancel workspace build admission: %w", err)
	}
	return true, nil
}
