package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	GitLab struct {
		URL     string `json:"url"`     // https://gitlab.example.com
		Project string `json:"project"` // group/app (path or numeric id)
		Branch  string `json:"branch"`  // main
	} `json:"gitlab"`
	Sites         []Site `json:"sites"`
	PollSeconds   int    `json:"poll_seconds"`
	VersionToken  string `json:"-"` // write-only through the API
	OAuthClientID string `json:"oauth_client_id"`
	OAuthSecret   string `json:"-"` // write-only through the API
}

// gitlabLogin reports whether GitLab sign-in is set up; until then the local
// password is the way in.
func (c *Config) gitlabLogin() bool {
	return c.GitLab.URL != "" && c.OAuthClientID != "" && c.OAuthSecret != ""
}

// ErrNotConfigured means GitLab or the site list hasn't been set up yet.
var ErrNotConfigured = errors.New("not configured yet: open Settings and set GitLab and the sites")

func (c *Config) ready() bool {
	return c.GitLab.URL != "" && c.GitLab.Project != "" && c.GitLab.Branch != "" && len(c.Sites) > 0
}

// validate checks a config before it is saved.
func (c *Config) validate() error {
	c.GitLab.URL = strings.TrimSpace(c.GitLab.URL)
	c.GitLab.Project, c.GitLab.Branch = strings.TrimSpace(c.GitLab.Project), strings.TrimSpace(c.GitLab.Branch)
	c.OAuthClientID = strings.TrimSpace(c.OAuthClientID)
	if c.GitLab.URL != "" && !isHTTP(c.GitLab.URL) {
		return fmt.Errorf("gitlab url must be http(s)://...")
	}
	if c.PollSeconds < 10 {
		return fmt.Errorf("poll_seconds must be at least 10")
	}
	seen := map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		s.Name, s.VersionURL = strings.TrimSpace(s.Name), strings.TrimSpace(s.VersionURL)
		if !siteName.MatchString(s.Name) {
			return fmt.Errorf("site %q: name must be letters, digits, '-' or '_' (it is the GitLab environment)", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("site %q is listed twice", s.Name)
		}
		seen[s.Name] = true
		if !isHTTP(s.VersionURL) {
			return fmt.Errorf("site %q: version url must be http(s)://...", s.Name)
		}
	}
	return nil
}

var siteName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Site.Name is the GitLab environment name and is passed to CI as $SITE.
type Site struct {
	Name       string `json:"name"`
	VersionURL string `json:"version_url"`
}

// Schema and Version mirror the /version response, see VERSION_API.md.
type Schema struct {
	Code    string   `json:"code"`
	DB      string   `json:"db"`
	Pending []string `json:"pending"`
	Unknown []string `json:"unknown"`
}

type Version struct {
	Commit string `json:"commit"`
	Schema Schema `json:"schema"`
}

type SiteStatus struct {
	Name   string  `json:"name"`
	State  string  `json:"state"`
	Commit string  `json:"commit,omitempty"`
	Behind int     `json:"behind"` // -1 = unknown
	Schema *Schema `json:"schema,omitempty"`
	Error  string  `json:"error,omitempty"`
}

type Snapshot struct {
	Branch    string       `json:"branch"`
	Head      string       `json:"head"`
	CheckedAt time.Time    `json:"checked_at"`
	Sites     []SiteStatus `json:"sites"`
}

// Fleet reads its configuration from the store on every call, so edits made
// in the dashboard apply without a restart.
type Fleet struct {
	store *Store
	token string // GITLAB_TOKEN
}

func (f *Fleet) load(ctx context.Context) (*Config, gitlab, error) {
	cfg, err := f.store.GetConfig(ctx)
	if err != nil {
		return nil, gitlab{}, err
	}
	if !cfg.ready() {
		return nil, gitlab{}, ErrNotConfigured
	}
	return cfg, newGitlab(cfg, f.token), nil
}

var client = &http.Client{Timeout: 10 * time.Second}

// ErrStale means main moved after the caller looked at it.
var ErrStale = errors.New("main moved since the status was loaded; refresh and confirm again")

func (f *Fleet) Snapshot(ctx context.Context) (*Snapshot, error) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	head, err := gl.branchHead(cfg.GitLab.Branch)
	if err != nil {
		return nil, fmt.Errorf("read %s head: %w", cfg.GitLab.Branch, err)
	}
	out := make([]SiteStatus, len(cfg.Sites))
	var wg sync.WaitGroup
	for i, s := range cfg.Sites {
		wg.Go(func() { out[i] = check(gl, s, head, cfg.VersionToken) })
	}
	wg.Wait()
	return &Snapshot{Branch: cfg.GitLab.Branch, Head: head, CheckedAt: time.Now().UTC(), Sites: out}, nil
}

func check(gl gitlab, s Site, head, versionToken string) SiteStatus {
	st := SiteStatus{Name: s.Name, Behind: -1}
	v, err := fetchVersion(s.VersionURL, versionToken)
	if err != nil {
		st.State, st.Error = "DOWN", err.Error()
		return st
	}
	st.Commit, st.Schema, st.State = v.Commit, &v.Schema, classify(head, v)
	if v.Commit == head {
		st.Behind = 0
	} else if n, err := gl.commitsBetween(v.Commit, head); err == nil {
		st.Behind = n
	}
	return st
}

// classify returns the worst state first: an unknown applied migration means
// the DB was migrated by a newer build than the one running, which is the
// dangerous case (rolled-back code on a forward schema).
func classify(head string, v *Version) string {
	switch {
	case len(v.Schema.Unknown) > 0:
		return "DB_AHEAD"
	case len(v.Schema.Pending) > 0:
		return "SCHEMA_PENDING"
	case v.Commit != head:
		return "BEHIND"
	}
	return "OK"
}

type Run struct {
	ID         int64  `json:"id"`
	Site       string `json:"site"`
	PipelineID int64  `json:"pipeline_id,omitempty"`
	WebURL     string `json:"web_url,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

type Deploy struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Branch string    `json:"branch"`
	SHA    string    `json:"sha"`
	Runs   []Run     `json:"runs"`
}

// Deploy triggers one pipeline per site at sha, which must still be the branch
// head, so a deploy always ships what the operator confirmed. With userToken
// set, pipelines are created as that GitLab user, so GitLab's own protected
// branch/environment rules apply and the pipeline shows who deployed.
func (f *Fleet) Deploy(ctx context.Context, by string, names []string, sha, userToken string) (*Deploy, error) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	trigger := gl
	if userToken != "" {
		trigger = newGitlab(cfg, userToken)
	}
	sites, err := pick(cfg.Sites, names)
	if err != nil {
		return nil, err
	}
	branch := cfg.GitLab.Branch
	head, err := gl.branchHead(branch)
	if err != nil {
		return nil, fmt.Errorf("read %s head: %w", branch, err)
	}
	if sha != head {
		return nil, ErrStale
	}
	d := &Deploy{By: by, Branch: branch, SHA: head}
	// ponytail: sequential triggers; one POST each, fine until the fleet is in the hundreds.
	for _, s := range sites {
		r := Run{Site: s.Name}
		p, err := trigger.triggerPipeline(branch, map[string]string{"SITE": s.Name, "DEPLOY_SHA": head})
		if err != nil {
			r.Status, r.Error = "error", err.Error()
		} else {
			r.PipelineID, r.WebURL, r.Status = p.ID, p.WebURL, p.Status
		}
		d.Runs = append(d.Runs, r)
	}
	return d, nil
}

func fetchVersion(u, token string) (*Version, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var v Version
	if err := doJSON(req, &v); err != nil {
		return nil, err
	}
	if v.Commit == "" {
		return nil, fmt.Errorf("version response has no commit")
	}
	return &v, nil
}

// configFile is the `import` format: Config plus the two secrets the API
// never returns. An absent secret keeps the stored one.
type configFile struct {
	Config
	VersionToken *string `json:"version_token"`
	OAuthSecret  *string `json:"oauth_secret"`
}

func readConfigFile(path string) (*configFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := configFile{Config: Config{PollSeconds: 60}}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// pick returns the named sites, or all of them when names is empty.
func pick(all []Site, names []string) ([]Site, error) {
	if len(names) == 0 {
		return all, nil
	}
	byName := map[string]Site{}
	for _, s := range all {
		byName[s.Name] = s
	}
	var out []Site
	for _, n := range names {
		s, ok := byName[strings.TrimSpace(n)]
		if !ok {
			return nil, fmt.Errorf("unknown site %q", n)
		}
		out = append(out, s)
	}
	return out, nil
}

// SyncRuns pulls the current pipeline status for every run still in flight.
func (f *Fleet) SyncRuns(ctx context.Context) error {
	runs, err := f.store.ListActiveRuns(ctx)
	if err != nil || len(runs) == 0 {
		return err
	}
	_, gl, err := f.load(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		p, err := gl.pipeline(*r.PipelineID)
		if err != nil || p.Status == r.Status {
			continue // GitLab unreachable: keep the last known status, retry next sync
		}
		if err := f.store.UpdateRunStatus(ctx, r.ID, p.Status); err != nil {
			return err
		}
	}
	return nil
}
