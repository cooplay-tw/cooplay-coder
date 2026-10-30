package codersdk

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	// WorkspaceCooplayKeepaliveDefaultSeconds is the keepalive duration of
	// a request without one: two hours, the "Keep awake 2 h" action.
	WorkspaceCooplayKeepaliveDefaultSeconds = 7200
	// WorkspaceCooplayKeepaliveMaxSeconds bounds a keepalive request. The
	// platform controller applies its own maximum.
	WorkspaceCooplayKeepaliveMaxSeconds = 43200
)

// WorkspaceCooplayKeepaliveRequest asks the platform controller to keep a
// workspace of a managed template awake.
type WorkspaceCooplayKeepaliveRequest struct {
	// ForSeconds is the requested keepalive duration. Zero requests
	// WorkspaceCooplayKeepaliveDefaultSeconds.
	ForSeconds int `json:"for_seconds,omitempty"`
}

// WorkspaceCooplayKeepaliveResponse is the recorded keepalive request. The
// platform controller applies it asynchronously.
type WorkspaceCooplayKeepaliveResponse struct {
	RequestedAt time.Time `json:"requested_at" format:"date-time"`
	ForSeconds  int       `json:"for_seconds"`
}

// WorkspaceAgentWakeResult tells what an explicit wake request did.
type WorkspaceAgentWakeResult string

const (
	// WorkspaceAgentWakeRequested means the platform controller was asked
	// to wake the workspace of the agent.
	WorkspaceAgentWakeRequested WorkspaceAgentWakeResult = "requested"
	// WorkspaceAgentWakeAwake means the workspace runs and its agent is
	// connected, so there was nothing to wake.
	WorkspaceAgentWakeAwake WorkspaceAgentWakeResult = "awake"
	// WorkspaceAgentWakeNotManaged means the template of the workspace is
	// not managed by workspace admission, so the platform never puts it to
	// sleep.
	WorkspaceAgentWakeNotManaged WorkspaceAgentWakeResult = "not_managed"
	// WorkspaceAgentWakeNotStarted means the workspace is stopped or
	// deleted, its latest build did not start it, or the agent belongs to
	// an older build. Start the workspace instead.
	WorkspaceAgentWakeNotStarted WorkspaceAgentWakeResult = "not_started"
)

// WorkspaceAgentWakeResponse is the answer to an explicit wake request.
type WorkspaceAgentWakeResponse struct {
	Result WorkspaceAgentWakeResult `json:"result" enums:"requested,awake,not_managed,not_started"`
}

// WorkspaceAgentWake asks the platform controller to wake the workspace of
// an agent whose microVM may be asleep. Clients call it when they find the
// agent disconnected, before they wait for it to reconnect. It needs SSH
// access to the workspace and never starts a stopped workspace.
func (c *Client) WorkspaceAgentWake(ctx context.Context, agentID uuid.UUID) (WorkspaceAgentWakeResponse, error) {
	res, err := c.Request(ctx, http.MethodPost, fmt.Sprintf("/api/v2/workspaceagents/%s/wake", agentID), nil)
	if err != nil {
		return WorkspaceAgentWakeResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		return WorkspaceAgentWakeResponse{}, ReadBodyAsError(res)
	}
	var resp WorkspaceAgentWakeResponse
	return resp, ReadBodyAsJSON(res, &resp)
}

// WorkspaceCooplayKeepalive asks the platform controller to keep a workspace
// of a managed template awake.
func (c *Client) WorkspaceCooplayKeepalive(ctx context.Context, workspaceID uuid.UUID, req WorkspaceCooplayKeepaliveRequest) (WorkspaceCooplayKeepaliveResponse, error) {
	res, err := c.Request(ctx, http.MethodPost, fmt.Sprintf("/api/v2/workspaces/%s/cooplay/keepalive", workspaceID), req)
	if err != nil {
		return WorkspaceCooplayKeepaliveResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		return WorkspaceCooplayKeepaliveResponse{}, ReadBodyAsError(res)
	}
	var resp WorkspaceCooplayKeepaliveResponse
	return resp, ReadBodyAsJSON(res, &resp)
}

// WorkspaceCooplayPowerState is the power state of the microVM of a
// workspace of a managed template, as reported by the platform controller.
// It is only meaningful while the latest build started the workspace.
type WorkspaceCooplayPowerState string

const (
	// WorkspaceCooplayPowerRunning means the microVM runs.
	WorkspaceCooplayPowerRunning WorkspaceCooplayPowerState = "running"
	// WorkspaceCooplayPowerReduced means the microVM runs with a reduced
	// memory guarantee after it was idle.
	WorkspaceCooplayPowerReduced WorkspaceCooplayPowerState = "reduced"
	// WorkspaceCooplayPowerPaused means the microVM is paused in memory.
	WorkspaceCooplayPowerPaused WorkspaceCooplayPowerState = "paused"
	// WorkspaceCooplayPowerHibernated means the microVM memory was saved to
	// disk and the microVM stopped. Waking restores its processes.
	WorkspaceCooplayPowerHibernated WorkspaceCooplayPowerState = "hibernated"
	// WorkspaceCooplayPowerCold means the microVM stopped without saved
	// memory. Waking boots it again; its processes ended.
	WorkspaceCooplayPowerCold WorkspaceCooplayPowerState = "cold"
	// WorkspaceCooplayPowerWaking means a wake is in progress.
	WorkspaceCooplayPowerWaking WorkspaceCooplayPowerState = "waking"
	// WorkspaceCooplayPowerSleeping means a pause, hibernation or cold stop
	// is in progress.
	WorkspaceCooplayPowerSleeping WorkspaceCooplayPowerState = "sleeping"
	// WorkspaceCooplayPowerUnknown means the platform cannot tell the state
	// of the microVM right now.
	WorkspaceCooplayPowerUnknown WorkspaceCooplayPowerState = "unknown"
)

// WorkspaceCooplayPowerStates lists every power state.
var WorkspaceCooplayPowerStates = []WorkspaceCooplayPowerState{
	WorkspaceCooplayPowerRunning,
	WorkspaceCooplayPowerReduced,
	WorkspaceCooplayPowerPaused,
	WorkspaceCooplayPowerHibernated,
	WorkspaceCooplayPowerCold,
	WorkspaceCooplayPowerWaking,
	WorkspaceCooplayPowerSleeping,
	WorkspaceCooplayPowerUnknown,
}

// VMRunning reports whether the microVM runs, so its agents are expected to
// be connected.
func (s WorkspaceCooplayPowerState) VMRunning() bool {
	return s == WorkspaceCooplayPowerRunning || s == WorkspaceCooplayPowerReduced
}

// WorkspaceCooplayRunState is the state of an agent Run.
type WorkspaceCooplayRunState string

// Run states. A Run waits for its model or for a human, is idle between
// turns, or ended as done or failed.
const (
	WorkspaceCooplayRunRunning      WorkspaceCooplayRunState = "running"
	WorkspaceCooplayRunWaitingModel WorkspaceCooplayRunState = "waiting_model"
	WorkspaceCooplayRunWaitingHuman WorkspaceCooplayRunState = "waiting_human"
	WorkspaceCooplayRunIdle         WorkspaceCooplayRunState = "idle"
	WorkspaceCooplayRunDone         WorkspaceCooplayRunState = "done"
	WorkspaceCooplayRunFailed       WorkspaceCooplayRunState = "failed"
)

// WorkspaceCooplayRunStates lists every Run state.
var WorkspaceCooplayRunStates = []WorkspaceCooplayRunState{
	WorkspaceCooplayRunRunning,
	WorkspaceCooplayRunWaitingModel,
	WorkspaceCooplayRunWaitingHuman,
	WorkspaceCooplayRunIdle,
	WorkspaceCooplayRunDone,
	WorkspaceCooplayRunFailed,
}

// WorkspaceCooplayRunStarter tells who started a Run.
type WorkspaceCooplayRunStarter string

const (
	// WorkspaceCooplayRunStartedByEmployee is a Run the workspace owner
	// started.
	WorkspaceCooplayRunStartedByEmployee WorkspaceCooplayRunStarter = "employee"
	// WorkspaceCooplayRunStartedByGrant is a Run an agent started under a
	// capability grant of the owner.
	WorkspaceCooplayRunStartedByGrant WorkspaceCooplayRunStarter = "grant"
)

// WorkspaceCooplayRun is an agent Run in a workspace, as reported by the
// platform controller.
type WorkspaceCooplayRun struct {
	ID string `json:"id"`
	// Harness is the agent harness, for example claude-code.
	Harness string `json:"harness"`
	// Mode is where the harness runs: A inside the workspace, B outside.
	Mode          string                     `json:"mode"`
	State         WorkspaceCooplayRunState   `json:"state" enums:"running,waiting_model,waiting_human,idle,done,failed"`
	WaitingReason string                     `json:"waiting_reason"`
	StartedBy     WorkspaceCooplayRunStarter `json:"started_by" enums:"employee,grant"`
	// GrantID is the capability grant of a Run started under one.
	GrantID     string    `json:"grant_id"`
	LastEventAt time.Time `json:"last_event_at" format:"date-time"`
}

// WorkspaceCooplayJob is the latest job run through the platform in a
// workspace.
type WorkspaceCooplayJob struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	ArgvSummary   string     `json:"argv_summary"`
	ArtifactsPath string     `json:"artifacts_path"`
	RunID         string     `json:"run_id"`
	StartedAt     time.Time  `json:"started_at" format:"date-time"`
	EndedAt       *time.Time `json:"ended_at,omitempty" format:"date-time"`
	ExitCode      *int       `json:"exit_code,omitempty"`
}

// WorkspaceCooplayGit are git facts the workspace reported about its work
// tree. They are reported from inside the workspace and only displayed.
type WorkspaceCooplayGit struct {
	Branch           string    `json:"branch"`
	Base             string    `json:"base"`
	Dirty            bool      `json:"dirty"`
	UncommittedFiles int       `json:"uncommitted_files"`
	Ahead            int       `json:"ahead"`
	Behind           int       `json:"behind"`
	MergeConflict    *bool     `json:"merge_conflict,omitempty"`
	ReportedAt       time.Time `json:"reported_at" format:"date-time"`
}

// WorkspaceCooplayPR are pull request facts the workspace reported. They
// are reported from inside the workspace and only displayed.
type WorkspaceCooplayPR struct {
	URL        string    `json:"url"`
	State      string    `json:"state"`
	CI         string    `json:"ci"`
	Number     int       `json:"number"`
	ReportedAt time.Time `json:"reported_at" format:"date-time"`
}

// WorkspaceCooplayPower is the power state of the microVM of a workspace.
type WorkspaceCooplayPower struct {
	State WorkspaceCooplayPowerState `json:"state" enums:"running,reduced,paused,hibernated,cold,waking,sleeping,unknown"`
	// Since is when the microVM entered the state.
	Since time.Time `json:"since" format:"date-time"`
	// Reason is the platform reason of the state, for example idle.
	Reason string `json:"reason"`
}

// WorkspaceCooplaySourceKind tells who asked the platform controller to
// create a workspace.
type WorkspaceCooplaySourceKind string

const (
	// WorkspaceCooplaySourceEmployee is a workspace the owner created
	// through the platform.
	WorkspaceCooplaySourceEmployee WorkspaceCooplaySourceKind = "employee"
	// WorkspaceCooplaySourceAgentGrant is a workspace an agent created
	// under a capability grant of the owner.
	WorkspaceCooplaySourceAgentGrant WorkspaceCooplaySourceKind = "agent_grant"
)

// WorkspaceCooplaySource tells how the platform controller created a
// workspace on behalf of its owner. Empty strings mean not set.
type WorkspaceCooplaySource struct {
	Kind WorkspaceCooplaySourceKind `json:"kind" enums:"employee,agent_grant"`
	// GrantID is the capability grant the workspace was created under.
	GrantID string `json:"grant_id"`
	// RunID is the agent Run that created the workspace.
	RunID string `json:"run_id"`
	// Harness is the agent harness that created the workspace.
	Harness              string `json:"harness"`
	InitiatorWorkspaceID string `json:"initiator_workspace_id"`
	CloneOfWorkspaceID   string `json:"clone_of_workspace_id"`
	// GrantExpiresAt and GrantRevokedAt describe the grant, when Coder
	// knows it.
	GrantExpiresAt *time.Time `json:"grant_expires_at,omitempty" format:"date-time"`
	GrantRevokedAt *time.Time `json:"grant_revoked_at,omitempty" format:"date-time"`
}

// WorkspaceCooplay is what the platform controller reports about a
// workspace of a managed template: the power state of its microVM, agent
// Runs, facts about its work and how it was created.
type WorkspaceCooplay struct {
	// Power is omitted while the platform reports no power state, for
	// example while the workspace is stopped.
	Power *WorkspaceCooplayPower `json:"power,omitempty"`
	// SleepNoticeAt is the time of a pending sleep. Until then a keepalive
	// request keeps the workspace awake.
	SleepNoticeAt *time.Time `json:"sleep_notice_at,omitempty" format:"date-time"`
	// KeepaliveUntil is the end of an applied keepalive.
	KeepaliveUntil *time.Time            `json:"keepalive_until,omitempty" format:"date-time"`
	Runs           []WorkspaceCooplayRun `json:"runs"`
	LastJob        *WorkspaceCooplayJob  `json:"last_job,omitempty"`
	Git            *WorkspaceCooplayGit  `json:"git,omitempty"`
	PR             *WorkspaceCooplayPR   `json:"pr,omitempty"`
	// Source is set for workspaces the platform controller created.
	Source *WorkspaceCooplaySource `json:"source,omitempty"`
}
