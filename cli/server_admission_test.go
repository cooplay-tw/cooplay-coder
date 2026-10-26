package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/cli/clitest"
	"github.com/coder/coder/v2/coderd/workspaceadmission/admissionapi"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/coder/v2/testutil/expecter"
)

func writeAdmissionTokenHash(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	path := filepath.Join(t.TempDir(), "admission-token.sha256")
	require.NoError(t, os.WriteFile(path, []byte("sha256:"+hex.EncodeToString(sum[:])), 0o600))
	return path
}

func admissionGet(ctx context.Context, t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, body
}

func TestServer_WorkspaceAdmissionListener(t *testing.T) {
	t.Parallel()

	t.Run("SeparateListener", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		const token = "controller-token"
		inv, _ := clitest.New(t,
			"server",
			dbArg(t),
			"--http-address", "127.0.0.1:0",
			"--access-url", "http://example.com",
			"--workspace-admission-listen-address", "127.0.0.1:0",
			"--workspace-admission-token-hash-file", writeAdmissionTokenHash(t, token),
			"--cache-dir", t.TempDir(),
		)
		stdout := expecter.NewAttachedToInvocation(t, inv)
		clitest.Start(t, inv)

		const httpLinePrefix = "Started HTTP listener at"
		stdout.ExpectMatch(ctx, httpLinePrefix)
		mainURL := strings.TrimSpace(stdout.ReadLine(ctx))
		require.NotEmpty(t, mainURL)
		const admissionLinePrefix = "Started workspace admission listener at"
		stdout.ExpectMatch(ctx, admissionLinePrefix)
		admissionURL := strings.TrimSpace(stdout.ReadLine(ctx))
		require.True(t, strings.HasPrefix(admissionURL, "http://127.0.0.1:"), admissionURL)
		require.NotEqual(t, mainURL, admissionURL)

		status, _ := admissionGet(ctx, t, admissionURL+"/healthz", "")
		require.Equal(t, http.StatusUnauthorized, status)
		status, _ = admissionGet(ctx, t, admissionURL+"/healthz", "wrong")
		require.Equal(t, http.StatusUnauthorized, status)
		status, body := admissionGet(ctx, t, admissionURL+"/v1/requests", token)
		require.Equal(t, http.StatusOK, status, string(body))
		var list admissionapi.ListRequestsResponse
		require.NoError(t, json.Unmarshal(body, &list))
		require.NotNil(t, list.Requests)

		// The employee-facing listener does not serve the admission API.
		for _, path := range []string{"/v1/requests", "/api/v2/v1/requests", "/healthz"} {
			_, body := admissionGet(ctx, t, mainURL+path, token)
			var list admissionapi.ListRequestsResponse
			if json.Unmarshal(body, &list) == nil {
				require.Nil(t, list.Requests, path)
			}
		}
	})

	t.Run("MissingTokenHashFile", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)
		inv, _ := clitest.New(t,
			"server",
			dbArg(t),
			"--http-address", "127.0.0.1:0",
			"--access-url", "http://example.com",
			"--workspace-admission-listen-address", "127.0.0.1:0",
			"--workspace-admission-token-hash-file", filepath.Join(t.TempDir(), "missing"),
			"--cache-dir", t.TempDir(),
		)
		err := inv.WithContext(ctx).Run()
		require.ErrorContains(t, err, "workspace admission")
	})
}
