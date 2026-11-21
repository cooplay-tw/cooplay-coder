package coderd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/apikey"
	"github.com/coder/coder/v2/coderd/audit"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/httpapi"
	"github.com/coder/coder/v2/coderd/httpmw"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/coderd/rbac/policy"
	"github.com/coder/coder/v2/codersdk"
)

const (
	cliHandoffPath         = "/cooplay/cli-handoff"
	cliHandoffCallbackPath = "/cooplay/coder-token"
	cliHandoffLifetime     = 60 * time.Second
	cliHandoffCapacity     = 1000
	cliHandoffBodyLimit    = 8 << 10
)

var (
	cliHandoffURLValue = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	cliHandoffVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
)

type cliHandoffCode struct {
	userID    uuid.UUID
	challenge string
	callback  string
	expiresAt time.Time
}

type cliHandoffCodes struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]cliHandoffCode
	now     func() time.Time
}

func newCLIHandoffCodes(now func() time.Time) *cliHandoffCodes {
	return &cliHandoffCodes{entries: make(map[[sha256.Size]byte]cliHandoffCode), now: now}
}

func (s *cliHandoffCodes) issue(userID uuid.UUID, challenge, callback string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for hash, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, hash)
		}
	}
	if len(s.entries) >= cliHandoffCapacity {
		return "", false
	}

	var random [32]byte
	_, _ = rand.Read(random[:])
	code := base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(code))
	if _, exists := s.entries[hash]; exists {
		return "", false
	}
	s.entries[hash] = cliHandoffCode{
		userID: userID, challenge: challenge, callback: callback, expiresAt: now.Add(cliHandoffLifetime),
	}
	return code, true
}

// consume removes the entry even when it has expired.
func (s *cliHandoffCodes) consume(code string) (cliHandoffCode, string) {
	hash := sha256.Sum256([]byte(code))
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, found := s.entries[hash]
	delete(s.entries, hash)
	switch {
	case !found:
		return cliHandoffCode{}, "unknown or already used code"
	case !s.now().Before(entry.expiresAt):
		return cliHandoffCode{}, "expired code"
	default:
		return entry, ""
	}
}

func cliHandoffMatchesChallenge(verifier, challenge string) bool {
	if !cliHandoffVerifier.MatchString(verifier) {
		return false
	}
	hash := sha256.Sum256([]byte(verifier))
	actual := base64.RawURLEncoding.EncodeToString(hash[:])
	return subtle.ConstantTimeCompare([]byte(actual), []byte(challenge)) == 1
}

type cliHandoffAuthorizeParams struct {
	callback  *url.URL
	state     string
	challenge string
}

func parseCLIHandoffAuthorize(rawQuery string) (cliHandoffAuthorizeParams, bool) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil || len(q["callback"]) != 1 || len(q["state"]) != 1 || len(q["code_challenge"]) != 1 || len(q["code_challenge_method"]) != 1 {
		return cliHandoffAuthorizeParams{}, false
	}
	rawCallback, state, challenge := q.Get("callback"), q.Get("state"), q.Get("code_challenge")
	if !cliHandoffURLValue.MatchString(state) || !cliHandoffURLValue.MatchString(challenge) || q.Get("code_challenge_method") != "S256" {
		return cliHandoffAuthorizeParams{}, false
	}
	callback, err := url.Parse(rawCallback)
	if err != nil {
		return cliHandoffAuthorizeParams{}, false
	}
	port, err := strconv.Atoi(callback.Port())
	// Compare the complete form to exclude extra URL components, including
	// empty query and fragment delimiters and alternate path encodings.
	if err != nil || port < 1024 || port > 65535 || rawCallback != "http://127.0.0.1:"+callback.Port()+cliHandoffCallbackPath {
		return cliHandoffAuthorizeParams{}, false
	}
	return cliHandoffAuthorizeParams{callback: callback, state: state, challenge: challenge}, true
}

func skipCooplayCLIHandoffPrecheck(precheck func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		checked := precheck(next)
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			// Use the path normalized by singleSlashMW, which chi routes on.
			switch chi.RouteContext(r.Context()).RoutePath {
			case "/api/v2" + cliHandoffPath, "/api/v2" + cliHandoffPath + "/authorize", "/api/v2" + cliHandoffPath + "/token":
				// Validate authorize parameters before checking its session.
				// The other two endpoints ignore browser/API credentials.
				next.ServeHTTP(rw, r)
			default:
				checked.ServeHTTP(rw, r)
			}
		})
	}
}

func (api *API) registerCooplayCLIHandoff(r chi.Router, auth httpmw.ExtractAPIKeyConfig, codes *cliHandoffCodes) {
	auth.SessionTokenFunc = func(r *http.Request) string {
		cookie, err := r.Cookie(codersdk.SessionTokenCookie)
		if err != nil {
			return ""
		}
		return cookie.Value
	}
	r.Get(cliHandoffPath, cooplayCLIHandoffVersion)
	r.Get(cliHandoffPath+"/authorize", api.authorizeCooplayCLIHandoff(codes, httpmw.ExtractAPIKeyMW(auth)))
	r.With(httpmw.RateLimit(api.LoginRateLimit, time.Minute)).Post(cliHandoffPath+"/token", api.exchangeCooplayCLIHandoff(codes))
}

type cliHandoffVersion struct {
	Version int `json:"version"`
}

// @Summary Probe Cooplay CLI handoff support
// @ID cooplay-cli-handoff-version
// @Produce json
// @Success 200 {object} cliHandoffVersion
// @Router /api/v2/cooplay/cli-handoff [get]
// @x-apidocgen {"skip": true}
func cooplayCLIHandoffVersion(rw http.ResponseWriter, r *http.Request) {
	httpapi.Write(r.Context(), rw, http.StatusOK, cliHandoffVersion{Version: 1})
}

// @Summary Authorize a Cooplay CLI handoff
// @ID cooplay-cli-handoff-authorize
// @Param callback query string true "Loopback callback URL"
// @Param state query string true "Client state"
// @Param code_challenge query string true "PKCE S256 challenge"
// @Param code_challenge_method query string true "S256"
// @Success 303 "Redirect to the loopback callback or login page"
// @Failure 400 "Invalid handoff parameters"
// @Failure 403 "User cannot create a session key"
// @Failure 503 "Handoff code store is full"
// @Router /api/v2/cooplay/cli-handoff/authorize [get]
// @x-apidocgen {"skip": true}
func (api *API) authorizeCooplayCLIHandoff(codes *cliHandoffCodes, authenticate func(http.Handler) http.Handler) http.HandlerFunc {
	type paramsKey struct{}
	authorized := authenticate(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		params, ok := r.Context().Value(paramsKey{}).(cliHandoffAuthorizeParams)
		if !ok {
			panic("developer error: missing validated handoff parameters")
		}
		ctx := r.Context()
		userID := httpmw.APIKey(r).UserID
		user, err := api.Database.GetUserByID(ctx, userID)
		if err != nil || !cliHandoffActiveUser(user) || !api.Authorize(r, policy.ActionCreate, rbac.ResourceApiKey.WithOwner(userID.String())) {
			httpapi.Forbidden(rw)
			return
		}
		code, ok := codes.issue(userID, params.challenge, params.callback.String())
		if !ok {
			http.Error(rw, "handoff unavailable", http.StatusServiceUnavailable)
			return
		}

		api.Logger.Info(ctx, "issued cli handoff code", slog.F("user_id", userID))
		params.callback.RawQuery = url.Values{"code": {code}, "state": {params.state}}.Encode()
		rw.Header().Set("Location", params.callback.String())
		rw.WriteHeader(http.StatusSeeOther)
	}))
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("Referrer-Policy", "no-referrer")
		params, ok := parseCLIHandoffAuthorize(r.URL.RawQuery)
		if !ok {
			http.Error(rw, "invalid handoff parameters", http.StatusBadRequest)
			return
		}
		authorized.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), paramsKey{}, params)))
	}
}

func cliHandoffActiveUser(user database.User) bool {
	return user.Status == database.UserStatusActive && !user.IsSystem && !user.Deleted
}

type cliHandoffTokenRequest struct {
	Code         string          `json:"code"`
	CodeVerifier json.RawMessage `json:"code_verifier" swaggertype:"string"`
	Callback     json.RawMessage `json:"callback" swaggertype:"string"`
}

// @Summary Exchange a Cooplay CLI handoff code for a session key
// @ID cooplay-cli-handoff-token
// @Accept json
// @Produce json
// @Param request body cliHandoffTokenRequest true "Handoff code, PKCE verifier and loopback callback"
// @Success 201 {object} codersdk.GenerateAPIKeyResponse
// @Failure 400 {object} codersdk.Response
// @Failure 429 {object} codersdk.Response
// @Router /api/v2/cooplay/cli-handoff/token [post]
// @x-apidocgen {"skip": true}
func (api *API) exchangeCooplayCLIHandoff(codes *cliHandoffCodes) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rw.Header().Set("Cache-Control", "no-store")
		reject := func(reason string) {
			api.Logger.Debug(ctx, "rejected cli handoff exchange", slog.F("reason", reason))
			httpapi.Write(ctx, rw, http.StatusBadRequest, codersdk.Response{Message: "invalid handoff"})
		}

		body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, cliHandoffBodyLimit))
		var req cliHandoffTokenRequest
		if err != nil || json.Unmarshal(body, &req) != nil {
			reject("invalid request body")
			return
		}

		// Consume before interpreting the other fields so even a well-formed
		// JSON request with a malformed verifier or callback spends the code.
		entry, reason := codes.consume(req.Code)
		if reason != "" {
			reject(reason)
			return
		}
		var callback, verifier string
		if json.Unmarshal(req.Callback, &callback) != nil || callback != entry.callback {
			reject("callback mismatch")
			return
		}
		if json.Unmarshal(req.CodeVerifier, &verifier) != nil || !cliHandoffMatchesChallenge(verifier, entry.challenge) {
			reject("invalid pkce verifier")
			return
		}

		// The validated handoff grants key creation for this user only.
		//nolint:gocritic // The exchange has no authenticated request actor.
		systemCtx := dbauthz.AsSystemRestricted(ctx)
		user, err := api.Database.GetUserByID(systemCtx, entry.userID)
		if err != nil || !cliHandoffActiveUser(user) {
			reject("user unavailable or ineligible")
			return
		}

		aReq, commitAudit := audit.InitRequest[database.APIKey](rw, &audit.RequestParams{
			Audit:   *api.Auditor.Load(),
			Log:     api.Logger,
			Request: r,
			Action:  database.AuditActionCreate,
		})
		defer commitAudit()
		aReq.UserID = entry.userID
		cookie, key, err := api.createAPIKey(systemCtx, apikey.CreateParams{
			UserID:          entry.userID,
			DefaultLifetime: api.DeploymentValues.Sessions.DefaultTokenDuration.Value(),
			LoginType:       database.LoginTypePassword,
			RemoteAddr:      r.RemoteAddr,
		})
		if err != nil {
			reject("session key creation failed")
			return
		}
		aReq.New = *key
		httpapi.Write(ctx, rw, http.StatusCreated, codersdk.GenerateAPIKeyResponse{Key: cookie.Value})
	}
}
