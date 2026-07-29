package server_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	loamweb "github.com/bobcob7/loam/web"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assetRefPattern extracts the root-absolute src=/href= targets Vite writes
// into index.html ("/assets/index-DllTWVUb.js"). It deliberately matches
// only root-absolute refs: those are the ones a Vite `base` misconfiguration
// breaks, and the only ones this server's own routing is responsible for.
var assetRefPattern = regexp.MustCompile(`(?:src|href)="(/[^"]*)"`)

// TestSPA_EmbeddedBuildServesItsOwnAssets is the one test that exercises the
// REAL embedded filesystem (web.Dist()) rather than an fstest.MapFS stand-in.
// Every other SPA test in this package builds a synthetic three-file MapFS,
// which cannot catch the failure this one exists for: the SPA compiles, the
// binary embeds it, the page loads -- and every asset 404s at runtime because
// index.html points somewhere the server does not serve (a Vite `base` other
// than "/", or an asset that never made it past `//go:embed all:dist`).
//
// It asserts the round trip that actually matters: for each asset index.html
// REFERENCES, the running handler serves that exact URL with a 200. Nothing
// here hardcodes a hashed filename -- those change on every build -- so the
// test is build-agnostic and needs no updating when the bundle changes.
//
// NOTE ON WHEN THIS ACTUALLY RUNS: web/dist ships two committed placeholders
// (loam-nvb.15 / loam-68os) so `go build ./...` works on a clean checkout
// with no Node. That placeholder index.html references NO assets, so this
// test would be VACUOUSLY GREEN against it -- passing while proving nothing.
// It therefore SKIPS LOUDLY rather than passing in that case. Run it against
// real output with `task test:spa`, which builds the SPA first and restores
// the placeholders afterward.
func TestSPA_EmbeddedBuildServesItsOwnAssets(t *testing.T) {
	t.Parallel()
	distFS := loamweb.Dist()
	index, err := fs.ReadFile(distFS, "index.html")
	require.NoError(t, err, "the embedded SPA must always have an index.html; //go:embed all:dist makes a missing one a compile error, so this failing means the embed root moved")
	refs := assetRefPattern.FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Skip("embedded web/dist is the committed placeholder (no asset references in index.html), so there is nothing to prove here -- run `task test:spa` to build the real SPA and exercise this")
	}
	srv := httptest.NewServer(spaOnlyRouter(t, distFS))
	t.Cleanup(srv.Close)
	for _, ref := range refs {
		assetPath := ref[1]
		t.Run(assetPath, func(t *testing.T) {
			t.Parallel()
			// Two distinct failures are worth telling apart. If the file is
			// absent from the embedded FS, the embed dropped it. If it is
			// present but the request does not return it, routing is wrong
			// -- spaHandler fell back to index.html, which is precisely the
			// symptom a browser reports as "unexpected token '<'" when it
			// tries to parse HTML as JavaScript.
			_, statErr := fs.Stat(distFS, strings.TrimPrefix(assetPath, "/"))
			require.NoError(t, statErr, "index.html references %s but it is not in the embedded filesystem", assetPath)
			resp := getWithAdminAuth(t, srv.URL+assetPath)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode, "referenced asset %s did not serve", assetPath)
			assert.NotContains(t, resp.Header.Get("Content-Type"), "text/html", "%s served as HTML, which means the SPA fallback swallowed it instead of the file server returning the real asset", assetPath)
		})
	}
}

// TestSPA_EmbeddedBuild_UnknownRouteFallsBackToIndex pins the other half of
// the contract against the real embedded build: a client-side route that is
// not a file must return index.html so wouter can take over. The path used
// is a real one from the app's own route table (a proposal detail URL, whose
// two-segment repo identifier makes it the most structurally awkward route
// the SPA has).
func TestSPA_EmbeddedBuild_UnknownRouteFallsBackToIndex(t *testing.T) {
	t.Parallel()
	distFS := loamweb.Dist()
	index, err := fs.ReadFile(distFS, "index.html")
	require.NoError(t, err)
	srv := httptest.NewServer(spaOnlyRouter(t, distFS))
	t.Cleanup(srv.Close)
	resp := getWithAdminAuth(t, srv.URL+"/proposals/acme/widgets/wb-9c2f1a")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body := readAllString(t, resp)
	assert.Equal(t, string(index), body, "an unknown client-side route must serve index.html byte-for-byte so the SPA router can take over")
}

// spaOnlyRouter wires the real RegisterSPA against distFS with the real
// httpauth middleware -- no fake stands in for either.
func spaOnlyRouter(t *testing.T, distFS fs.FS) http.Handler {
	t.Helper()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	router.RegisterSPA(distFS)
	return router.Handler()
}

func getWithAdminAuth(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	req.SetBasicAuth(testAdminUser, testAdminPassword)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
