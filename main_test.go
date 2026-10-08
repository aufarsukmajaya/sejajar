package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aufarsukmajaya/sejajar/internal/mock"
)

func TestClassify(t *testing.T) {
	v := func(commit string, pending, unknown []string) *Version {
		return &Version{Commit: commit, Schema: Schema{Pending: pending, Unknown: unknown}}
	}
	cases := []struct {
		v    *Version
		want string
	}{
		{v("h", nil, nil), "OK"},
		{v("old", nil, nil), "BEHIND"},
		{v("h", []string{"m2"}, nil), "SCHEMA_PENDING"},
		{v("old", []string{"m2"}, []string{"m9"}), "DB_AHEAD"},
	}
	for _, c := range cases {
		if got := classify("h", c.v); got != c.want {
			t.Errorf("classify(%+v) = %s, want %s", c.v, got, c.want)
		}
	}
}

func TestPick(t *testing.T) {
	all := []Site{{Name: "a"}, {Name: "b"}}
	if got, _ := pick(all, nil); len(got) != 2 {
		t.Fatalf("no names should return all, got %v", got)
	}
	if got, _ := pick(all, []string{" b"}); len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("pick b = %v", got)
	}
	if _, err := pick(all, []string{"zzz"}); err == nil {
		t.Fatal("unknown site must error")
	}
}

func TestPassword(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "correct horse") || checkPassword(h, "correct hors") || checkPassword("garbage", "x") {
		t.Fatalf("checkPassword wrong for %s", h)
	}
}

// TestEndToEnd drives the HTTP API against the mock GitLab and a real
// Postgres. Run with SEJAJAR_TEST_DATABASE_URL pointing at a scratch database;
// the test truncates its tables.
func TestEndToEnd(t *testing.T) {
	dsn := os.Getenv("SEJAJAR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SEJAJAR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := openStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, "TRUNCATE deploys, deploy_runs, site_versions, sites, sessions RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, &Config{PollSeconds: 60}, new(""), new(""), "test"); err != nil {
		t.Fatal(err)
	}
	hash, _ := hashPassword("pw-12345")
	if err := store.SetLogin(ctx, "admin", hash); err != nil {
		t.Fatal(err)
	}

	m := mock.New()
	m.Delay = 20 * time.Millisecond
	gl := httptest.NewServer(m)
	defer gl.Close()
	api := httptest.NewServer(newHandler(&Fleet{store: store, token: "token"}))
	defer api.Close()

	// A browser keeps cookies and doesn't follow redirects, so each hop is checked.
	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(c *http.Client, method, url, contentType string, body, out any) *http.Response {
		t.Helper()
		var rd bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&rd).Encode(body)
		}
		req, _ := http.NewRequest(method, url, &rd)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp
	}
	admin := browser()
	call := func(method, path, contentType string, body, out any) int {
		t.Helper()
		return do(admin, method, api.URL+path, contentType, body, out).StatusCode
	}

	// sign in with the local password
	var denied map[string]string
	if code := call("GET", "/api/status", "", nil, &denied); code != http.StatusUnauthorized || denied["login"] != "password" {
		t.Fatalf("signed out: %d %v, want 401 with login=password", code, denied)
	}
	if code := call("POST", "/auth/password", "application/json", map[string]string{"user": "admin", "password": "nope-nope"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", code)
	}
	if code := call("POST", "/auth/password", "application/json", map[string]string{"user": "admin", "password": "pw-12345"}, nil); code != 200 {
		t.Fatalf("password sign-in: %d", code)
	}

	// 0. settings live in the DB and apply without a restart
	if code := call("GET", "/api/status", "", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status: %d, want 503", code)
	}
	settings := map[string]any{"gitlab": map[string]string{"url": gl.URL, "project": "acme/app", "branch": "main"},
		"poll_seconds": 30, "version_token": "vt"}
	var sites []map[string]string
	for _, n := range mock.SiteNames {
		sites = append(sites, map[string]string{"name": n, "version_url": gl.URL + "/sites/" + n + "/version"})
	}
	settings["sites"] = append(slices.Clone(sites), map[string]string{"name": "bad name", "version_url": gl.URL})
	if code := call("PUT", "/api/settings", "application/json", settings, nil); code != http.StatusBadRequest {
		t.Errorf("invalid site name: %d, want 400", code)
	}
	settings["sites"] = sites
	if code := call("PUT", "/api/settings", "text/plain", settings, nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON settings: %d, want 415", code)
	}
	var view map[string]any
	if code := call("PUT", "/api/settings", "application/json", settings, &view); code != 200 {
		t.Fatalf("save settings: %d %v", code, view)
	}
	if _, leaked := view["version_token"]; leaked || view["version_token_set"] != true || view["poll_seconds"] != 30.0 || len(view["sites"].([]any)) != 5 {
		t.Errorf("settings view = %v", view)
	}

	// 1. status classifies every demo site
	var snap Snapshot
	if code := call("GET", "/api/status", "", nil, &snap); code != 200 {
		t.Fatalf("status: %d", code)
	}
	want := map[string]string{"jakarta": "OK", "surabaya": "BEHIND", "medan": "SCHEMA_PENDING", "bandung": "DB_AHEAD", "makassar": "DOWN"}
	for _, s := range snap.Sites {
		if s.State != want[s.Name] {
			t.Errorf("%s: state %s, want %s", s.Name, s.State, want[s.Name])
		}
		if s.Name == "surabaya" && s.Behind != 2 {
			t.Errorf("surabaya behind %d, want 2", s.Behind)
		}
	}

	// 2. an unchanged fleet records nothing new
	if n, err := store.RecordSnapshot(ctx, &snap); err != nil || n != 0 {
		t.Fatalf("second record: n=%d err=%v, want 0 rows", n, err)
	}

	// 3. deploy guards
	if code := call("POST", "/api/deploys", "text/plain", map[string]any{"sites": []string{"medan"}, "sha": snap.Head}, nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON post: %d, want 415", code)
	}
	if code := call("POST", "/api/deploys", "application/json", map[string]any{"sites": []string{"medan"}, "sha": "stale"}, nil); code != http.StatusConflict {
		t.Errorf("stale sha: %d, want 409", code)
	}

	// 4. mass deploy; makassar is down so its pipeline fails
	targets := []string{"surabaya", "medan", "bandung", "makassar"}
	if code := call("POST", "/api/deploys", "application/json", map[string]any{"sites": targets, "sha": snap.Head}, nil); code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	var deploys []Deploy
	waitDeploys := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); ; {
			call("GET", "/api/deploys", "", nil, &deploys)
			if len(deploys) == n && !slices.ContainsFunc(deploys, func(d Deploy) bool {
				return slices.ContainsFunc(d.Runs, func(r Run) bool { return slices.Contains(activeStatuses, r.Status) })
			}) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("pipelines never finished: %+v", deploys)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitDeploys(1)
	got := map[string]string{}
	for _, r := range deploys[0].Runs {
		got[r.Site] = r.Status
	}
	if len(got) != 4 || got["surabaya"] != "success" || got["medan"] != "success" || got["bandung"] != "success" || got["makassar"] != "failed" {
		t.Errorf("run statuses = %v", got)
	}
	if deploys[0].By != "admin" || deploys[0].SHA != snap.Head || m.TriggeredBy(deploys[0].Runs[0].PipelineID) != "deploy-bot" {
		t.Errorf("deploy by=%s sha=%s", deploys[0].By, deploys[0].SHA)
	}

	// 5. the fleet converged, and the tracker kept medan's history
	call("GET", "/api/status", "", nil, &snap)
	for _, s := range snap.Sites {
		if w := map[bool]string{true: "DOWN", false: "OK"}[s.Name == "makassar"]; s.State != w {
			t.Errorf("after deploy %s: %s, want %s", s.Name, s.State, w)
		}
	}
	var hist []SiteVersion
	call("GET", "/api/sites/medan/versions", "", nil, &hist)
	if len(hist) != 2 || hist[0].State != "OK" || hist[1].State != "SCHEMA_PENDING" ||
		!slices.Equal(hist[1].Pending, []string{"M20260913120000_job_runs"}) || len(hist[0].Pending) != 0 {
		t.Errorf("medan history = %+v", hist)
	}

	// 6. dropping a site applies at once; the password change applies to the
	// next sign-in; omitting version_token keeps the stored one
	delete(settings, "version_token")
	settings["sites"], settings["password"] = sites[1:], "new-password"
	if code := call("PUT", "/api/settings", "application/json", settings, nil); code != 200 {
		t.Fatalf("second save: %d", code)
	}
	delete(settings, "password")
	call("GET", "/api/status", "", nil, &snap)
	if len(snap.Sites) != 4 || slices.ContainsFunc(snap.Sites, func(s SiteStatus) bool { return s.Name == "jakarta" }) {
		t.Errorf("after removing jakarta: %+v", snap.Sites)
	}
	if code := do(browser(), "POST", api.URL+"/auth/password", "application/json", map[string]string{"user": "admin", "password": "pw-12345"}, nil).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("old password still works: %d", code)
	}
	if cfg, _ := store.GetConfig(ctx); cfg.VersionToken != "vt" {
		t.Errorf("version token = %q, want it kept", cfg.VersionToken)
	}

	// 7. GitLab sign-in on: the password session ends and password sign-in is refused
	m.TokenTTL = 30 * time.Second // under a minute, so every request refreshes the token
	settings["oauth_client_id"], settings["oauth_secret"] = mock.ClientID, mock.ClientSecret
	if code := call("PUT", "/api/settings", "application/json", settings, &view); code != 200 {
		t.Fatalf("enable gitlab sign-in: %d", code)
	}
	if _, leaked := view["oauth_secret"]; leaked || view["oauth_secret_set"] != true || view["redirect_uri"] != api.URL+"/auth/callback" {
		t.Errorf("settings view = %v", view)
	}
	if code := call("GET", "/api/status", "", nil, &denied); code != http.StatusUnauthorized || denied["login"] != "gitlab" {
		t.Errorf("password session after enabling gitlab: %d %v", code, denied)
	}
	if code := do(browser(), "POST", api.URL+"/auth/password", "application/json", map[string]string{"user": "admin", "password": "new-password"}, nil).StatusCode; code != http.StatusForbidden {
		t.Errorf("password sign-in with gitlab on: %d, want 403", code)
	}

	// signIn walks the OAuth redirects as a browser would and returns where it lands.
	signIn := func(user string) (*http.Client, string) {
		t.Helper()
		c := browser()
		toGitlab := do(c, "GET", api.URL+"/auth/gitlab", "", nil, nil).Header.Get("Location")
		if !strings.HasPrefix(toGitlab, gl.URL+"/oauth/authorize?") {
			t.Fatalf("login redirect = %q", toGitlab)
		}
		back := do(c, "GET", toGitlab+"&mock_user="+user, "", nil, nil).Header.Get("Location")
		return c, do(c, "GET", back, "", nil, nil).Header.Get("Location")
	}
	if _, landed := signIn("luar"); !strings.Contains(landed, "login_error=") {
		t.Errorf("outsider landed on %q, want a login error", landed)
	}

	budi, landed := signIn("budi")
	if landed != "/" {
		t.Fatalf("budi landed on %q", landed)
	}
	var me struct {
		User         User `json:"user"`
		CanConfigure bool `json:"can_configure"`
	}
	for range 2 { // each call refreshes the (rotating) token; both must work
		if code := do(budi, "GET", api.URL+"/api/me", "", nil, &me).StatusCode; code != 200 || me.User.Username != "budi" || me.User.AccessLevel != 30 || me.CanConfigure {
			t.Fatalf("budi /api/me: %d %+v", code, me)
		}
	}
	if code := do(budi, "GET", api.URL+"/api/settings", "", nil, nil).StatusCode; code != http.StatusForbidden {
		t.Errorf("developer reading settings: %d, want 403", code)
	}
	if code := do(budi, "POST", api.URL+"/api/deploys", "application/json", map[string]any{"sites": []string{"medan"}, "sha": snap.Head}, nil).StatusCode; code != http.StatusCreated {
		t.Fatalf("budi deploy: %d", code)
	}
	tamu, _ := signIn("tamu")
	do(tamu, "POST", api.URL+"/api/deploys", "application/json", map[string]any{"sites": []string{"medan"}, "sha": snap.Head}, nil)
	call = func(method, path, contentType string, body, out any) int {
		return do(budi, method, api.URL+path, contentType, body, out).StatusCode
	}
	waitDeploys(3)
	if d := deploys[1]; d.By != "budi" || d.Runs[0].Status != "success" || m.TriggeredBy(d.Runs[0].PipelineID) != "budi" {
		t.Errorf("budi's deploy = %+v, triggered by %q", d, m.TriggeredBy(d.Runs[0].PipelineID))
	}
	if d := deploys[0]; d.By != "tamu" || d.Runs[0].Status != "error" || !strings.Contains(d.Runs[0].Error, "403") {
		t.Errorf("reporter's deploy should be refused by GitLab: %+v", d)
	}

	dewi, _ := signIn("dewi")
	if code := do(dewi, "GET", api.URL+"/api/settings", "", nil, nil).StatusCode; code != 200 {
		t.Errorf("maintainer reading settings: %d", code)
	}
	do(dewi, "POST", api.URL+"/auth/logout", "", nil, nil)
	if code := do(dewi, "GET", api.URL+"/api/me", "", nil, nil).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", code)
	}
}
