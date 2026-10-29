//go:build !slim

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/serpent"
)

// validateWorkspaceAdmission fails startup early when the admission listener
// is enabled without a valid token hash file.
func validateWorkspaceAdmission(vals *codersdk.DeploymentValues) error {
	if vals.WorkspaceAdmission.ListenAddress.String() == "" {
		return nil
	}
	if _, err := admissionapi.LoadTokenHash(vals.WorkspaceAdmission.TokenHashFile.String()); err != nil {
		return xerrors.Errorf("invalid workspace admission token hash file: %w", err)
	}
	return nil
}

// serveWorkspaceAdmission starts the restricted workspace admission API on its
// own listener so it is never reachable through the employee-facing API. It
// returns a function that shuts the listener down. When no listen address is
// configured the API is disabled and the returned function does nothing.
func serveWorkspaceAdmission(ctx context.Context, logger slog.Logger, inv *serpent.Invocation, vals *codersdk.DeploymentValues, api *coderd.API) (func(), error) {
	addr := vals.WorkspaceAdmission.ListenAddress.String()
	if addr == "" {
		return func() {}, nil
	}
	server, err := admissionapi.New(admissionapi.Options{
		Logger:                         logger,
		Database:                       api.Database,
		Pubsub:                         api.Pubsub,
		FileCache:                      api.FileCache,
		BuildUsageChecker:              api.BuildUsageChecker,
		Experiments:                    api.Experiments,
		DeploymentValues:               vals,
		BuildMetrics:                   api.WorkspaceBuilderMetrics,
		TokenHashFile:                  vals.WorkspaceAdmission.TokenHashFile.String(),
		Clock:                          nil,
		AgentInactiveDisconnectTimeout: api.AgentInactiveDisconnectTimeout,
		CreateWorkspace:                api.CreateWorkspaceForAdmission,
	})
	if err != nil {
		return nil, xerrors.Errorf("create workspace admission api: %w", err)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, xerrors.Errorf("listen on workspace admission address %q: %w", addr, err)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Long polls on /v1/watch last up to admissionapi.MaxWatchTimeout.
		WriteTimeout: admissionapi.MaxWatchTimeout + 30*time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(ctx, "workspace admission listener stopped", slog.Error(err))
		}
	}()
	_, _ = fmt.Fprintf(inv.Stdout, "Started workspace admission listener at http://%s\n", listener.Addr())
	logger.Info(ctx, "workspace admission listener started", slog.F("address", listener.Addr()))

	return func() {
		if err := shutdownWithTimeout(httpServer.Shutdown, 5*time.Second); err != nil {
			_ = httpServer.Close()
		}
	}, nil
}
