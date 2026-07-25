package forge

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestForgejo_ValidateToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{name: "valid token", statusCode: http.StatusOK, wantErr: nil},
		{name: "invalid token rejected", statusCode: http.StatusUnauthorized, wantErr: ErrInvalidToken},
		{name: "forbidden token rejected", statusCode: http.StatusForbidden, wantErr: ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/user", r.URL.Path)
				assert.Equal(t, "token good-token", r.Header.Get("Authorization"))
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()
			f := NewForgejo(server.URL, "unused", server.Client(), testLogger())
			err := f.ValidateToken(t.Context(), server.URL, "good-token")
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestForgejo_ValidateToken_NetworkFailure(t *testing.T) {
	t.Parallel()
	f := NewForgejo("http://127.0.0.1:0", "unused", http.DefaultClient, testLogger())
	err := f.ValidateToken(t.Context(), "http://127.0.0.1:0", "token")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidToken)
}

func TestForgejo_CreatePR(t *testing.T) {
	t.Parallel()
	const wantPRURL = "https://forgejo.example.com/acme/widgets/pulls/7"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/repos/acme/widgets/pulls", r.URL.Path)
		assert.Equal(t, "token secret", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "wb-9c2f1a", body["head"])
		assert.Equal(t, "main", body["base"])
		assert.Equal(t, "Add widget", body["title"])
		assert.Equal(t, "does widget things", body["body"])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"html_url": wantPRURL, "number": 7})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	prURL, prNumber, err := f.CreatePR(t.Context(), "acme/widgets", "wb-9c2f1a", "main", "Add widget", "does widget things")
	require.NoError(t, err)
	assert.Equal(t, wantPRURL, prURL)
	assert.Equal(t, 7, prNumber)
}

func TestForgejo_CreatePR_RepoNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	_, _, err := f.CreatePR(t.Context(), "acme/missing", "wb-1", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoNotFound)
}

func TestForgejo_GetPRState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		state     string
		merged    bool
		wantState string
	}{
		{name: "open", state: "open", merged: false, wantState: "open"},
		{name: "merged", state: "closed", merged: true, wantState: "merged"},
		{name: "closed without merge", state: "closed", merged: false, wantState: "closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"state": tt.state, "merged": tt.merged, "number": 7})
			}))
			defer server.Close()
			f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
			state, err := f.GetPRState(t.Context(), "acme/widgets", 7)
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, state)
		})
	}
}

func TestForgejo_ClosePR(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "closed", body["state"])
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "closed", "number": 7})
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "secret", server.Client(), testLogger())
	err := f.ClosePR(t.Context(), "acme/widgets", 7)
	require.NoError(t, err)
}

func TestForgejo_ClosePR_AuthFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	f := NewForgejo(server.URL, "bad", server.Client(), testLogger())
	err := f.ClosePR(t.Context(), "acme/widgets", 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestForgejo_GitCredentials(t *testing.T) {
	t.Parallel()
	f := NewForgejo("forgejo.example.com", "", http.DefaultClient, testLogger())
	username, password, err := f.GitCredentials(t.Context(), "the-token")
	require.NoError(t, err)
	assert.Equal(t, gitUsername, username)
	assert.Equal(t, "the-token", password)
}

func TestForgejo_GitCredentials_EmptyToken(t *testing.T) {
	t.Parallel()
	f := NewForgejo("forgejo.example.com", "", http.DefaultClient, testLogger())
	_, _, err := f.GitCredentials(t.Context(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestApiBaseURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://forgejo.example.com/api/v1", apiBaseURL("forgejo.example.com"))
	assert.Equal(t, "http://127.0.0.1:8080/api/v1", apiBaseURL("http://127.0.0.1:8080"))
}
