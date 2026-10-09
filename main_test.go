package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aufarsukmajaya/resonate/internal/mock"
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

func testConfig() *Config {
	return &Config{PollSeconds: 60, Sites: []Site{{Name: "a"}, {Name: "b"}}, Services: []Service{
		{Name: "api", Kind: "api"},
		{Name: "api", Postfix: "-x", Kind: "api", Sites: []string{"b"}},
		{Name: "worker", Kind: "worker"},
		{Name: "job", Kind: "job"},
	}}
}

func TestTargetsFor(t *testing.T) {
	c := testConfig()
	ids := func(ts []Target) []string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Site+"/"+t.Service+t.Postfix)
		}
		return out
	}
	// No filter: every cell except jobs; the postfix variant only on its site.
	got, _ := c.targetsFor(nil, nil)
	if want := []string{"a/api", "b/api", "b/api-x", "a/worker", "b/worker"}; !slices.Equal(ids(got), want) {
		t.Errorf("all = %v, want %v", ids(got), want)
	}
	// Jobs only when named.
	got, _ = c.targetsFor([]string{"a"}, []string{"job", "api-x"})
	if want := []string{"a/job"}; !slices.Equal(ids(got), want) {
		t.Errorf("a + job,api-x = %v, want %v", ids(got), want)
	}
	if _, err := c.targetsFor([]string{"zzz"}, nil); err == nil {
		t.Error("unknown site must error")
	}
	if _, err := c.targetsFor(nil, []string{"zzz"}); err == nil {
		t.Error("unknown service must error")
	}
}

func TestResolve(t *testing.T) {
	c := testConfig()
	got, err := c.resolve([]Target{{"b", "api", "-x"}, {"b", "api", "-x"}, {"a", "worker", ""}})
	if err != nil || len(got) != 2 {
		t.Fatalf("resolve = %v, %v; want 2 deduped targets", got, err)
	}
	if _, err := c.resolve([]Target{{"a", "api", "-x"}}); err == nil {
		t.Error("a postfix variant must not deploy to a site it doesn't run on")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(c *Config){
		"colliding deployment names": func(c *Config) {
			c.Services = append(c.Services, Service{Name: "api-x", Kind: "api"}) // api + -x is also api-x
		},
		"unknown site on a service": func(c *Config) { c.Services[1].Sites = []string{"zzz"} },
		"bad kind":                  func(c *Config) { c.Services[0].Kind = "cron" },
		"bad input name":            func(c *Config) { c.PipelineInputs = map[string]string{"BAD-NAME": "x"} },
		"version url not http":      func(c *Config) { c.Sites[0].VersionURL = "{site}.example.com/version" },
		"bad trigger":               func(c *Config) { c.TriggerAs = "webhook" },
		"inputs can't tell variants apart": func(c *Config) {
			c.PipelineInputs = map[string]string{"SITE": "{site}", "SERVICE": "{service}"} // api and api-x would deploy the same
		},
		"inputs set but empty":            func(c *Config) { c.PipelineInputs = map[string]string{} },
		"environment without {postfix}":   func(c *Config) { c.EnvironmentTemplate = "{site}-{service}" },
		"API environment without {site}":  func(c *Config) { c.APIEnvironmentTemplate = "{service}{postfix}-stable" },
		"version url shared by every API": func(c *Config) { c.Sites[0].VersionURL = "https://a.example.com/version" },
		"version url without {postfix}":   func(c *Config) { c.Sites[1].VersionURL = "https://b.example.com/{service}/version" },
		"agent without a pod namespace":   func(c *Config) { c.Kubernetes.Namespace = "" },
		"pods shared by every service":    func(c *Config) { c.Kubernetes.Selector = "app=web" },
		"shared agent, sites not apart":   func(c *Config) { c.Sites[1].AgentID = 1; c.Kubernetes.Namespace = "prod" },
		"negative agent id":               func(c *Config) { c.Sites[1].AgentID = -2 },
		"proxy url not http":              func(c *Config) { c.Kubernetes.ProxyURL = "kas.example.com" },
	}
	pods := func(c *Config) *Config {
		c.Sites[0].AgentID, c.Sites[1].AgentID, c.Kubernetes.Namespace = 1, 2, "prod"
		return c
	}
	c := pods(testConfig())
	c.Sites[0].VersionURL = "https://a.example.com/{service}/version" // no postfixed API on a
	if err := c.validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if !maps.Equal(c.PipelineInputs, defaultInputs()) {
		t.Errorf("no pipeline inputs should mean the defaults, got %v", c.PipelineInputs)
	}
	if c.Kubernetes.Selector != "app={service}{postfix}" {
		t.Errorf("no selector should mean the default, got %q", c.Kubernetes.Selector)
	}
	for name, breakIt := range cases {
		c := pods(testConfig())
		breakIt(c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestK8sProxy(t *testing.T) {
	c := &Config{}
	for url, want := range map[string]string{
		"https://gitlab.com":         "https://kas.gitlab.com/k8s-proxy",
		"https://git.example.com/":   "https://git.example.com/-/kubernetes-agent/k8s-proxy",
		"https://example.com/gitlab": "https://example.com/gitlab/-/kubernetes-agent/k8s-proxy",
	} {
		c.GitLab.URL = url
		if got := c.k8sProxy(); got != want {
			t.Errorf("k8sProxy(%s) = %s, want %s", url, got, want)
		}
	}
	c.Kubernetes.ProxyURL = "https://kas.example.com/k8s-proxy/"
	if got := c.k8sProxy(); got != "https://kas.example.com/k8s-proxy" {
		t.Errorf("explicit proxy = %s", got)
	}
}

// TestSchemaUpgrade applies schema.sql over the first release's schema: the
// tables exist already, so every new column must come from an ALTER.
func TestSchemaUpgrade(t *testing.T) {
	dsn := os.Getenv("RESONATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESONATE_TEST_DATABASE_URL not set")
	}
	v1, err := os.ReadFile("testdata/schema_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx) // search_path is per connection
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{"DROP SCHEMA IF EXISTS upgrade_test CASCADE", "CREATE SCHEMA upgrade_test", "SET search_path TO upgrade_test", string(v1)} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%.40s: %v", q, err)
		}
	}
	defer db.ExecContext(ctx, "DROP SCHEMA IF EXISTS upgrade_test CASCADE")
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("upgrade from v1: %v", err)
	}
	var inputs string
	if err := conn.QueryRowContext(ctx, "SELECT pipeline_inputs::text FROM settings").Scan(&inputs); err != nil || !strings.Contains(inputs, "{postfix}") {
		t.Errorf("settings after upgrade: %q, %v", inputs, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO deploys (created_by, branch, sha) VALUES ('t', 'main', 'x'); "+
		"INSERT INTO deploy_runs (deploy_id, site, service, postfix, pipeline_sha, status) VALUES (1, 's', 'api', '', 'x', 'running'); "+
		"INSERT INTO site_versions (site, service, postfix, state, source) VALUES ('s', 'api', '', 'OK', 'version')"); err != nil {
		t.Errorf("writing the new columns after upgrade: %v", err)
	}
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		t.Errorf("schema.sql is not idempotent: %v", err)
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
// Postgres. Run with RESONATE_TEST_DATABASE_URL pointing at a scratch database;
// the test truncates its tables.
func TestEndToEnd(t *testing.T) {
	dsn := os.Getenv("RESONATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESONATE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := openStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, "TRUNCATE deploys, deploy_runs, site_versions, sites, services, sessions RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveConfig(ctx, &Config{PollSeconds: 60, EnvironmentTemplate: "{site}-{service}{postfix}", TriggerAs: "inputs"}, new(""), new(""), "test"); err != nil {
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
	as := browser() // whose session call() uses
	call := func(method, path, contentType string, body, out any) int {
		t.Helper()
		return do(as, method, api.URL+path, contentType, body, out).StatusCode
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
	var unset map[string]string
	if code := call("GET", "/api/status", "", nil, &unset); code != http.StatusServiceUnavailable || !strings.Contains(unset["error"], "the GitLab URL, the GitLab project") {
		t.Fatalf("unconfigured status: %d %v, want 503 naming what's missing", code, unset)
	}
	var sites []map[string]any
	for _, n := range mock.SiteNames {
		sites = append(sites, map[string]any{"name": n, "version_url": gl.URL + "/sites/{site}/{service}{postfix}/version", "agent_id": mock.AgentID(n)})
	}
	var services []map[string]any
	for _, s := range mock.Services {
		services = append(services, map[string]any{"name": s.Name, "postfix": s.Postfix, "kind": s.Kind, "sites": s.Sites})
	}
	settings := map[string]any{"gitlab": map[string]string{"url": gl.URL, "project": "acme/app", "branch": "main"},
		"poll_seconds": 30, "version_token": "vt", "services": services,
		"environment_template": "{site}-{service}{postfix}", "trigger_as": "inputs",
		"kubernetes":      map[string]string{"namespace": "{site}", "selector": "app={service}{postfix}"},
		"pipeline_inputs": map[string]string{"SITE": "{site}", "SERVICE": "{service}", "POSTFIX": "{postfix}", "DEPLOY_SHA": "{sha}"}}
	settings["sites"] = append(slices.Clone(sites), map[string]any{"name": "bad name"})
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
	if _, leaked := view["version_token"]; leaked || view["version_token_set"] != true || view["poll_seconds"] != 30.0 ||
		len(view["sites"].([]any)) != 5 || len(view["services"].([]any)) != 6 {
		t.Errorf("settings view = %v", view)
	}

	// 1. status: one cell per (site, service), APIs from /version, the rest from GitLab deployments
	var snap Snapshot
	if code := call("GET", "/api/status", "", nil, &snap); code != 200 {
		t.Fatalf("status: %d", code)
	}
	cells := func() map[string]Cell {
		out := map[string]Cell{}
		for _, c := range snap.Cells {
			out[c.Site+"/"+c.Service+c.Postfix] = c
		}
		return out
	}
	want := map[string]string{}
	for _, s := range mock.SiteNames {
		for _, svc := range []string{"api-core", "api-report", "worker-mailer", "worker-sync", "job-backfill"} {
			want[s+"/"+svc] = "OK"
		}
	}
	maps.Copy(want, map[string]string{
		"jakarta/api-core-partner": "OK",
		"surabaya/api-core":        "BEHIND", "surabaya/api-core-partner": "BEHIND", "surabaya/api-report": "BEHIND",
		"surabaya/worker-mailer": "BEHIND", "surabaya/worker-sync": "BEHIND", "surabaya/job-backfill": "BEHIND",
		"medan/api-core": "SCHEMA_PENDING", "medan/api-report": "SCHEMA_PENDING", "medan/job-backfill": "NEVER",
		"bandung/api-core": "DB_AHEAD", "bandung/api-report": "DB_AHEAD", "bandung/worker-sync": "BEHIND",
		"makassar/api-core": "DOWN", "makassar/api-report": "DOWN", "makassar/job-backfill": "NEVER",
		"makassar/worker-mailer": "DOWN", // deployed, but its pods never turn ready
		"makassar/worker-sync":   "DOWN", // its GitLab environment was stopped
	})
	got := cells()
	if len(got) != len(want) {
		t.Errorf("%d cells, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k].State != w {
			t.Errorf("%s: state %s, want %s (%s)", k, got[k].State, w, got[k].Error)
		}
	}
	if c := got["surabaya/worker-sync"]; c.Behind != 2 || c.Source != "gitlab" || c.DeployedAt == nil {
		t.Errorf("surabaya/worker-sync = %+v, want 2 behind from a gitlab deployment", c)
	}
	if c := got["medan/api-core"]; c.Source != "version" || c.Schema == nil || !slices.Equal(c.Schema.Pending, []string{"M20260913120000_job_runs"}) {
		t.Errorf("medan/api-core = %+v", c)
	}
	// workers: pods through the agent, stopped environments, failed deploys
	if c := got["jakarta/worker-mailer"]; c.Pods == nil || *c.Pods != (Pods{Ready: 2, Total: 2}) {
		t.Errorf("jakarta/worker-mailer pods = %+v, want 2 of 2 ready", c.Pods)
	}
	if c := got["makassar/worker-mailer"]; c.Pods == nil || *c.Pods != (Pods{Ready: 0, Total: 2}) || c.Error != "0 of 2 pods ready" {
		t.Errorf("makassar/worker-mailer = %+v, pods %+v", c, c.Pods)
	}
	if c := got["makassar/worker-sync"]; !strings.Contains(c.Error, "stopped") {
		t.Errorf("makassar/worker-sync = %+v, want a stopped environment", c)
	}
	if c := got["surabaya/worker-mailer"]; c.FailedDeploy == nil || c.FailedDeploy.Status != "failed" || c.FailedDeploy.SHA != snap.Head || c.Behind != 2 {
		t.Errorf("surabaya/worker-mailer = %+v, want 2 behind with the head's deploy failed", c)
	}
	if c := got["jakarta/job-backfill"]; c.Pods != nil {
		t.Errorf("a job's pods were checked: %+v", c.Pods)
	}

	// 2. an unchanged fleet records nothing new, and a cell GitLab didn't
	// answer for (UNKNOWN) is not a change
	if n, err := store.RecordSnapshot(ctx, &snap); err != nil || n != 0 {
		t.Fatalf("second record: n=%d err=%v, want 0 rows", n, err)
	}
	blip := Snapshot{Cells: []Cell{{Site: "medan", Service: "worker-sync", State: "UNKNOWN", Source: "gitlab", Error: "502"}}}
	if n, err := store.RecordSnapshot(ctx, &blip); err != nil || n != 0 {
		t.Fatalf("UNKNOWN cell recorded: n=%d err=%v", n, err)
	}

	// 3. deploy guards
	target := func(site, svc, postfix string) map[string]string {
		return map[string]string{"site": site, "service": svc, "postfix": postfix}
	}
	deployBody := func(sha string, ts ...map[string]string) map[string]any {
		return map[string]any{"targets": ts, "sha": sha}
	}
	if code := call("POST", "/api/deploys", "text/plain", deployBody(snap.Head, target("medan", "api-core", "")), nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON post: %d, want 415", code)
	}
	if code := call("POST", "/api/deploys", "application/json", deployBody("stale", target("medan", "api-core", "")), nil); code != http.StatusConflict {
		t.Errorf("stale sha: %d, want 409", code)
	}
	if code := call("POST", "/api/deploys", "application/json", deployBody(snap.Head, target("medan", "api-core", "-partner")), nil); code != http.StatusBadRequest {
		t.Errorf("variant on a site it doesn't run on: %d, want 400", code)
	}

	// 4. mass deploy across sites and kinds; makassar's DB is down so its API
	// deploy fails, while its worker deploys fine
	targets := []map[string]string{
		target("surabaya", "api-core", ""), target("surabaya", "api-core", "-partner"), target("surabaya", "api-report", ""),
		target("surabaya", "worker-mailer", ""), target("surabaya", "worker-sync", ""),
		target("medan", "api-core", ""), target("medan", "api-report", ""), target("medan", "job-backfill", ""),
		target("bandung", "api-core", ""), target("bandung", "api-report", ""), target("bandung", "worker-sync", ""),
		target("makassar", "api-core", ""), target("makassar", "worker-sync", ""),
	}
	if code := call("POST", "/api/deploys", "application/json", deployBody(snap.Head, targets...), nil); code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	var deploys []Deploy
	waitDeploys := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; {
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
	runs := map[string]Run{}
	for _, r := range deploys[0].Runs {
		runs[r.Site+"/"+r.Service+r.Postfix] = r
	}
	for _, tg := range targets {
		k := tg["site"] + "/" + tg["service"] + tg["postfix"]
		w := map[bool]string{true: "failed", false: "success"}[k == "makassar/api-core"]
		if runs[k].Status != w || runs[k].ID == 0 || runs[k].PipelineSHA != snap.Head {
			t.Errorf("run %s = %+v, want %s at the head", k, runs[k], w)
		}
	}
	if len(runs) != len(targets) || deploys[0].By != "admin" || deploys[0].SHA != snap.Head {
		t.Errorf("deploy = %+v", deploys[0])
	}
	partner := runs["surabaya/api-core-partner"].PipelineID
	if in := m.Inputs(partner); !maps.Equal(in, map[string]string{"SITE": "surabaya", "SERVICE": "api-core", "POSTFIX": "-partner", "DEPLOY_SHA": snap.Head}) {
		t.Errorf("pipeline inputs = %v", in)
	}
	if m.TriggeredBy(partner) != "deploy-bot" {
		t.Errorf("password admin's deploy should run as the bot token, ran as %q", m.TriggeredBy(partner))
	}

	// 5. the deployed cells converged, and the tracker kept their history
	snap = Snapshot{} // decoding into the old cells would keep fields the new JSON omits
	call("GET", "/api/status?fresh", "", nil, &snap)
	got = cells()
	for _, tg := range targets {
		k := tg["site"] + "/" + tg["service"] + tg["postfix"]
		if w := map[bool]string{true: "DOWN", false: "OK"}[k == "makassar/api-core"]; got[k].State != w {
			t.Errorf("after deploy %s: %s, want %s", k, got[k].State, w)
		}
	}
	if got["makassar/worker-mailer"].State != "DOWN" {
		t.Errorf("an untouched cell changed: %+v", got["makassar/worker-mailer"])
	}
	if c := got["surabaya/worker-mailer"]; c.FailedDeploy != nil {
		t.Errorf("a successful deploy should clear the failed one: %+v", c.FailedDeploy)
	}
	history := func(site, svc, postfix string) []SiteVersion {
		var h []SiteVersion
		call("GET", "/api/versions?site="+site+"&service="+svc+"&postfix="+postfix, "", nil, &h)
		return h
	}
	if h := history("medan", "api-core", ""); len(h) != 2 || h[0].State != "OK" || h[1].State != "SCHEMA_PENDING" ||
		!slices.Equal(h[1].Pending, []string{"M20260913120000_job_runs"}) || len(h[0].Pending) != 0 {
		t.Errorf("medan api-core history = %+v", h)
	}
	if h := history("medan", "job-backfill", ""); len(h) != 2 || h[0].State != "OK" || h[0].Source != "gitlab" || h[1].State != "NEVER" {
		t.Errorf("medan job-backfill history = %+v", h)
	}

	// 6. a site can't be dropped while a service still names it; drop both and
	// it applies at once. The password change applies to the next sign-in and
	// omitting version_token keeps the stored one.
	delete(settings, "version_token")
	without := slices.DeleteFunc(slices.Clone(sites), func(s map[string]any) bool { return s["name"] == "jakarta" })
	settings["sites"] = without // api-core-partner still lists jakarta
	if code := call("PUT", "/api/settings", "application/json", settings, nil); code != http.StatusBadRequest {
		t.Errorf("dropping a site a service names: %d, want 400", code)
	}
	services[1]["sites"] = []string{"surabaya"}
	settings["services"], settings["password"] = services, "new-password"
	if code := call("PUT", "/api/settings", "application/json", settings, nil); code != 200 {
		t.Fatalf("second save: %d", code)
	}
	delete(settings, "password")
	snap = Snapshot{}
	call("GET", "/api/status", "", nil, &snap)
	if len(snap.Sites) != 4 || slices.Contains(snap.Sites, "jakarta") || len(snap.Cells) != 4*5+1 {
		t.Errorf("after dropping jakarta: %d sites, %d cells", len(snap.Sites), len(snap.Cells))
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
	as = budi
	var me struct {
		User         User `json:"user"`
		CanConfigure bool `json:"can_configure"`
	}
	for range 2 { // each call refreshes the (rotating) token; both must work
		if code := call("GET", "/api/me", "", nil, &me); code != 200 || me.User.Username != "budi" || me.User.AccessLevel != 30 || me.CanConfigure {
			t.Fatalf("budi /api/me: %d %+v", code, me)
		}
	}
	if code := call("GET", "/api/settings", "", nil, nil); code != http.StatusForbidden {
		t.Errorf("developer reading settings: %d, want 403", code)
	}
	if code := call("POST", "/api/deploys", "application/json", deployBody(snap.Head, target("makassar", "worker-mailer", "")), nil); code != http.StatusCreated {
		t.Fatalf("budi deploy: %d", code)
	}
	tamu, _ := signIn("tamu")
	do(tamu, "POST", api.URL+"/api/deploys", "application/json", deployBody(snap.Head, target("medan", "worker-sync", "")), nil)
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
