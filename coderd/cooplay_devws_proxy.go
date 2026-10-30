package coderd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/codersdk"
)

// CooplayDevwsRoutePrefix is the coderd path under which the Cooplay devws
// controller user API is served: /api/v2/cooplay/devws/v1/me/... is forwarded
// to <controller URL>/v1/me/....
const CooplayDevwsRoutePrefix = "/api/v2/cooplay/devws"

// cooplayDevwsUserRoutes are the controller user API routes that the proxy
// forwards, as http.ServeMux patterns. Every other path and method is answered
// with 404 by coderd and never reaches the controller, in particular the
// provider, admin, host, self and loopback routes of the controller.
var cooplayDevwsUserRoutes = []string{
	"GET /v1/me",
	"GET /v1/me/ps",
	"GET /v1/me/workspaces",
	"POST /v1/me/workspaces",
	"GET /v1/me/workspaces/{workspace}",
	"DELETE /v1/me/workspaces/{workspace}",
	"POST /v1/me/workspaces/{workspace}/clone",
	"POST /v1/me/workspaces/{workspace}/wake",
	"POST /v1/me/workspaces/{workspace}/sleep",
	"POST /v1/me/workspaces/{workspace}/stop",
	"POST /v1/me/workspaces/{workspace}/keepalive",
	"DELETE /v1/me/workspaces/{workspace}/keepalive",
	"PATCH /v1/me/workspaces/{workspace}/power-policy",
	"POST /v1/me/workspaces/{workspace}/exec",
	"POST /v1/me/workspaces/{workspace}/exec/{request}/cancel",
	"GET /v1/me/workspaces/{workspace}/files",
	"PUT /v1/me/workspaces/{workspace}/files",
	"GET /v1/me/workspaces/{workspace}/snapshots",
	"POST /v1/me/workspaces/{workspace}/snapshots",
	"POST /v1/me/workspaces/{workspace}/snapshots/{snapshot}/restore",
	"DELETE /v1/me/workspaces/{workspace}/snapshots/{snapshot}",
	"GET /v1/me/power-transitions/{transition}",
	"GET /v1/me/operations/{operation}",
	"POST /v1/me/runs",
	"PATCH /v1/me/runs/{run}",
	"POST /v1/me/runs/{run}/end",
	"GET /v1/me/grants",
	"POST /v1/me/grants",
	"DELETE /v1/me/grants/{grant}",
}

// cooplayDevwsExecutorHeaders identify a workspace to the controller. Only
// the executor relay may set them, on host routes, so the proxy never
// forwards them.
var cooplayDevwsExecutorHeaders = []string{
	"Devws-Workspace-Id",
	"Devws-Generation",
	"Devws-Power-Epoch",
}

// ValidateCooplayDevwsControllerURL checks the base URL of the devws
// controller user API. An empty URL is valid and disables the route.
func ValidateCooplayDevwsControllerURL(u *url.URL) error {
	if u == nil || u.String() == "" {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return xerrors.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return xerrors.New("host is required")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return xerrors.New("must not contain user info, a query or a fragment")
	}
	return nil
}

// cooplayDevwsProxy forwards the allowlisted controller user API routes for
// signed-in users. The caller's Coder session token travels in the
// Coder-Session-Token header and the controller verifies it with coderd
// itself; cookies, Authorization headers and session tokens in the query are
// never forwarded, and the controller cannot set cookies on the Coder origin.
type cooplayDevwsProxy struct {
	logger slog.Logger
	routes *http.ServeMux
	proxy  *httputil.ReverseProxy
}

// cooplayDevwsAllowedRoute marks the allowlisted patterns in the routes mux.
// The mux only matches requests and never serves them.
type cooplayDevwsAllowedRoute struct{}

func (cooplayDevwsAllowedRoute) ServeHTTP(http.ResponseWriter, *http.Request) {}

// newCooplayDevwsProxy returns the handler of CooplayDevwsRoutePrefix. With
// an empty or invalid controller URL every request is answered with 404.
func newCooplayDevwsProxy(logger slog.Logger, controllerURL *url.URL) http.Handler {
	if controllerURL == nil || controllerURL.String() == "" {
		return http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			httpapi.RouteNotFound(rw)
		})
	}
	if err := ValidateCooplayDevwsControllerURL(controllerURL); err != nil {
		logger.Error(context.Background(), "devws controller route disabled: invalid controller URL", slog.Error(err))
		return http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			httpapi.RouteNotFound(rw)
		})
	}
	target := *controllerURL

	p := &cooplayDevwsProxy{
		logger: logger,
		routes: http.NewServeMux(),
	}
	for _, pattern := range cooplayDevwsUserRoutes {
		p.routes.Handle(pattern, cooplayDevwsAllowedRoute{})
	}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, CooplayDevwsRoutePrefix)
			pr.Out.URL.RawPath = ""
			query := pr.Out.URL.Query()
			if query.Has(codersdk.SessionTokenCookie) || query.Has("access_token") {
				query.Del(codersdk.SessionTokenCookie)
				query.Del("access_token")
				pr.Out.URL.RawQuery = query.Encode()
			}
			pr.SetURL(&target)
			// Client-sent X-Forwarded-* headers are already removed.
			pr.SetXForwarded()
			if pr.Out.Header.Get("X-Forwarded-For") == "" {
				// ExtractRealIP leaves the client IP in RemoteAddr
				// without a port, which SetXForwarded cannot parse.
				if ip, err := netip.ParseAddr(pr.In.RemoteAddr); err == nil {
					pr.Out.Header.Set("X-Forwarded-For", ip.String())
				}
			}
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			for _, header := range cooplayDevwsExecutorHeaders {
				pr.Out.Header.Del(header)
			}
		},
		// Exec output is NDJSON that must reach the client frame by frame.
		FlushInterval: -1,
		Transport: &http.Transport{
			// Never send session tokens through a proxy from the
			// environment; the controller is a local service.
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			// Compressed responses would be buffered by the decompressor.
			DisableCompression: true,
			// No ResponseHeaderTimeout: wake and long-poll routes answer
			// after up to 25 seconds, exec streams until the command ends.
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				// The client went away; nobody reads the answer.
				return
			}
			logger.Warn(r.Context(), "devws controller request failed", slog.Error(err))
			httpapi.Write(r.Context(), rw, http.StatusBadGateway, codersdk.Response{
				Message: "The devws controller is unreachable.",
			})
		},
	}
	return p
}

func (p *cooplayDevwsProxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !p.allowedRoute(r) {
		httpapi.RouteNotFound(rw)
		return
	}
	token := r.Header.Get(codersdk.SessionTokenHeader)
	if token == "" || httpmw.APITokenFromRequest(r) != token {
		httpapi.Write(ctx, rw, http.StatusUnauthorized, codersdk.Response{
			Message: "The devws controller route requires the " + codersdk.SessionTokenHeader + " header.",
			Detail:  "Cookies, query parameters and Authorization headers are not forwarded to the controller, and the header must carry the session coderd authenticated.",
		})
		return
	}
	// Exec streams and long polls have no fixed duration. The main
	// listener sets no write timeout today; clear one if that changes.
	err := http.NewResponseController(rw).SetWriteDeadline(time.Time{})
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		p.logger.Debug(ctx, "clear write deadline", slog.Error(err))
	}
	p.proxy.ServeHTTP(rw, r)
}

// allowedRoute reports whether the request names an allowlisted route
// exactly: a clean path without percent-encoding (no route needs it) that
// the routes mux would dispatch as is, not redirect.
func (p *cooplayDevwsProxy) allowedRoute(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, CooplayDevwsRoutePrefix)
	if !ok || strings.Contains(r.URL.EscapedPath(), "%") {
		return false
	}
	h, _ := p.routes.Handler(&http.Request{Method: r.Method, URL: &url.URL{Path: rest}})
	_, ok = h.(cooplayDevwsAllowedRoute)
	return ok
}
