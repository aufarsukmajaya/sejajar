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
	"slices"
	"strings"
	"sync"
	"time"
)

// Config is everything the dashboard's Settings edit. The fleet is a matrix:
// sites are the columns, services the rows.
type Config struct {
	GitLab struct {
		URL     string `json:"url"`     // https://gitlab.example.com
		Project string `json:"project"` // group/app (path or numeric id)
		Branch  string `json:"branch"`  // main
	} `json:"gitlab"`
	Sites    []Site    `json:"sites"`
	Services []Service `json:"services"`

	// The GitLab environment a deploy job records, so workers and jobs (which
	// expose no API) can be tracked by their last successful deployment.
	EnvironmentTemplate    string `json:"environment_template"`
	APIEnvironmentTemplate string `json:"api_environment_template"` // "" = EnvironmentTemplate

	// What each deploy pipeline is given; values are templates.
	PipelineInputs map[string]string `json:"pipeline_inputs"`
	TriggerAs      string            `json:"trigger_as"` // "inputs" (spec:inputs) or "variables"

	PollSeconds   int    `json:"poll_seconds"`
	VersionToken  string `json:"-"` // write-only through the API
	OAuthClientID string `json:"oauth_client_id"`
	OAuthSecret   string `json:"-"` // write-only through the API
}

// Site is a column. VersionURL is a template for its APIs' /version, e.g.
// https://{site}.example.com/{service}{postfix}/version; "" when they expose none.
type Site struct {
	Name       string `json:"name"`
	VersionURL string `json:"version_url"`
}

// Service is a row. A postfix is a separately deployed variant of the same
// service; Sites limits where it runs (empty = every site).
type Service struct {
	Name    string   `json:"name"`
	Postfix string   `json:"postfix"`
	Kind    string   `json:"kind"` // api, worker or job
	Sites   []string `json:"sites"`
}

func (s Service) ID() string { return s.Name + s.Postfix }

func (s Service) runsOn(site string) bool { return len(s.Sites) == 0 || slices.Contains(s.Sites, site) }

var kinds = []string{"api", "worker", "job"}

// expand fills a template's {site}, {service} and {postfix}.
func expand(tmpl, site string, svc Service) string {
	return strings.NewReplacer("{site}", site, "{service}", svc.Name, "{postfix}", svc.Postfix).Replace(tmpl)
}

func (c *Config) environment(site string, svc Service) string {
	if svc.Kind == "api" && c.APIEnvironmentTemplate != "" {
		return expand(c.APIEnvironmentTemplate, site, svc)
	}
	return expand(c.EnvironmentTemplate, site, svc)
}

// gitlabLogin reports whether GitLab sign-in is set up; until then the local
// password is the way in.
func (c *Config) gitlabLogin() bool {
	return c.GitLab.URL != "" && c.OAuthClientID != "" && c.OAuthSecret != ""
}

// ErrNotConfigured means GitLab, the sites or the services aren't set up yet.
var ErrNotConfigured = errors.New("not configured yet: open Settings and set GitLab, the sites and the services")

func (c *Config) ready() bool {
	return c.GitLab.URL != "" && c.GitLab.Project != "" && c.GitLab.Branch != "" && len(c.Sites) > 0 && len(c.Services) > 0
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	postfixRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,32}$`)
	inputRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// validate normalises and checks a config before it is saved.
func (c *Config) validate() error {
	c.GitLab.URL = strings.TrimSpace(c.GitLab.URL)
	c.GitLab.Project, c.GitLab.Branch = strings.TrimSpace(c.GitLab.Project), strings.TrimSpace(c.GitLab.Branch)
	c.OAuthClientID = strings.TrimSpace(c.OAuthClientID)
	c.EnvironmentTemplate = strings.TrimSpace(c.EnvironmentTemplate)
	c.APIEnvironmentTemplate = strings.TrimSpace(c.APIEnvironmentTemplate)
	if c.GitLab.URL != "" && !isHTTP(c.GitLab.URL) {
		return errors.New("gitlab url must be http(s)://...")
	}
	if c.PollSeconds < 10 {
		return errors.New("poll_seconds must be at least 10")
	}
	if c.EnvironmentTemplate == "" {
		c.EnvironmentTemplate = "{site}-{service}{postfix}"
	}
	if c.TriggerAs == "" {
		c.TriggerAs = "inputs"
	}
	if c.TriggerAs != "inputs" && c.TriggerAs != "variables" {
		return errors.New(`trigger_as must be "inputs" or "variables"`)
	}
	if len(c.PipelineInputs) > 30 {
		return errors.New("at most 30 pipeline inputs")
	}
	for k, v := range c.PipelineInputs {
		if !inputRe.MatchString(k) {
			return fmt.Errorf("pipeline input %q: name must be letters, digits or '_'", k)
		}
		if len(v) > 256 {
			return fmt.Errorf("pipeline input %q: value too long", k)
		}
	}

	sites := map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		s.Name, s.VersionURL = strings.TrimSpace(s.Name), strings.TrimSpace(s.VersionURL)
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("site %q: name must be letters, digits, '-' or '_'", s.Name)
		}
		if sites[s.Name] {
			return fmt.Errorf("site %q is listed twice", s.Name)
		}
		sites[s.Name] = true
		if s.VersionURL != "" && !isHTTP(expand(s.VersionURL, s.Name, Service{Name: "svc"})) {
			return fmt.Errorf("site %q: version url must be http(s)://... (placeholders {site} {service} {postfix})", s.Name)
		}
	}
	ids := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		s.Name, s.Postfix, s.Kind = strings.TrimSpace(s.Name), strings.TrimSpace(s.Postfix), strings.TrimSpace(s.Kind)
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("service %q: name must be letters, digits, '-' or '_'", s.Name)
		}
		if !postfixRe.MatchString(s.Postfix) {
			return fmt.Errorf("service %s: postfix must be letters, digits, '-', '_' or '.'", s.ID())
		}
		if !slices.Contains(kinds, s.Kind) {
			return fmt.Errorf("service %s: kind must be api, worker or job", s.ID())
		}
		// name+postfix names the deployment, so it must be unique on its own.
		if ids[s.ID()] {
			return fmt.Errorf("service %s is listed twice", s.ID())
		}
		ids[s.ID()] = true
		for _, site := range s.Sites {
			if !sites[site] {
				return fmt.Errorf("service %s: unknown site %q", s.ID(), site)
			}
		}
	}
	return nil
}

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
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

// Cell is one service on one site.
type Cell struct {
	Site        string     `json:"site"`
	Service     string     `json:"service"`
	Postfix     string     `json:"postfix,omitempty"`
	Kind        string     `json:"kind"`
	State       string     `json:"state"`
	Source      string     `json:"source"` // "version" (live /version) or "gitlab" (last deployment)
	Commit      string     `json:"commit,omitempty"`
	Behind      int        `json:"behind"` // -1 = unknown
	DeployedAt  *time.Time `json:"deployed_at,omitempty"`
	PipelineURL string     `json:"pipeline_url,omitempty"`
	Schema      *Schema    `json:"schema,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type Snapshot struct {
	Branch    string    `json:"branch"`
	Head      string    `json:"head"`
	CheckedAt time.Time `json:"checked_at"`
	Sites     []string  `json:"sites"`
	Services  []Service `json:"services"`
	Cells     []Cell    `json:"cells"`
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

// parallel caps concurrent calls to GitLab and the sites; a fleet is
// sites x services, which runs to hundreds of cells.
const parallel = 16

// eachLimited runs fn(i) for i in [0, n) with at most parallel at once.
func eachLimited(n int, fn func(i int)) {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

type target struct {
	site Site
	svc  Service
}

// cells lists every (site, service) the fleet runs, row by row.
func (c *Config) cells() []target {
	var out []target
	for _, svc := range c.Services {
		for _, site := range c.Sites {
			if svc.runsOn(site.Name) {
				out = append(out, target{site, svc})
			}
		}
	}
	return out
}

func (f *Fleet) Snapshot(ctx context.Context) (*Snapshot, error) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	head, err := gl.branchHead(cfg.GitLab.Branch)
	if err != nil {
		return nil, fmt.Errorf("read %s head: %w", cfg.GitLab.Branch, err)
	}
	refs := cfg.cells()
	out := make([]Cell, len(refs))
	behind := &behindCache{gl: gl, head: head, m: map[string]*behindEntry{}}
	eachLimited(len(refs), func(i int) { out[i] = check(gl, cfg, refs[i], head, behind) })

	snap := &Snapshot{Branch: cfg.GitLab.Branch, Head: head, CheckedAt: time.Now().UTC(), Services: cfg.Services, Cells: out}
	for _, s := range cfg.Sites {
		snap.Sites = append(snap.Sites, s.Name)
	}
	return snap, nil
}

// check reads what one cell runs: live from an API's /version, otherwise from
// the GitLab environment's last successful deployment.
func check(gl gitlab, cfg *Config, t target, head string, behind *behindCache) Cell {
	c := Cell{Site: t.site.Name, Service: t.svc.Name, Postfix: t.svc.Postfix, Kind: t.svc.Kind, Behind: -1}
	if t.svc.Kind == "api" && t.site.VersionURL != "" {
		c.Source = "version"
		v, err := fetchVersion(expand(t.site.VersionURL, t.site.Name, t.svc), cfg.VersionToken)
		if err != nil {
			c.State, c.Error = "DOWN", err.Error()
			return c
		}
		c.Commit, c.Schema, c.State = v.Commit, &v.Schema, classify(head, v)
	} else {
		c.Source = "gitlab"
		d, err := gl.lastDeployment(cfg.environment(t.site.Name, t.svc))
		if err != nil {
			c.State, c.Error = "UNKNOWN", err.Error()
			return c
		}
		if d == nil {
			c.State = "NEVER"
			return c
		}
		c.Commit, c.DeployedAt, c.PipelineURL = d.SHA, &d.UpdatedAt, d.Deployable.Pipeline.WebURL
		c.State = classify(head, &Version{Commit: d.SHA})
	}
	c.Behind = behind.of(c.Commit)
	return c
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

// behindCache asks GitLab once per distinct commit: most cells share a
// handful of commits.
type behindCache struct {
	gl   gitlab
	head string
	mu   sync.Mutex
	m    map[string]*behindEntry
}

type behindEntry struct {
	once sync.Once
	n    int
}

func (b *behindCache) of(sha string) int {
	if sha == b.head {
		return 0
	}
	b.mu.Lock()
	e := b.m[sha]
	if e == nil {
		e = &behindEntry{n: -1}
		b.m[sha] = e
	}
	b.mu.Unlock()
	e.once.Do(func() {
		if n, err := b.gl.commitsBetween(sha, b.head); err == nil {
			e.n = n
		}
	})
	return e.n
}

// Target names one cell to deploy.
type Target struct {
	Site    string `json:"site"`
	Service string `json:"service"`
	Postfix string `json:"postfix"`
}

type Run struct {
	ID          int64  `json:"id"`
	Site        string `json:"site"`
	Service     string `json:"service"`
	Postfix     string `json:"postfix,omitempty"`
	PipelineID  int64  `json:"pipeline_id,omitempty"`
	PipelineSHA string `json:"pipeline_sha,omitempty"`
	WebURL      string `json:"web_url,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type Deploy struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Branch string    `json:"branch"`
	SHA    string    `json:"sha"`
	Runs   []Run     `json:"runs"`
}

// resolve checks the targets against the config and drops duplicates.
func (c *Config) resolve(ts []Target) ([]target, error) {
	sites := map[string]Site{}
	for _, s := range c.Sites {
		sites[s.Name] = s
	}
	seen := map[Target]bool{}
	var out []target
	for _, t := range ts {
		site, ok := sites[t.Site]
		if !ok {
			return nil, fmt.Errorf("unknown site %q", t.Site)
		}
		i := slices.IndexFunc(c.Services, func(s Service) bool { return s.Name == t.Service && s.Postfix == t.Postfix })
		if i < 0 {
			return nil, fmt.Errorf("unknown service %q", t.Service+t.Postfix)
		}
		if !c.Services[i].runsOn(t.Site) {
			return nil, fmt.Errorf("%s does not run on %s", t.Service+t.Postfix, t.Site)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, target{site, c.Services[i]})
		}
	}
	return out, nil
}

// Deploy triggers one pipeline per cell on the branch, which must still be at
// sha, so a deploy ships what the operator confirmed. With userToken set,
// pipelines are created as that GitLab user, so GitLab's own protected
// branch/environment rules apply and the pipeline shows who deployed.
func (f *Fleet) Deploy(ctx context.Context, by string, targets []Target, sha, userToken string) (*Deploy, error) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	cells, err := cfg.resolve(targets)
	if err != nil {
		return nil, err
	}
	if len(cells) == 0 {
		return nil, errors.New("nothing selected")
	}
	trigger := gl
	if userToken != "" {
		trigger = newGitlab(cfg, userToken)
	}
	branch := cfg.GitLab.Branch
	head, err := gl.branchHead(branch)
	if err != nil {
		return nil, fmt.Errorf("read %s head: %w", branch, err)
	}
	if sha != head {
		return nil, ErrStale
	}
	d := &Deploy{By: by, Branch: branch, SHA: head, Runs: make([]Run, len(cells))}
	eachLimited(len(cells), func(i int) {
		t := cells[i]
		r := Run{Site: t.site.Name, Service: t.svc.Name, Postfix: t.svc.Postfix}
		inputs := map[string]string{}
		for k, v := range cfg.PipelineInputs {
			inputs[k] = expand(v, t.site.Name, t.svc)
		}
		p, err := trigger.triggerPipeline(branch, inputs, cfg.TriggerAs)
		switch {
		case err != nil:
			r.Status, r.Error = "error", err.Error()
		default:
			r.PipelineID, r.PipelineSHA, r.WebURL, r.Status = p.ID, p.SHA, p.WebURL, p.Status
			// A pipeline runs the branch, not a SHA; say so if main moved
			// between the check above and this trigger.
			if p.SHA != "" && p.SHA != head {
				r.Error = "main moved while triggering; this pipeline builds " + short(p.SHA)
			}
		}
		d.Runs[i] = r
	})
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
		return nil, errors.New("version response has no commit")
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

// targetsFor lists the cells matching the site and service filters (empty =
// all). Jobs are one-off, so they're only included when named.
func (c *Config) targetsFor(sites, services []string) ([]Target, error) {
	for _, s := range sites {
		if !slices.ContainsFunc(c.Sites, func(x Site) bool { return x.Name == s }) {
			return nil, fmt.Errorf("unknown site %q", s)
		}
	}
	for _, s := range services {
		if !slices.ContainsFunc(c.Services, func(x Service) bool { return x.ID() == s }) {
			return nil, fmt.Errorf("unknown service %q", s)
		}
	}
	var out []Target
	for _, t := range c.cells() {
		if len(sites) > 0 && !slices.Contains(sites, t.site.Name) {
			continue
		}
		if len(services) > 0 && !slices.Contains(services, t.svc.ID()) || len(services) == 0 && t.svc.Kind == "job" {
			continue
		}
		out = append(out, Target{t.site.Name, t.svc.Name, t.svc.Postfix})
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
	errs := make([]error, len(runs))
	eachLimited(len(runs), func(i int) {
		p, err := gl.pipeline(*runs[i].PipelineID)
		if err != nil || p.Status == runs[i].Status {
			return // GitLab unreachable: keep the last known status, retry next sync
		}
		errs[i] = f.store.UpdateRunStatus(ctx, runs[i].ID, p.Status)
	})
	return errors.Join(errs...)
}
