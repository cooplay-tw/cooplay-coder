// Package wakeintent records wake intents: a client tried to reach an agent
// of a managed workspace whose microVM the platform controller may have put
// to sleep while the Coder build stays running.
//
// Hooks on the client connection paths call Enqueue, which never blocks and
// never touches the database, so it is safe under the tailnet coordinator
// mutex. A single worker resolves the workspace of the agent, skips
// workspaces that need no wake, writes the intent and notifies the admission
// service watchers. The first request for an agent is written immediately;
// repeats within DebounceWindow are coalesced into one delayed write.
package wakeintent

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/quartz"
)

// Source identifies the client path of a wake request. The values are the
// ones the controller receives in wake intents.
type Source string

const (
	// SourceTailnet is a coordinated tunnel to the agent, used by CLI
	// commands, IDE extensions and port forwarding.
	SourceTailnet Source = "tailnet"
	// SourceCLI is an explicit wake request from a client.
	SourceCLI Source = "cli"
	// SourcePTY is a web terminal.
	SourcePTY Source = "pty"
	// SourceApp is a request through the workspace app proxy.
	SourceApp Source = "app"
)

// Results of a wake request, the result label of the
// coderd_workspace_wake_intents_total metric. Every enqueued request is
// counted once.
const (
	// ResultRecorded means the intent was written.
	ResultRecorded = "recorded"
	// ResultDebounced means the request repeated an earlier one within
	// DebounceWindow and was coalesced into a delayed write.
	ResultDebounced = "debounced"
	// ResultDropped means the queue was full.
	ResultDropped = "dropped"
	// ResultFailed means the lookup or write failed.
	ResultFailed = "failed"
	// ResultSkippedConnect means the request came from Coder Connect, which
	// opens tunnels to every agent and must never wake workspaces.
	ResultSkippedConnect = "skipped_connect"
	// ResultSkippedUnmanaged means the agent is unknown or its template is
	// not managed by workspace admission.
	ResultSkippedUnmanaged = "skipped_unmanaged"
	// ResultSkippedStopped means the workspace is deleted, its latest build
	// is not a succeeded START, or the agent belongs to an older build.
	ResultSkippedStopped = "skipped_stopped"
	// ResultSkippedRunning means the controller reports the workspace as
	// running (or reports nothing) and the agent is connected.
	ResultSkippedRunning = "skipped_running"
)

const (
	// DebounceWindow is the time after a request for an agent during which
	// further requests for the same agent are coalesced.
	DebounceWindow = time.Second
	// QueueSize bounds the requests waiting for the worker.
	QueueSize = 1024

	writeTimeout = 5 * time.Second
)

// Power states reported by the controller in which the microVM runs.
const (
	powerStateRunning = "running"
	powerStateReduced = "reduced"
)

// Options configures a Recorder.
type Options struct {
	Logger slog.Logger
	// Database must be the dbauthz-wrapped store. The recorder uses its own
	// narrow subject.
	Database database.Store
	Pubsub   pubsub.Pubsub
	// Registerer receives coderd_workspace_wake_intents_total. Nil skips
	// registration.
	Registerer prometheus.Registerer
	Clock      quartz.Clock
	// AgentInactiveDisconnectTimeout decides when an agent that stopped
	// updating its connection counts as disconnected, as in the agent API.
	AgentInactiveDisconnectTimeout time.Duration
}

type request struct {
	agentID uuid.UUID
	userID  uuid.UUID
	source  Source
	at      time.Time
}

// batch is a set of requests for one agent written together.
type batch struct {
	first time.Time
	last  request
	count int32
}

// window tracks the debounce window of one agent. pending holds requests
// that arrived inside the window.
type window struct {
	opened  time.Time
	pending *batch
}

// Recorder records wake intents. A nil Recorder ignores every call.
type Recorder struct {
	logger          slog.Logger
	db              database.Store
	pubsub          pubsub.Pubsub
	clock           quartz.Clock
	inactiveTimeout time.Duration
	requests        *prometheus.CounterVec

	queue     chan request
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	// windows is owned by the worker goroutine.
	windows map[uuid.UUID]*window
}

// New starts a recorder. Call Close to stop its worker.
func New(opts Options) *Recorder {
	clock := opts.Clock
	if clock == nil {
		clock = quartz.NewReal()
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "coderd",
		Name:      "workspace_wake_intents_total",
		Help:      "Wake requests for workspaces managed by workspace admission, by client path and result.",
	}, []string{"source", "result"})
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(requests); err != nil {
			var already prometheus.AlreadyRegisteredError
			if existing, ok := alreadyRegistered(err, &already); ok {
				requests = existing
			} else {
				opts.Logger.Warn(context.Background(), "register workspace wake intent metric", slog.Error(err))
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Recorder{
		logger:          opts.Logger,
		db:              opts.Database,
		pubsub:          opts.Pubsub,
		clock:           clock,
		inactiveTimeout: opts.AgentInactiveDisconnectTimeout,
		requests:        requests,
		queue:           make(chan request, QueueSize),
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		windows:         make(map[uuid.UUID]*window),
	}
	go r.run()
	return r
}

func alreadyRegistered(err error, target *prometheus.AlreadyRegisteredError) (*prometheus.CounterVec, bool) {
	if !errors.As(err, target) {
		return nil, false
	}
	existing, ok := target.ExistingCollector.(*prometheus.CounterVec)
	return existing, ok
}

// Enqueue queues a wake request for an agent on behalf of userID (zero when
// the requester is not a user). It never blocks: when the queue is full the
// request is dropped and counted. Call it only after the requester was
// authorized to connect to the agent.
func (r *Recorder) Enqueue(agentID, userID uuid.UUID, source Source) {
	if r == nil {
		return
	}
	select {
	case r.queue <- request{agentID: agentID, userID: userID, source: source, at: r.clock.Now()}:
	default:
		r.count(source, ResultDropped)
	}
}

// SkipConnect counts a request that was not enqueued because it came from
// Coder Connect.
func (r *Recorder) SkipConnect(source Source) {
	if r == nil {
		return
	}
	r.count(source, ResultSkippedConnect)
}

// Close stops the worker. Requests still queued are discarded; their
// clients retry.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.cancel()
		<-r.done
	})
}

func (r *Recorder) count(source Source, result string) {
	r.requests.WithLabelValues(string(source), result).Inc()
}

func (r *Recorder) run() {
	defer close(r.done)
	ticker := r.clock.NewTicker(DebounceWindow, "wakeintent", "flush")
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case req := <-r.queue:
			r.handle(req)
		case <-ticker.C:
			r.flush(r.clock.Now())
		}
	}
}

// handle writes the first request for an agent at once and coalesces
// repeats that arrive within DebounceWindow of it.
func (r *Recorder) handle(req request) {
	if w, ok := r.windows[req.agentID]; ok && req.at.Sub(w.opened) < DebounceWindow {
		if w.pending == nil {
			w.pending = &batch{first: req.at}
		}
		w.pending.last = req
		w.pending.count++
		r.count(req.source, ResultDebounced)
		return
	}
	r.windows[req.agentID] = &window{opened: req.at}
	r.count(req.source, r.record(batch{first: req.at, last: req, count: 1}))
}

// flush writes the coalesced requests of every window that ended, and
// forgets windows without pending requests. A flushed window opens a new
// one, so a steady stream of requests is written about once per window.
func (r *Recorder) flush(now time.Time) {
	for agentID, w := range r.windows {
		if now.Sub(w.opened) < DebounceWindow {
			continue
		}
		if w.pending == nil {
			delete(r.windows, agentID)
			continue
		}
		b := *w.pending
		r.windows[agentID] = &window{opened: now}
		// The requests were counted as debounced when they arrived.
		_ = r.record(b)
	}
}

// record decides whether the requests of b need a wake and writes the
// intent. It returns the metric result.
func (r *Recorder) record(b batch) string {
	//nolint:gocritic // The hooks authorized the requester's access to the agent; this subject only reads wake state and writes intents.
	ctx, cancel := context.WithTimeout(dbauthz.AsWorkspaceWakeRecorder(r.ctx), writeTimeout)
	defer cancel()
	logger := r.logger.With(slog.F("agent_id", b.last.agentID), slog.F("source", b.last.source))

	target, err := r.db.GetWorkspaceAdmissionWakeTargetByAgentID(ctx, b.last.agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ResultSkippedUnmanaged
	}
	if err != nil {
		if r.ctx.Err() == nil {
			logger.Warn(ctx, "look up workspace wake target", slog.Error(err))
		}
		return ResultFailed
	}
	if result, skip := r.skip(target); skip {
		return result
	}

	_, err = r.db.UpsertWorkspaceAdmissionWakeIntent(ctx, database.UpsertWorkspaceAdmissionWakeIntentParams{
		WorkspaceID:      target.WorkspaceID,
		OrganizationID:   target.OrganizationID,
		AgentID:          b.last.agentID,
		Source:           string(b.last.source),
		RequestedBy:      uuid.NullUUID{UUID: b.last.userID, Valid: b.last.userID != uuid.Nil},
		FirstRequestedAt: dbtime.Time(b.first),
		LastRequestedAt:  dbtime.Time(b.last.at),
		RequestCount:     b.count,
	})
	if err != nil {
		if r.ctx.Err() == nil {
			logger.Warn(ctx, "record workspace wake intent", slog.F("workspace_id", target.WorkspaceID), slog.Error(err))
		}
		return ResultFailed
	}
	if err := workspaceadmission.PublishIntent(r.pubsub, workspaceadmission.EventKindWakeIntent, target.WorkspaceID); err != nil {
		logger.Warn(ctx, "publish workspace wake intent", slog.F("workspace_id", target.WorkspaceID), slog.Error(err))
	}
	logger.Debug(ctx, "recorded workspace wake intent", slog.F("workspace_id", target.WorkspaceID), slog.F("requests", b.count))
	return ResultRecorded
}

// skip reports whether a connection to the target agent needs no wake.
func (r *Recorder) skip(target database.GetWorkspaceAdmissionWakeTargetByAgentIDRow) (string, bool) {
	switch {
	case !target.Managed:
		return ResultSkippedUnmanaged, true
	case target.WorkspaceDeleted,
		!target.AgentInLatestBuild,
		target.LatestBuildTransition != database.WorkspaceTransitionStart,
		target.LatestJobStatus != database.ProvisionerJobStatusSucceeded:
		return ResultSkippedStopped, true
	}
	// Without a reported power state the agent connection decides. A
	// reported sleep, a wake in progress or an unknown state always records,
	// because coderd may still show the agent as connected for a while
	// after its microVM was suspended.
	awake := !target.PowerState.Valid ||
		target.PowerState.String == powerStateRunning ||
		target.PowerState.String == powerStateReduced
	if awake && target.WorkspaceAgent.Status(r.clock.Now(), r.inactiveTimeout).Status == database.WorkspaceAgentStatusConnected {
		return ResultSkippedRunning, true
	}
	return "", false
}
