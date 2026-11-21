package coderd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/mock/gomock"

	"github.com/coder/coder/v2/coderd/apikey"
	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbmock"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/telemetry"
	"github.com/coder/coder/v2/coderd/tracing"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/serpent"
)

const (
	handoffTestPath     = "/api/v2" + cliHandoffPath
	handoffTestCallback = "http://127.0.0.1:8765/cooplay/coder-token"
	handoffTestState    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdef"
	handoffTestIP       = "192.0.2.1"
	handoffTestLifetime = 30 * 24 * time.Hour
	// Syntactically valid test-only credentials must not trigger a lookup on
	// routes that skip the global API key precheck.
	handoffIgnoredCredential = "0123456789-aaaaaaaaaaaaaaaaaaaaaa"

	// RFC 7636 appendix B.
	handoffTestVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	handoffTestChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

func handoffTestQuery() url.Values {
	return url.Values{
		"callback": {handoffTestCallback}, "state": {handoffTestState},
		"code_challenge": {handoffTestChallenge}, "code_challenge_method": {"S256"},
	}
}

func TestCLIHandoffAuthorizeParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		param string
		value string
		valid bool
	}{
		{name: "Valid", valid: true},
		{name: "MinPort", param: "callback", value: "http://127.0.0.1:1024/cooplay/coder-token", valid: true},
		{name: "MaxPort", param: "callback", value: "http://127.0.0.1:65535/cooplay/coder-token", valid: true},
		{name: "MissingPort", param: "callback", value: "http://127.0.0.1/cooplay/coder-token"},
		{name: "ZeroPort", param: "callback", value: "http://127.0.0.1:0/cooplay/coder-token"},
		{name: "LowPort", param: "callback", value: "http://127.0.0.1:1023/cooplay/coder-token"},
		{name: "HighPort", param: "callback", value: "http://127.0.0.1:65536/cooplay/coder-token"},
		{name: "NonNumericPort", param: "callback", value: "http://127.0.0.1:test/cooplay/coder-token"},
		{name: "Localhost", param: "callback", value: "http://localhost:8765/cooplay/coder-token"},
		{name: "OtherHost", param: "callback", value: "http://example.test:8765/cooplay/coder-token"},
		{name: "OtherLoopbackHost", param: "callback", value: "http://127.0.0.2:8765/cooplay/coder-token"},
		{name: "IPv6", param: "callback", value: "http://[::1]:8765/cooplay/coder-token"},
		{name: "HTTPS", param: "callback", value: "https://127.0.0.1:8765/cooplay/coder-token"},
		{name: "Userinfo", param: "callback", value: "http://user@127.0.0.1:8765/cooplay/coder-token"},
		{name: "WrongPath", param: "callback", value: "http://127.0.0.1:8765/cooplay/other"},
		{name: "EscapedPath", param: "callback", value: "http://127.0.0.1:8765/cooplay/%63oder-token"},
		{name: "ExtraQuery", param: "callback", value: handoffTestCallback + "?a=b"},
		{name: "EmptyQuery", param: "callback", value: handoffTestCallback + "?"},
		{name: "Fragment", param: "callback", value: handoffTestCallback + "#fragment"},
		{name: "EmptyFragment", param: "callback", value: handoffTestCallback + "#"},
		{name: "ShortState", param: "state", value: strings.Repeat("a", 42)},
		{name: "LongState", param: "state", value: strings.Repeat("a", 44)},
		{name: "InvalidState", param: "state", value: "+" + strings.Repeat("a", 42)},
		{name: "ShortChallenge", param: "code_challenge", value: strings.Repeat("a", 42)},
		{name: "LongChallenge", param: "code_challenge", value: strings.Repeat("a", 44)},
		{name: "InvalidChallenge", param: "code_challenge", value: "/" + strings.Repeat("a", 42)},
		{name: "WrongMethod", param: "code_challenge_method", value: "plain"},
		{name: "WrongMethodCase", param: "code_challenge_method", value: "s256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := handoffTestQuery()
			if tt.param != "" {
				q.Set(tt.param, tt.value)
			}
			got, ok := parseCLIHandoffAuthorize(q.Encode())
			require.Equal(t, tt.valid, ok)
			if tt.valid {
				require.True(t, got.callback.String() == q.Get("callback"))
				require.True(t, got.state == q.Get("state"))
				require.True(t, got.challenge == q.Get("code_challenge"))
			}
		})
	}
	for param := range handoffTestQuery() {
		t.Run("Missing_"+param, func(t *testing.T) {
			t.Parallel()
			q := handoffTestQuery()
			q.Del(param)
			_, ok := parseCLIHandoffAuthorize(q.Encode())
			require.False(t, ok)
		})
		t.Run("Duplicate_"+param, func(t *testing.T) {
			t.Parallel()
			q := handoffTestQuery()
			q.Add(param, q.Get(param))
			_, ok := parseCLIHandoffAuthorize(q.Encode())
			require.False(t, ok)
		})
	}
	_, ok := parseCLIHandoffAuthorize(handoffTestQuery().Encode() + "&bad=%zz")
	require.False(t, ok)
}

func TestCLIHandoffCodes(t *testing.T) {
	t.Parallel()

	now := time.Now()
	codes := newCLIHandoffCodes(func() time.Time { return now })
	userID := uuid.New()
	code := issueCLIHandoffTestCode(t, codes, userID)
	data, err := base64.RawURLEncoding.DecodeString(code)
	require.NoError(t, err)
	require.Len(t, data, 32)
	require.Contains(t, codes.entries, sha256.Sum256([]byte(code)))
	entry, reason := codes.consume(code)
	require.Empty(t, reason)
	require.Equal(t, userID, entry.userID)
	require.True(t, entry.challenge == handoffTestChallenge)
	require.True(t, entry.callback == handoffTestCallback)
	require.Equal(t, now.Add(cliHandoffLifetime), entry.expiresAt)
	_, reason = codes.consume(code)
	require.Equal(t, "unknown or already used code", reason)

	code = issueCLIHandoffTestCode(t, codes, userID)
	now = now.Add(cliHandoffLifetime)
	_, reason = codes.consume(code)
	require.Equal(t, "expired code", reason)
	require.Empty(t, codes.entries)
}

func TestCLIHandoffCapacity(t *testing.T) {
	t.Parallel()

	now := time.Now()
	codes := newCLIHandoffCodes(func() time.Time { return now })
	userID := uuid.New()
	for range cliHandoffCapacity {
		issueCLIHandoffTestCode(t, codes, userID)
	}
	_, ok := codes.issue(userID, handoffTestChallenge, handoffTestCallback)
	require.False(t, ok)
	require.Len(t, codes.entries, cliHandoffCapacity)
	now = now.Add(cliHandoffLifetime)
	issueCLIHandoffTestCode(t, codes, userID)
	require.Len(t, codes.entries, 1)
}

func TestCLIHandoffConcurrentConsume(t *testing.T) {
	t.Parallel()

	codes := newCLIHandoffCodes(time.Now)
	code := issueCLIHandoffTestCode(t, codes, uuid.New())
	var wg sync.WaitGroup
	var winners atomic.Int32
	start := make(chan struct{})
	for range 64 {
		wg.Go(func() {
			<-start
			if _, reason := codes.consume(code); reason == "" {
				winners.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
}

func TestCLIHandoffPKCE(t *testing.T) {
	t.Parallel()

	require.True(t, cliHandoffMatchesChallenge(handoffTestVerifier, handoffTestChallenge))
	require.False(t, cliHandoffMatchesChallenge(handoffTestVerifier, "A"+handoffTestChallenge[1:]))
	require.False(t, cliHandoffMatchesChallenge(handoffTestVerifier, handoffTestChallenge[:42]+"A"))
	require.False(t, cliHandoffMatchesChallenge(handoffTestVerifier, handoffTestChallenge[:42]))
	for _, verifier := range []string{
		strings.Repeat("a", 42), strings.Repeat("a", 129), strings.Repeat("a", 42) + "+",
		strings.Repeat("a", 42) + "/", strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + "\n",
	} {
		hash := sha256.Sum256([]byte(verifier))
		require.False(t, cliHandoffMatchesChallenge(verifier, base64.RawURLEncoding.EncodeToString(hash[:])))
	}
	for _, verifier := range []string{handoffTestVerifier, strings.Repeat("a", 124) + "._~-"} {
		hash := sha256.Sum256([]byte(verifier))
		require.True(t, cliHandoffMatchesChallenge(verifier, base64.RawURLEncoding.EncodeToString(hash[:])))
	}
}

func TestCLIHandoffAuthorize(t *testing.T) {
	t.Parallel()

	t.Run("IssuesCode", func(t *testing.T) {
		t.Parallel()
		h := newCLIHandoffTest(t)
		user := database.User{ID: uuid.New(), Status: database.UserStatusActive}
		cookie := h.expectSession(t, user.ID, user.Status, database.ApiKeyScopeCoderAll)
		h.db.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)

		rec := h.authorize(handoffTestQuery(), cookie)
		require.Equal(t, http.StatusSeeOther, rec.Code)
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
		require.Zero(t, rec.Body.Len())
		location := rec.Header().Get("Location")
		u, err := url.Parse(location)
		require.NoError(t, err)
		code := u.Query().Get("code")
		require.True(t, cliHandoffURLValue.MatchString(code))
		require.True(t, location == handoffTestCallback+"?code="+code+"&state="+handoffTestState)
		entry, reason := h.codes.consume(code)
		require.Empty(t, reason)
		require.Equal(t, user.ID, entry.userID)
		require.True(t, entry.callback == handoffTestCallback)
		require.True(t, entry.challenge == handoffTestChallenge)
	})

	t.Run("InvalidBeforeAuthentication", func(t *testing.T) {
		t.Parallel()
		h := newCLIHandoffTest(t)
		q := handoffTestQuery()
		q.Set("callback", "http://localhost:8765/cooplay/coder-token")
		rec := h.authorize(q, handoffIgnoredCredential)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
		require.Empty(t, rec.Header().Get("Location"))
		require.Empty(t, h.codes.entries)
	})

	t.Run("Unauthenticated", func(t *testing.T) {
		t.Parallel()
		h := newCLIHandoffTest(t)
		q := handoffTestQuery()
		req := httptest.NewRequest(http.MethodGet, handoffTestPath+"/authorize?"+q.Encode(), nil)
		// A request with a header credential still needs the browser cookie.
		req.Header.Set(codersdk.SessionTokenHeader, handoffIgnoredCredential)
		rec := h.serve(req)
		require.Equal(t, http.StatusSeeOther, rec.Code)
		u, err := url.Parse(rec.Header().Get("Location"))
		require.NoError(t, err)
		require.Equal(t, "/login", u.Path)
		require.True(t, u.Query().Get("redirect") == req.URL.RequestURI())
		require.Empty(t, h.codes.entries)
		spans := h.traces.Ended()
		require.NotEmpty(t, spans)
		for _, span := range spans {
			for _, attr := range span.Attributes() {
				value := attr.Value.AsString()
				require.False(t, strings.Contains(value, handoffTestState), "trace contains handoff state")
				require.False(t, strings.Contains(value, handoffTestChallenge), "trace contains handoff challenge")
				require.False(t, strings.Contains(value, handoffTestCallback), "trace contains handoff callback")
			}
		}
	})

	t.Run("Ineligible", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name   string
			status database.UserStatus
			system bool
			scope  database.APIKeyScope
		}{
			{name: "Suspended", status: database.UserStatusSuspended, scope: database.ApiKeyScopeCoderAll},
			{name: "System", status: database.UserStatusActive, system: true, scope: database.ApiKeyScopeCoderAll},
			{name: "RestrictedScope", status: database.UserStatusActive, scope: database.ApiKeyScopeCoderApplicationConnect},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				h := newCLIHandoffTest(t)
				user := database.User{ID: uuid.New(), Status: tt.status, IsSystem: tt.system}
				// Simulate a status change between authentication and issuance.
				cookie := h.expectSession(t, user.ID, database.UserStatusActive, tt.scope)
				h.db.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
				rec := h.authorize(handoffTestQuery(), cookie)
				require.Equal(t, http.StatusForbidden, rec.Code)
				require.Empty(t, rec.Header().Get("Location"))
				require.Empty(t, h.codes.entries)
			})
		}
	})
}

func TestCLIHandoffExchange(t *testing.T) {
	t.Parallel()
	h := newCLIHandoffTest(t)
	user := database.User{ID: uuid.New(), Status: database.UserStatusActive}
	code := issueCLIHandoffTestCode(t, h.codes, user.ID)
	h.db.EXPECT().GetUserByID(gomock.Any(), user.ID).Return(user, nil)
	var inserted database.InsertAPIKeyParams
	h.db.EXPECT().InsertAPIKey(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, params database.InsertAPIKeyParams) (database.APIKey, error) {
			actor, ok := dbauthz.ActorFromContext(ctx)
			require.True(t, ok)
			require.Equal(t, rbac.SubjectTypeSystemRestricted, actor.Type)
			inserted = params
			return database.APIKey{
				ID: params.ID, UserID: params.UserID, LoginType: params.LoginType,
				HashedSecret: params.HashedSecret, Scopes: params.Scopes,
			}, nil
		})
	fields := handoffTokenFields(code)
	rec := h.exchange(t, fields)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Empty(t, rec.Header().Get("Set-Cookie"))
	require.Equal(t, database.LoginTypePassword, inserted.LoginType)
	require.Equal(t, user.ID, inserted.UserID)
	require.EqualValues(t, handoffTestLifetime.Seconds(), inserted.LifetimeSeconds)
	require.WithinDuration(t, inserted.CreatedAt.Add(handoffTestLifetime), inserted.ExpiresAt, time.Second)
	require.Equal(t, handoffTestIP, inserted.IPAddress.IPNet.IP.String())
	var response codersdk.GenerateAPIKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	id, secret, found := strings.Cut(response.Key, "-")
	require.True(t, found)
	require.Equal(t, inserted.ID, id)
	require.True(t, apikey.ValidateHash(inserted.HashedSecret, secret))
	require.Empty(t, rec.Header().Get("Location"))

	logs := h.auditor.AuditLogs()
	require.Len(t, logs, 1)
	require.Equal(t, database.AuditActionCreate, logs[0].Action)
	require.Equal(t, database.ResourceTypeApiKey, logs[0].ResourceType)
	require.Equal(t, user.ID, logs[0].UserID)
	require.Equal(t, user.ID, logs[0].ResourceID)
	require.EqualValues(t, http.StatusCreated, logs[0].StatusCode)
	requireCLIHandoffRejection(t, h.exchange(t, fields))
	require.Len(t, h.auditor.AuditLogs(), 1)
}

func TestCLIHandoffExchangeRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*cliHandoffTest, uuid.UUID, map[string]any)
	}{
		{name: "Expired", change: func(h *cliHandoffTest, _ uuid.UUID, _ map[string]any) {
			h.now = h.now.Add(cliHandoffLifetime)
		}},
		{name: "WrongVerifier", change: func(_ *cliHandoffTest, _ uuid.UUID, m map[string]any) {
			m["code_verifier"] = strings.Repeat("a", 43)
		}},
		{name: "MalformedVerifier", change: func(_ *cliHandoffTest, _ uuid.UUID, m map[string]any) {
			m["code_verifier"] = 123
		}},
		{name: "CallbackMismatch", change: func(_ *cliHandoffTest, _ uuid.UUID, m map[string]any) {
			m["callback"] = "http://127.0.0.1:8766/cooplay/coder-token"
		}},
		{name: "MissingCallback", change: func(_ *cliHandoffTest, _ uuid.UUID, m map[string]any) {
			delete(m, "callback")
		}},
		{name: "SuspendedUser", change: func(h *cliHandoffTest, id uuid.UUID, _ map[string]any) {
			h.db.EXPECT().GetUserByID(gomock.Any(), id).Return(database.User{ID: id, Status: database.UserStatusSuspended}, nil)
		}},
		{name: "SystemUser", change: func(h *cliHandoffTest, id uuid.UUID, _ map[string]any) {
			h.db.EXPECT().GetUserByID(gomock.Any(), id).Return(database.User{ID: id, Status: database.UserStatusActive, IsSystem: true}, nil)
		}},
		{name: "DeletedUser", change: func(h *cliHandoffTest, id uuid.UUID, _ map[string]any) {
			h.db.EXPECT().GetUserByID(gomock.Any(), id).Return(database.User{ID: id, Status: database.UserStatusActive, Deleted: true}, nil)
		}},
		{name: "MissingUser", change: func(h *cliHandoffTest, id uuid.UUID, _ map[string]any) {
			h.db.EXPECT().GetUserByID(gomock.Any(), id).Return(database.User{}, sql.ErrNoRows)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newCLIHandoffTest(t)
			id := uuid.New()
			code := issueCLIHandoffTestCode(t, h.codes, id)
			fields := handoffTokenFields(code)
			tt.change(h, id, fields)
			requireCLIHandoffRejection(t, h.exchange(t, fields))
			require.Empty(t, h.codes.entries)
			requireCLIHandoffRejection(t, h.exchange(t, handoffTokenFields(code)))
			require.Empty(t, h.auditor.AuditLogs())
		})
	}

	t.Run("UnknownCode", func(t *testing.T) {
		t.Parallel()
		h := newCLIHandoffTest(t)
		requireCLIHandoffRejection(t, h.exchange(t, handoffTokenFields(strings.Repeat("a", 43))))
	})
	t.Run("BodyLimit", func(t *testing.T) {
		t.Parallel()
		h := newCLIHandoffTest(t)
		req := httptest.NewRequest(http.MethodPost, handoffTestPath+"/token", strings.NewReader(strings.Repeat(" ", cliHandoffBodyLimit)+"{}"))
		requireCLIHandoffRejection(t, h.serve(req))
	})
}

func TestCLIHandoffProbeAndRateLimit(t *testing.T) {
	t.Parallel()
	h := newCLIHandoffTest(t)
	req := httptest.NewRequest(http.MethodGet, handoffTestPath, nil)
	req.AddCookie(&http.Cookie{Name: codersdk.SessionTokenCookie, Value: handoffIgnoredCredential})
	rec := h.serve(req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"version":1}`, rec.Body.String())

	request := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, handoffTestPath+"/token", strings.NewReader("{}"))
		req.RemoteAddr = ip
		req.Header.Set(codersdk.SessionTokenHeader, handoffIgnoredCredential)
		req.Header.Set(codersdk.BypassRatelimitHeader, "true")
		return h.serve(req)
	}
	requireCLIHandoffRejection(t, request(handoffTestIP))
	requireCLIHandoffRejection(t, request(handoffTestIP))
	require.Equal(t, http.StatusTooManyRequests, request(handoffTestIP).Code)
	requireCLIHandoffRejection(t, request("192.0.2.2"))
}

type cliHandoffTest struct {
	db      *dbmock.MockStore
	auditor *audit.MockAuditor
	traces  *tracetest.SpanRecorder
	codes   *cliHandoffCodes
	now     time.Time
	router  http.Handler
}

func newCLIHandoffTest(t *testing.T) *cliHandoffTest {
	t.Helper()
	h := &cliHandoffTest{
		db: dbmock.NewMockStore(gomock.NewController(t)), auditor: audit.NewMock(), now: time.Now(),
		traces: tracetest.NewSpanRecorder(),
	}
	h.codes = newCLIHandoffCodes(func() time.Time { return h.now })
	deployment := &codersdk.DeploymentValues{}
	deployment.Sessions.DefaultTokenDuration = serpent.Duration(handoffTestLifetime)
	api := &API{
		Options: &Options{
			Database: h.db, DeploymentValues: deployment, Telemetry: telemetry.NewNoop(), LoginRateLimit: 2,
		},
		HTTPAuth: &HTTPAuthorizer{Authorizer: rbac.NewAuthorizer(nil)},
	}
	var auditor audit.Auditor = h.auditor
	api.Auditor.Store(&auditor)

	r := chi.NewRouter()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.traces))
	r.Use(tracing.StatusWriterMiddleware, tracing.Middleware(tracer),
		httpmw.AttachRequestID, singleSlashMW,
		skipCooplayCLIHandoffPrecheck(httpmw.PrecheckAPIKey(httpmw.ValidateAPIKeyConfig{DB: h.db})))
	r.Route("/api/v2", func(r chi.Router) {
		api.registerCooplayCLIHandoff(r, httpmw.ExtractAPIKeyConfig{
			DB: h.db, RedirectToLogin: true, DisableSessionExpiryRefresh: true,
		}, h.codes)
	})
	h.router = r
	return h
}

func (h *cliHandoffTest) expectSession(t *testing.T, id uuid.UUID, status database.UserStatus, scope database.APIKeyScope) string {
	t.Helper()
	params, token, err := apikey.Generate(apikey.CreateParams{
		UserID: id, LoginType: database.LoginTypePassword, DefaultLifetime: handoffTestLifetime,
		Scopes: database.APIKeyScopes{scope},
	})
	require.NoError(t, err)
	h.db.EXPECT().GetAPIKeyByID(gomock.Any(), params.ID).Return(database.APIKey{
		ID: params.ID, UserID: id, LoginType: params.LoginType, LastUsed: dbtime.Now(),
		ExpiresAt: params.ExpiresAt, HashedSecret: params.HashedSecret, Scopes: params.Scopes,
		AllowList: params.AllowList,
	}, nil)
	h.db.EXPECT().GetAuthorizationUserRoles(gomock.Any(), id).Return(database.GetAuthorizationUserRolesRow{
		ID: id, Status: status, Roles: []string{codersdk.RoleMember},
	}, nil)
	return token
}

func (h *cliHandoffTest) authorize(q url.Values, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, handoffTestPath+"/authorize?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: codersdk.SessionTokenCookie, Value: cookie})
	return h.serve(req)
}

func (h *cliHandoffTest) exchange(t *testing.T, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(fields)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, handoffTestPath+"/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = handoffTestIP
	return h.serve(req)
}

func (h *cliHandoffTest) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func handoffTokenFields(code string) map[string]any {
	return map[string]any{"code": code, "callback": handoffTestCallback, "code_verifier": handoffTestVerifier}
}

func issueCLIHandoffTestCode(t *testing.T, codes *cliHandoffCodes, id uuid.UUID) string {
	t.Helper()
	code, ok := codes.issue(id, handoffTestChallenge, handoffTestCallback)
	require.True(t, ok)
	return code
}

func requireCLIHandoffRejection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, `{"message":"invalid handoff"}`, rec.Body.String())
	require.Empty(t, rec.Header().Get("Set-Cookie"))
}
