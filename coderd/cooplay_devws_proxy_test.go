package coderd_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

// devwsEcho is what the fake controller answers for a forwarded request.
type devwsEcho struct {
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	RawQuery   string      `json:"raw_query"`
	Header     http.Header `json:"header"`
	BodySHA256 string      `json:"body_sha256"`
	BodyBytes  int         `json:"body_bytes"`
}

// fakeDevwsController records every request it receives and echoes it back,
// unless a handler is registered for a workspace path segment.
type fakeDevwsController struct {
	*httptest.Server

	mu       sync.Mutex
	seen     map[string]bool
	handlers map[string]http.HandlerFunc
}

func newFakeDevwsController(t *testing.T) *fakeDevwsController {
	t.Helper()
	c := &fakeDevwsController{
		seen:     map[string]bool{},
		handlers: map[string]http.HandlerFunc{},
	}
	c.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen[r.Method+" "+r.URL.EscapedPath()] = true
		var handler http.HandlerFunc
		for id, h := range c.handlers {
			if strings.Contains(r.URL.Path, "/"+id+"/") || strings.HasSuffix(r.URL.Path, "/"+id) {
				handler = h
			}
		}
		c.mu.Unlock()
		if handler != nil {
			handler(rw, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("X-Devws-Test", "echo")
		http.SetCookie(rw, &http.Cookie{Name: "controller", Value: "1"})
		_ = json.NewEncoder(rw).Encode(devwsEcho{
			Method:     r.Method,
			Path:       r.URL.EscapedPath(),
			RawQuery:   r.URL.RawQuery,
			Header:     r.Header,
			BodySHA256: hex.EncodeToString(sum[:]),
			BodyBytes:  len(body),
		})
	}))
	t.Cleanup(c.Close)
	return c
}

// handle registers h for requests whose path has the segment id.
func (c *fakeDevwsController) handle(id string, h http.HandlerFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[id] = h
}

func (c *fakeDevwsController) saw(method, path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[method+" "+path]
}

// devwsRequest sends a request through coderd's devws route. rawPath is
// appended to the route prefix unchanged.
func devwsRequest(ctx context.Context, t *testing.T, client *codersdk.Client, method, rawPath string, body []byte, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, client.URL.String()+coderd.CooplayDevwsRoutePrefix+rawPath, bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := client.HTTPClient.Do(req)
	require.NoError(t, err)
	return res
}

func sessionHeader(client *codersdk.Client) http.Header {
	return http.Header{codersdk.SessionTokenHeader: {client.SessionToken()}}
}

func newDevwsCoderd(t *testing.T, controllerURL string) (*codersdk.Client, codersdk.CreateFirstUserResponse) {
	t.Helper()
	dv := coderdtest.DeploymentValues(t)
	if controllerURL != "" {
		require.NoError(t, dv.WorkspaceAdmission.ControllerURL.Set(controllerURL))
	}
	client := coderdtest.New(t, &coderdtest.Options{DeploymentValues: dv})
	return client, coderdtest.CreateFirstUser(t, client)
}

func TestCooplayDevwsProxy(t *testing.T) {
	t.Parallel()

	controller := newFakeDevwsController(t)
	client, firstUser := newDevwsCoderd(t, controller.URL)

	t.Run("AllowedRoutes", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		ws := uuid.NewString()
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/v1/me"},
			{http.MethodGet, "/v1/me/ps"},
			{http.MethodGet, "/v1/me/workspaces"},
			{http.MethodPost, "/v1/me/workspaces"},
			{http.MethodGet, "/v1/me/workspaces/" + ws},
			{http.MethodDelete, "/v1/me/workspaces/" + ws},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/clone"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/wake"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/sleep"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/stop"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/keepalive"},
			{http.MethodDelete, "/v1/me/workspaces/" + ws + "/keepalive"},
			{http.MethodPatch, "/v1/me/workspaces/" + ws + "/power-policy"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/exec"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/exec/" + uuid.NewString() + "/cancel"},
			{http.MethodGet, "/v1/me/workspaces/" + ws + "/files"},
			{http.MethodPut, "/v1/me/workspaces/" + ws + "/files"},
			{http.MethodGet, "/v1/me/workspaces/" + ws + "/snapshots"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/snapshots"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/snapshots/ck-01jbz2/restore"},
			{http.MethodDelete, "/v1/me/workspaces/" + ws + "/snapshots/ck-01jbz2"},
			{http.MethodGet, "/v1/me/power-transitions/" + uuid.NewString()},
			{http.MethodGet, "/v1/me/operations/" + uuid.NewString()},
			{http.MethodPost, "/v1/me/runs"},
			{http.MethodPatch, "/v1/me/runs/r-01jbz2"},
			{http.MethodPost, "/v1/me/runs/r-01jbz2/end"},
			{http.MethodGet, "/v1/me/grants"},
			{http.MethodPost, "/v1/me/grants"},
			{http.MethodDelete, "/v1/me/grants/g-01jbz2"},
		} {
			body := []byte(`{"route":"` + route.method + " " + route.path + `"}`)
			res := devwsRequest(ctx, t, client, route.method, route.path+"?wait_seconds=5", body, sessionHeader(client))
			require.Equal(t, http.StatusOK, res.StatusCode, "%s %s", route.method, route.path)
			var echo devwsEcho
			err := json.NewDecoder(res.Body).Decode(&echo)
			_ = res.Body.Close()
			require.NoError(t, err, "%s %s", route.method, route.path)
			sum := sha256.Sum256(body)
			require.Equal(t, devwsEcho{
				Method:     route.method,
				Path:       route.path,
				RawQuery:   "wait_seconds=5",
				Header:     echo.Header,
				BodySHA256: hex.EncodeToString(sum[:]),
				BodyBytes:  len(body),
			}, echo)
			require.Equal(t, client.SessionToken(), echo.Header.Get(codersdk.SessionTokenHeader))
		}
	})

	t.Run("LargeBody", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		body := bytes.Repeat([]byte("devws"), 1<<20/5)
		res := devwsRequest(ctx, t, client, http.MethodPut, "/v1/me/workspaces/"+uuid.NewString()+"/files?path=%2Fhome%2Fdev%2Fa.txt", body, sessionHeader(client))
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		var echo devwsEcho
		require.NoError(t, json.NewDecoder(res.Body).Decode(&echo))
		sum := sha256.Sum256(body)
		require.Equal(t, hex.EncodeToString(sum[:]), echo.BodySHA256)
		require.Equal(t, "path=%2Fhome%2Fdev%2Fa.txt", echo.RawQuery)
	})

	t.Run("Headers", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		token := client.SessionToken()
		operationID := uuid.NewString()
		header := http.Header{
			codersdk.SessionTokenHeader: {token},
			"Devws-Grant":               {"g-01jbz2"},
			"Devws-Operation-Id":        {operationID},
			"Devws-Protocol":            {"2"},
			"User-Agent":                {"devws/1.0.0 (mcp)"},
			"Authorization":             {"Bearer not-for-the-controller"},
			"Cookie":                    {codersdk.SessionTokenCookie + "=" + token + "; other=1"},
			"Devws-Workspace-Id":        {uuid.NewString()},
			"Devws-Generation":          {"3"},
			"Devws-Power-Epoch":         {"4"},
			"X-Forwarded-For":           {"203.0.113.9"},
		}
		query := url.Values{
			codersdk.SessionTokenCookie: {token},
			"access_token":              {"not-for-the-controller"},
			"wait_seconds":              {"5"},
		}
		res := devwsRequest(ctx, t, client, http.MethodPost, "/v1/me/workspaces/"+uuid.NewString()+"/wake?"+query.Encode(), []byte(`{}`), header)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)

		var echo devwsEcho
		require.NoError(t, json.NewDecoder(res.Body).Decode(&echo))
		require.Equal(t, "wait_seconds=5", echo.RawQuery)
		require.Equal(t, token, echo.Header.Get(codersdk.SessionTokenHeader))
		require.Equal(t, "g-01jbz2", echo.Header.Get("Devws-Grant"))
		require.Equal(t, operationID, echo.Header.Get("Devws-Operation-Id"))
		require.Equal(t, "2", echo.Header.Get("Devws-Protocol"))
		require.Equal(t, "devws/1.0.0 (mcp)", echo.Header.Get("User-Agent"))
		require.Equal(t, "127.0.0.1", echo.Header.Get("X-Forwarded-For"))
		for _, stripped := range []string{"Authorization", "Cookie", "Devws-Workspace-Id", "Devws-Generation", "Devws-Power-Epoch"} {
			require.Empty(t, echo.Header.Values(stripped), stripped)
		}

		// The controller cannot set cookies on the Coder origin.
		require.Equal(t, "echo", res.Header.Get("X-Devws-Test"))
		for _, cookie := range res.Cookies() {
			require.NotEqual(t, "controller", cookie.Name)
		}
	})

	t.Run("Denied", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		ws := uuid.NewString()
		for _, route := range []struct{ method, path string }{
			// Other caller classes of the controller.
			{http.MethodGet, "/v1/admin/workspaces"},
			{http.MethodPost, "/v1/admin/owners/" + ws + "/suspend"},
			{http.MethodPut, "/v1/provider/workspaces/" + ws},
			{http.MethodGet, "/v1/host/workspaces/" + ws + "/self"},
			{http.MethodPost, "/v1/host/workspaces/" + ws + "/agent/exec"},
			{http.MethodGet, "/v1/self"},
			{http.MethodGet, "/healthz"},
			{http.MethodGet, "/metrics"},
			{http.MethodGet, ""},
			{http.MethodGet, "/"},
			// User routes with the wrong method or shape.
			{http.MethodPost, "/v1/me"},
			{http.MethodGet, "/v1/me/"},
			{http.MethodGet, "/v1/me/workspaces/" + ws + "/exec"},
			{http.MethodPut, "/v1/me/workspaces/" + ws},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "/unknown"},
			{http.MethodGet, "/v1/me/runs"},
			{http.MethodGet, "/v1/me/workspaces/" + ws + "/exec/x/cancel"},
			// Path tricks.
			{http.MethodGet, "/v1/me/../admin/workspaces"},
			{http.MethodGet, "/v1/me/./ps"},
			{http.MethodGet, "/v1/me//ps"},
			{http.MethodPost, "/v1/me/workspaces/" + ws + "%2F..%2F..%2Fadmin%2Freconcile/exec"},
			{http.MethodPost, "/v1/me/workspaces/%2e%2e/exec"},
			{http.MethodGet, "/v1/me/workspaces/a%20b"},
		} {
			res := devwsRequest(ctx, t, client, route.method, route.path, nil, sessionHeader(client))
			_ = res.Body.Close()
			require.Equal(t, http.StatusNotFound, res.StatusCode, "%s %q", route.method, route.path)
		}
		for _, path := range []string{"/v1/admin/workspaces", "/v1/host/workspaces/" + ws + "/self", "/v1/self", "/healthz", "/metrics"} {
			require.False(t, controller.saw(http.MethodGet, path), path)
		}
		require.False(t, controller.saw(http.MethodGet, "/v1/me/workspaces/"+ws+"/exec"))
		require.False(t, controller.saw(http.MethodPost, "/v1/me"))
	})

	t.Run("Unauthenticated", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		other, _ := coderdtest.CreateAnotherUser(t, client, firstUser.OrganizationID)
		ws := uuid.NewString()
		path := "/v1/me/workspaces/" + ws
		for name, header := range map[string]http.Header{
			"NoToken":      {},
			"InvalidToken": {codersdk.SessionTokenHeader: {"aaaaaaaaaa-bbbbbbbbbbbbbbbbbbbbbbbb"}},
			"CookieOnly":   {"Cookie": {codersdk.SessionTokenCookie + "=" + client.SessionToken()}},
			"BearerOnly":   {"Authorization": {"Bearer " + client.SessionToken()}},
			"CookieAndOtherHeader": {
				"Cookie":                    {codersdk.SessionTokenCookie + "=" + client.SessionToken()},
				codersdk.SessionTokenHeader: {other.SessionToken()},
			},
		} {
			res := devwsRequest(ctx, t, client, http.MethodGet, path, nil, header)
			_ = res.Body.Close()
			require.Equal(t, http.StatusUnauthorized, res.StatusCode, name)
		}
		res := devwsRequest(ctx, t, client, http.MethodGet, path+"?"+codersdk.SessionTokenCookie+"="+client.SessionToken(), nil, nil)
		_ = res.Body.Close()
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, "QueryOnly")
		require.False(t, controller.saw(http.MethodGet, path))
	})

	t.Run("Streaming", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		ws := uuid.NewString()
		release := make(chan struct{})
		controller.handle(ws, func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", "application/x-ndjson")
			rw.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(rw, `{"type":"stdout","data":"first"}`+"\n")
			rw.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(rw, `{"type":"exit","code":0}`+"\n")
		})

		res := devwsRequest(ctx, t, client, http.MethodPost, "/v1/me/workspaces/"+ws+"/exec", []byte(`{"argv":["true"]}`), sessionHeader(client))
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, "application/x-ndjson", res.Header.Get("Content-Type"))
		lines := make(chan string)
		scanner := bufio.NewScanner(res.Body)
		go func() {
			defer close(lines)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-ctx.Done():
					return
				}
			}
		}()
		// The first frame arrives while the controller still holds the
		// response open.
		require.Equal(t, `{"type":"stdout","data":"first"}`, testutil.RequireReceive(ctx, t, lines))
		close(release)
		require.Equal(t, `{"type":"exit","code":0}`, testutil.RequireReceive(ctx, t, lines))
		_, ok := <-lines
		require.False(t, ok)
	})

	t.Run("ClientCancel", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		ws := uuid.NewString()
		started := make(chan struct{}, 1)
		canceled := make(chan struct{}, 1)
		controller.handle(ws, func(rw http.ResponseWriter, r *http.Request) {
			rw.WriteHeader(http.StatusOK)
			rw.(http.Flusher).Flush()
			started <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
		})

		reqCtx, cancel := context.WithCancel(ctx)
		res := devwsRequest(reqCtx, t, client, http.MethodGet, "/v1/me/power-transitions/"+ws+"?wait_seconds=25", nil, sessionHeader(client))
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		testutil.RequireReceive(ctx, t, started)
		cancel()
		testutil.RequireReceive(ctx, t, canceled)
	})
}

func TestCooplayDevwsProxyDisabled(t *testing.T) {
	t.Parallel()
	client, _ := newDevwsCoderd(t, "")
	ctx := testutil.Context(t, testutil.WaitLong)
	res := devwsRequest(ctx, t, client, http.MethodGet, "/v1/me", nil, sessionHeader(client))
	defer res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestCooplayDevwsProxyControllerDown(t *testing.T) {
	t.Parallel()
	// A port nothing listens on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	controllerURL := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	client, _ := newDevwsCoderd(t, controllerURL)
	ctx := testutil.Context(t, testutil.WaitLong)
	res := devwsRequest(ctx, t, client, http.MethodGet, "/v1/me", nil, sessionHeader(client))
	defer res.Body.Close()
	require.Equal(t, http.StatusBadGateway, res.StatusCode)
	var resp codersdk.Response
	require.NoError(t, json.NewDecoder(res.Body).Decode(&resp))
	require.Equal(t, "The devws controller is unreachable.", resp.Message)
}

func TestValidateCooplayDevwsControllerURL(t *testing.T) {
	t.Parallel()

	for raw, valid := range map[string]bool{
		"":                             true,
		"http://127.0.0.1:7410":        true,
		"https://controller.internal":  true,
		"http://127.0.0.1:7410/prefix": true,
		"localhost:7410":               false,
		"unix:///run/devws/user.sock":  false,
		"http://":                      false,
		"http://user:pw@127.0.0.1":     false,
		"http://127.0.0.1:7410/?a=b":   false,
		"http://127.0.0.1:7410/#frag":  false,
		"ftp://127.0.0.1":              false,
	} {
		u, err := url.Parse(raw)
		require.NoError(t, err, raw)
		err = coderd.ValidateCooplayDevwsControllerURL(u)
		if valid {
			require.NoError(t, err, raw)
		} else {
			require.Error(t, err, raw)
		}
	}
}
