// Package admissionapi serves the restricted workspace admission API used by
// the external capacity controller. It runs on its own listener, separate
// from the employee-facing Coder API, and authenticates a single service
// bearer token whose SHA-256 hash is read from a file.
package admissionapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/pubsub"
	"github.com/coder/coder/v2/coderd/files"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/workspaceadmission"
	"github.com/coder/coder/v2/coderd/wsbuilder"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/quartz"
)

const (
	// MaxWatchTimeout bounds the long poll of GET /v1/watch.
	MaxWatchTimeout = 30 * time.Second
	// maxBodyBytes bounds request bodies.
	maxBodyBytes = 1 << 20
	// tokenHashPrefix prefixes the hex SHA-256 digest in the token hash file.
	tokenHashPrefix = "sha256:"
	// DefaultAgentInactiveDisconnectTimeout matches the coderd default.
	DefaultAgentInactiveDisconnectTimeout = 30 * time.Second
)

// Options configures the admission API server.
type Options struct {
	Logger slog.Logger
	// Database must be the dbauthz-wrapped store of the Coder API.
	Database database.Store
	Pubsub   pubsub.Pubsub
	// FileCache and BuildUsageChecker are used to create STOP builds.
	FileCache         *files.Cache
	BuildUsageChecker *atomic.Pointer[wsbuilder.UsageChecker]
	Experiments       codersdk.Experiments
	DeploymentValues  *codersdk.DeploymentValues
	BuildMetrics      *wsbuilder.Metrics
	// TokenHashFile contains "sha256:<hex>" of the service bearer token.
	TokenHashFile string
	Clock         quartz.Clock
	// AgentInactiveDisconnectTimeout decides when an agent that stopped
	// reporting its connection counts as disconnected, as in the Coder
	// API. Zero uses DefaultAgentInactiveDisconnectTimeout.
	AgentInactiveDisconnectTimeout time.Duration
	// CreateWorkspace creates workspaces on behalf of their owners for
	// POST /v1/workspaces, normally coderd's
	// API.CreateWorkspaceForAdmission. Without it the route answers 501.
	CreateWorkspace workspaceadmission.CreateWorkspaceFunc
}

// Server serves the admission API.
type Server struct {
	logger            slog.Logger
	db                database.Store
	pubsub            pubsub.Pubsub
	fileCache         *files.Cache
	buildUsageChecker *atomic.Pointer[wsbuilder.UsageChecker]
	experiments       codersdk.Experiments
	deploymentValues  *codersdk.DeploymentValues
	buildMetrics      *wsbuilder.Metrics
	tokenHashFile     string
	clock             quartz.Clock
	// agentInactiveTimeout decides when an agent that stopped reporting
	// counts as disconnected.
	agentInactiveTimeout time.Duration
	createWorkspaceFn    workspaceadmission.CreateWorkspaceFunc
}

// New validates the options and returns a server. It fails when the token
// hash file is missing or invalid, so a misconfigured listener never starts.
func New(opts Options) (*Server, error) {
	if opts.Database == nil {
		return nil, xerrors.New("database is required")
	}
	if opts.Pubsub == nil {
		return nil, xerrors.New("pubsub is required")
	}
	if opts.BuildUsageChecker == nil {
		return nil, xerrors.New("build usage checker is required")
	}
	if _, err := LoadTokenHash(opts.TokenHashFile); err != nil {
		return nil, xerrors.Errorf("load workspace admission token hash: %w", err)
	}
	clock := opts.Clock
	if clock == nil {
		clock = quartz.NewReal()
	}
	agentInactiveTimeout := opts.AgentInactiveDisconnectTimeout
	if agentInactiveTimeout <= 0 {
		agentInactiveTimeout = DefaultAgentInactiveDisconnectTimeout
	}
	return &Server{
		logger:               opts.Logger,
		db:                   opts.Database,
		pubsub:               opts.Pubsub,
		fileCache:            opts.FileCache,
		buildUsageChecker:    opts.BuildUsageChecker,
		experiments:          opts.Experiments,
		deploymentValues:     opts.DeploymentValues,
		buildMetrics:         opts.BuildMetrics,
		tokenHashFile:        opts.TokenHashFile,
		clock:                clock,
		agentInactiveTimeout: agentInactiveTimeout,
		createWorkspaceFn:    opts.CreateWorkspace,
	}, nil
}

// Handler returns the HTTP handler of the admission API. Every route requires
// the service bearer token.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.authenticate)
	r.Get("/healthz", s.healthz)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/watch", s.watch)
		r.Route("/requests", func(r chi.Router) {
			r.Get("/", s.listRequests)
			r.Post("/display", s.updateDisplays)
			r.Route("/{job_id}", func(r chi.Router) {
				r.Get("/", s.showRequest)
				r.Post("/grant", s.grantRequest)
				r.Post("/revoke", s.revokeRequest)
				r.Post("/reject", s.rejectRequest)
			})
		})
		r.Route("/templates", func(r chi.Router) {
			r.Get("/", s.listTemplates)
			r.Get("/{template_id}", s.showTemplate)
			r.Put("/{template_id}", s.putTemplate)
			r.Delete("/{template_id}", s.deleteTemplate)
		})
		r.Route("/entitlements/{user_id}", func(r chi.Router) {
			r.Get("/", s.showEntitlements)
			r.Put("/", s.putEntitlements)
		})
		r.Route("/users", func(r chi.Router) {
			r.Get("/", s.listUsers)
			r.Route("/{user_id}", func(r chi.Router) {
				r.Get("/", s.showUser)
				r.Post("/suspend", s.suspendUser)
				r.Post("/activate", s.activateUser)
			})
		})
		r.Route("/workspaces", func(r chi.Router) {
			r.Get("/", s.listWorkspaces)
			r.Post("/", s.createWorkspace)
			r.Route("/{workspace_id}", func(r chi.Router) {
				r.Get("/", s.showWorkspace)
				r.Post("/stop", s.stopWorkspace)
				r.Post("/delete", s.deleteWorkspace)
			})
		})
		r.Route("/grants/{grant_id}", func(r chi.Router) {
			r.Put("/", s.putGrant)
			r.Post("/revoke", s.revokeGrant)
		})
		r.Post("/jobs/{job_id}/cancel", s.cancelJob)
		r.Route("/wake-intents", func(r chi.Router) {
			r.Get("/", s.listWakeIntents)
			r.Post("/{workspace_id}/ack", s.ackWakeIntent)
		})
		r.Route("/keepalive-intents", func(r chi.Router) {
			r.Get("/", s.listKeepaliveIntents)
			r.Post("/{workspace_id}/ack", s.ackKeepaliveIntent)
		})
	})
	r.NotFound(func(rw http.ResponseWriter, r *http.Request) {
		writeError(r.Context(), rw, http.StatusNotFound, CodeNotFound, "Route not found.", "")
	})
	r.MethodNotAllowed(func(rw http.ResponseWriter, r *http.Request) {
		writeError(r.Context(), rw, http.StatusMethodNotAllowed, CodeInvalidRequest, "Method not allowed.", "")
	})
	return r
}

// LoadTokenHash reads the SHA-256 digest of the service token from a file
// containing "sha256:<hex>".
func LoadTokenHash(path string) ([]byte, error) {
	if path == "" {
		return nil, xerrors.New("token hash file is not configured")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // The path comes from server configuration.
	if err != nil {
		return nil, xerrors.Errorf("read token hash file: %w", err)
	}
	digest, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), tokenHashPrefix)
	if !ok {
		return nil, xerrors.Errorf("token hash file must contain %q followed by a hex SHA-256 digest", tokenHashPrefix)
	}
	sum, err := hex.DecodeString(digest)
	if err != nil {
		return nil, xerrors.Errorf("decode token hash: %w", err)
	}
	if len(sum) != sha256.Size {
		return nil, xerrors.Errorf("token hash must be %d bytes, got %d", sha256.Size, len(sum))
	}
	return sum, nil
}

// authenticate checks the bearer token against the hash file on every
// request, so a rotated file applies without a restart. A missing or invalid
// file rejects every request.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		expected, err := LoadTokenHash(s.tokenHashFile)
		if err != nil {
			s.logger.Error(ctx, "workspace admission token hash unavailable", slog.Error(err))
			writeError(ctx, rw, http.StatusServiceUnavailable, CodeTokenUnavailable, "The admission service token is not configured.", "")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || token == "" {
			writeError(ctx, rw, http.StatusUnauthorized, CodeUnauthorized, "A bearer token is required.", "")
			return
		}
		sum := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(sum[:], expected) != 1 {
			writeError(ctx, rw, http.StatusUnauthorized, CodeUnauthorized, "The bearer token is invalid.", "")
			return
		}
		//nolint:gocritic // The caller proved it holds the admission service token; this subject only carries the admission service permissions.
		next.ServeHTTP(rw, r.WithContext(dbauthz.AsWorkspaceAdmission(ctx)))
	})
}

func (*Server) healthz(rw http.ResponseWriter, r *http.Request) {
	httpapi.Write(r.Context(), rw, http.StatusOK, map[string]string{"status": "ok"})
}

// apiError is returned from transaction closures to abort with a specific
// response.
type apiError struct {
	status int
	resp   ErrorResponse
}

func (e *apiError) Error() string {
	return e.resp.Code + ": " + e.resp.Message
}

func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, resp: ErrorResponse{Code: code, Message: message}}
}

func writeError(ctx context.Context, rw http.ResponseWriter, status int, code, message, detail string) {
	httpapi.Write(ctx, rw, status, ErrorResponse{Code: code, Message: message, Detail: detail})
}

// writeErr writes an apiError as is, and any other error as an internal
// error.
func (s *Server) writeErr(ctx context.Context, rw http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		httpapi.Write(ctx, rw, ae.status, ae.resp)
		return
	}
	s.logger.Error(ctx, "workspace admission request failed", slog.Error(err))
	writeError(ctx, rw, http.StatusInternalServerError, CodeInternal, "Internal error.", err.Error())
}

// decodeBody strictly decodes a JSON request body. Unknown fields are
// rejected so typos in controller requests fail loudly.
func decodeBody(rw http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(rw, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return newAPIError(http.StatusBadRequest, CodeInvalidRequest, "Invalid request body: "+err.Error())
	}
	if dec.More() {
		return newAPIError(http.StatusBadRequest, CodeInvalidRequest, "Invalid request body: unexpected trailing data.")
	}
	return nil
}

func uuidParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, newAPIError(http.StatusBadRequest, CodeInvalidRequest, "Invalid "+name+".")
	}
	return id, nil
}

// publish notifies admission watchers after a committed change. Failures are
// only logged because watchers also poll.
func (s *Server) publish(ctx context.Context, jobID uuid.UUID) {
	if err := publishAdmission(s.pubsub, jobID); err != nil {
		s.logger.Warn(ctx, "failed to publish workspace admission event", slog.F("job_id", jobID), slog.Error(err))
	}
}
