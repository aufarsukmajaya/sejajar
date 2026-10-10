package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
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

	// Pods are read through the GitLab agent for Kubernetes, for cells read
	// from GitLab deployments (workers, and APIs without a /version), on
	// sites that name their agent.
	Kubernetes struct {
		ProxyURL  string `json:"proxy_url"` // "" = derived from the GitLab URL
		Namespace string `json:"namespace"` // template
		Selector  string `json:"selector"`  // label selector template
	} `json:"kubernetes"`

	PollSeconds   int    `json:"poll_seconds"`
	VersionToken  string `json:"-"` // write-only through the API
	OAuthClientID string `json:"oauth_client_id"`
	OAuthSecret   string `json:"-"` // write-only through the API
}

// Site is a column. VersionURL is a template for its APIs' /version, e.g.
// https://{site}.example.com/{service}{postfix}/version; "" when they expose none.
// AgentID is the GitLab agent for Kubernetes in its cluster; 0 = no pod checks.
type Site struct {
	Name       string `json:"name"`
	VersionURL string `json:"version_url"`
	AgentID    int64  `json:"agent_id,omitempty"`
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

// defaultInputs is what a deploy pipeline gets when Settings name none.
func defaultInputs() map[string]string {
	return map[string]string{"SITE": "{site}", "SERVICE": "{service}", "POSTFIX": "{postfix}", "DEPLOY_SHA": "{sha}"}
}

// expand fills a template's {site}, {service} and {postfix}. Deploy also
// fills {sha} in pipeline inputs.
func expand(tmpl, site string, svc Service) string {
	return strings.NewReplacer("{site}", site, "{service}", svc.Name, "{postfix}", svc.Postfix).Replace(tmpl)
}

// k8sProxy is the agent's Kubernetes API proxy: kas.gitlab.com on GitLab.com,
// a path on the instance when self-managed, unless Settings name another.
func (c *Config) k8sProxy() string {
	if c.Kubernetes.ProxyURL != "" {
		return strings.TrimRight(c.Kubernetes.ProxyURL, "/")
	}
	if u, err := url.Parse(c.GitLab.URL); err == nil && u.Host == "gitlab.com" {
		return "https://kas.gitlab.com/k8s-proxy"
	}
	return strings.TrimRight(c.GitLab.URL, "/") + "/-/kubernetes-agent/k8s-proxy"
}

// pinsSHA reports whether deploy pipelines are told which commit to roll out
// ({sha}), so the image can differ from the commit the pipeline ran on.
func (c *Config) pinsSHA() bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(c.PipelineInputs)), func(v string) bool { return strings.Contains(v, "{sha}") })
}

// environmentSearch is the literal start every environment name shares, so
// listing stopped environments can skip the rest (review apps and such).
func (c *Config) environmentSearch() string {
	p := c.EnvironmentTemplate
	if c.APIEnvironmentTemplate != "" {
		p = commonPrefix(p, c.APIEnvironmentTemplate)
	}
	if i := strings.IndexByte(p, '{'); i >= 0 {
		p = p[:i]
	}
	return p
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
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
var ErrNotConfigured = errors.New("not configured yet")

// missing names what Settings still needs before anything can be checked.
func (c *Config) missing() []string {
	var m []string
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"the GitLab URL", c.GitLab.URL != ""},
		{"the GitLab project", c.GitLab.Project != ""},
		{"the branch", c.GitLab.Branch != ""},
		{"a site", len(c.Sites) > 0},
		{"a service", len(c.Services) > 0},
	} {
		if !f.ok {
			m = append(m, f.name)
		}
	}
	return m
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
	k := &c.Kubernetes
	k.ProxyURL, k.Namespace, k.Selector = strings.TrimSpace(k.ProxyURL), strings.TrimSpace(k.Namespace), strings.TrimSpace(k.Selector)
	if k.Selector == "" {
		k.Selector = "app={service}{postfix}"
	}
	if k.ProxyURL != "" && !isHTTP(k.ProxyURL) {
		return errors.New("kubernetes proxy url must be http(s)://...")
	}
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
	if c.PipelineInputs == nil {
		c.PipelineInputs = defaultInputs()
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
		if s.AgentID < 0 {
			return fmt.Errorf("site %q: agent id must be a GitLab agent's id, or 0 for none", s.Name)
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
	return c.validateTemplates()
}

// validateTemplates checks that the templates tell cells apart; otherwise two
// cells would share a pipeline, a GitLab environment or a /version, and one
// would silently stand in for the other.
func (c *Config) validateTemplates() error {
	postfix := func(svcs []Service) bool {
		return slices.ContainsFunc(svcs, func(s Service) bool { return s.Postfix != "" })
	}
	need := []string{"{site}", "{service}"}
	if postfix(c.Services) {
		need = append(need, "{postfix}")
	}
	values := strings.Join(slices.Collect(maps.Values(c.PipelineInputs)), " ")
	for _, p := range need {
		if !strings.Contains(values, p) {
			return fmt.Errorf("pipeline inputs must pass %s, or deploys can't tell those cells apart", p)
		}
		if !strings.Contains(c.EnvironmentTemplate, p) {
			return fmt.Errorf("environment template must contain %s", p)
		}
		if c.APIEnvironmentTemplate != "" && !strings.Contains(c.APIEnvironmentTemplate, p) {
			return fmt.Errorf("API environment template must contain %s", p)
		}
	}
	agents := map[int64]int{} // agent id -> sites using it
	for _, site := range c.Sites {
		if site.AgentID > 0 {
			agents[site.AgentID]++
		}
	}
	if len(agents) > 0 {
		pods := c.Kubernetes.Namespace + " " + c.Kubernetes.Selector
		if c.Kubernetes.Namespace == "" {
			return errors.New("sites name a GitLab agent: set the pod namespace")
		}
		for _, p := range need[1:] { // {service}, and {postfix} when variants exist
			if !strings.Contains(pods, p) {
				return fmt.Errorf("pod namespace or selector must contain %s, or services would share their pods", p)
			}
		}
		if slices.ContainsFunc(slices.Collect(maps.Values(agents)), func(n int) bool { return n > 1 }) && !strings.Contains(pods, "{site}") {
			return errors.New("sites share a GitLab agent: the pod namespace or selector must contain {site}")
		}
	}
	for _, site := range c.Sites {
		if site.VersionURL == "" {
			continue
		}
		apis := slices.DeleteFunc(slices.Clone(c.Services), func(s Service) bool { return s.Kind != "api" || !s.runsOn(site.Name) })
		if len(apis) > 0 && !strings.Contains(site.VersionURL, "{service}") {
			return fmt.Errorf("site %s: version url must contain {service}: each API answers its own /version", site.Name)
		}
		if postfix(apis) && !strings.Contains(site.VersionURL, "{postfix}") {
			return fmt.Errorf("site %s: version url must contain {postfix}: a postfixed API is its own deployment", site.Name)
		}
	}
	return c.distinctNames()
}

// distinctNames checks the expanded names, not only their placeholders: names
// may contain '-', so site "eu-west" + service "api" and site "eu" + service
// "west-api" both expand "{site}-{service}" to "eu-west-api".
func (c *Config) distinctNames() error {
	seen := map[string]string{} // kind + name -> the cell using it
	use := func(kind, name, cell string) error {
		if other, ok := seen[kind+"\x00"+name]; ok {
			return fmt.Errorf("%s and %s would share the %s %q", other, cell, kind, name)
		}
		seen[kind+"\x00"+name] = cell
		return nil
	}
	for _, t := range c.cells() {
		site, svc, cell := t.site.Name, t.svc, t.site.Name+" "+t.svc.ID()
		var inputs []string
		for k, v := range c.PipelineInputs {
			inputs = append(inputs, k+"="+expand(v, site, svc))
		}
		slices.Sort(inputs)
		errs := []error{use("GitLab environment", c.environment(site, svc), cell), use("pipeline inputs", strings.Join(inputs, " "), cell)}
		if t.site.AgentID > 0 && svc.Kind != "job" {
			pods := fmt.Sprintf("agent %d: %s -l %s", t.site.AgentID, expand(c.Kubernetes.Namespace, site, svc), expand(c.Kubernetes.Selector, site, svc))
			errs = append(errs, use("pods", pods, cell))
		}
		if svc.Kind == "api" && t.site.VersionURL != "" {
			errs = append(errs, use("version url", expand(t.site.VersionURL, site, svc), cell))
		}
		if err := errors.Join(errs...); err != nil {
			return err
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
	Pods        *Pods      `json:"pods,omitempty"` // nil when not checked
	// FailedDeploy is the environment's latest deployment when it failed or was
	// canceled: the cell still runs the one before it.
	FailedDeploy *FailedDeploy `json:"failed_deploy,omitempty"`
	Error        string        `json:"error,omitempty"`

	pipelineID int64 // the deployment's pipeline, for cells read from GitLab
}

type FailedDeploy struct {
	Status      string    `json:"status"`
	SHA         string    `json:"sha"`
	At          time.Time `json:"at"`
	PipelineURL string    `json:"pipeline_url,omitempty"`
}

type Snapshot struct {
	Branch    string    `json:"branch"`
	Head      string    `json:"head"`
	CheckedAt time.Time `json:"checked_at"`
	Sites     []string  `json:"sites"`
	Services  []Service `json:"services"`
	Cells     []Cell    `json:"cells"`

	PollSeconds int `json:"poll_seconds"` // how often an open dashboard re-checks
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
	if m := cfg.missing(); len(m) > 0 {
		return nil, gitlab{}, fmt.Errorf("%w: open Settings and set %s", ErrNotConfigured, strings.Join(m, ", "))
	}
	return cfg, newGitlab(cfg, f.token), nil
}

var client = &http.Client{Timeout: 10 * time.Second}

// ErrStale means main moved after the caller looked at it.
var ErrStale = errors.New("main moved since the status was loaded; refresh and confirm again")

// parallel caps concurrent calls to GitLab and the sites; a fleet is
// sites x services, which runs to hundreds of cells.
const parallel = 32

// versionTimeout bounds one /version call, so an unreachable site costs
// seconds, not the client's full timeout, per API.
const versionTimeout = 5 * time.Second

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

// snapshot also returns the config it was taken with.
func (f *Fleet) snapshot(ctx context.Context) (*Snapshot, *Config, error) {
	cfg, gl, err := f.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	head, err := gl.branchHead(ctx, cfg.GitLab.Branch)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s head: %w", cfg.GitLab.Branch, err)
	}
	stopped, err := gl.stoppedEnvironments(ctx, cfg.environmentSearch())
	if err != nil {
		log.Printf("stopped environments: %v", err) // judge cells by their deployments and pods alone
	}
	refs := cfg.cells()
	out := make([]Cell, len(refs))
	behind := &behindCache{ctx: ctx, gl: gl, head: head, m: map[string]*behindEntry{}}
	eachLimited(len(refs), func(i int) {
		c := check(ctx, gl, cfg, head, stopped, refs[i])
		if c.Commit != "" {
			c.Behind = behind.of(c.Commit)
		}
		out[i] = c
	})

	var pipelines []int64
	for _, c := range out {
		if c.pipelineID != 0 {
			pipelines = append(pipelines, c.pipelineID)
		}
	}
	pinned, err := f.store.PinnedSHAs(ctx, pipelines)
	if err != nil {
		log.Printf("pinned deploys: %v", err) // show what GitLab recorded
	}
	// Only pipelines main moved under are pinned, so these are few.
	for i := range out {
		c := &out[i]
		sha, ok := pinned[c.pipelineID]
		if !ok {
			continue
		}
		c.Commit = sha // GitLab records the branch commit; the job rolled out the pinned image
		if c.State != "DOWN" {
			c.State = classify(head, &Version{Commit: sha})
		}
		c.Behind = behind.of(sha)
	}

	snap := &Snapshot{Branch: cfg.GitLab.Branch, Head: head, CheckedAt: time.Now().UTC(), Services: cfg.Services, Cells: out,
		PollSeconds: cfg.PollSeconds}
	for _, s := range cfg.Sites {
		snap.Sites = append(snap.Sites, s.Name)
	}
	return snap, cfg, nil
}

// check reads what one cell runs: live from an API's /version, otherwise from
// the GitLab environment's last successful deployment. A cell read from GitLab
// is down when its environment was stopped (stopped is nil when GitLab
// couldn't tell) or none of its pods are ready.
func check(ctx context.Context, gl gitlab, cfg *Config, head string, stopped map[string]bool, t target) Cell {
	c := Cell{Site: t.site.Name, Service: t.svc.Name, Postfix: t.svc.Postfix, Kind: t.svc.Kind, Behind: -1}
	if t.svc.Kind == "api" && t.site.VersionURL != "" {
		c.Source = "version"
		v, err := fetchVersion(ctx, expand(t.site.VersionURL, t.site.Name, t.svc), cfg.VersionToken)
		if err != nil {
			c.State, c.Error = "DOWN", err.Error()
			return c
		}
		c.Commit, c.Schema, c.State = v.Commit, &v.Schema, classify(head, v)
		return c
	}
	c.Source = "gitlab"
	env := cfg.environment(t.site.Name, t.svc)
	d, failed, err := gl.running(ctx, env)
	if err != nil {
		c.State, c.Error = "UNKNOWN", err.Error()
		return c
	}
	if failed != nil {
		c.FailedDeploy = &FailedDeploy{Status: failed.Status, SHA: failed.SHA, At: failed.UpdatedAt, PipelineURL: failed.Deployable.Pipeline.WebURL}
	}
	if d == nil {
		c.State, c.Error = "NEVER", "no successful deployment in the GitLab environment "+env
		return c
	}
	c.Commit, c.DeployedAt, c.PipelineURL, c.pipelineID = d.SHA, &d.UpdatedAt, d.Deployable.Pipeline.WebURL, d.Deployable.Pipeline.ID
	if stopped[env] {
		c.State, c.Error = "DOWN", "the GitLab environment "+env+" is stopped"
		return c
	}
	if t.svc.Kind != "job" && t.site.AgentID > 0 { // a job's pods finish by design
		p, err := countPods(ctx, cfg.k8sProxy(), t.site.AgentID, gl.token,
			expand(cfg.Kubernetes.Namespace, t.site.Name, t.svc), expand(cfg.Kubernetes.Selector, t.site.Name, t.svc))
		if err != nil {
			p.Error = err.Error() // the agent didn't answer: keep the deployment's word
		} else if p.Ready == 0 {
			c.State, c.Error = "DOWN", "no pods running"
			if p.Total > 0 {
				c.Error = fmt.Sprintf("0 of %d pods ready", p.Total)
			}
		}
		c.Pods = &p
	}
	if c.State == "" {
		c.State = classify(head, &Version{Commit: c.Commit})
	}
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
	ctx  context.Context
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
		if n, err := b.gl.commitsBetween(b.ctx, sha, b.head); err == nil {
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
	Pinned bool      `json:"pinned"` // its pipelines were given {sha}
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
	head, err := gl.branchHead(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("read %s head: %w", branch, err)
	}
	if sha != head {
		return nil, ErrStale
	}
	// Once the fan-out starts it runs to the end, even if the caller goes
	// away, so every pipeline that was created gets recorded.
	ctx = context.WithoutCancel(ctx)
	d := &Deploy{By: by, Branch: branch, SHA: head, Pinned: cfg.pinsSHA(), Runs: make([]Run, len(cells))}
	eachLimited(len(cells), func(i int) {
		t := cells[i]
		r := Run{Site: t.site.Name, Service: t.svc.Name, Postfix: t.svc.Postfix}
		inputs := map[string]string{}
		for k, v := range cfg.PipelineInputs {
			inputs[k] = strings.ReplaceAll(expand(v, t.site.Name, t.svc), "{sha}", head)
		}
		p, err := trigger.triggerPipeline(ctx, branch, inputs, cfg.TriggerAs)
		switch {
		case err != nil:
			r.Status, r.Error = "error", err.Error()
		default:
			r.PipelineID, r.PipelineSHA, r.WebURL, r.Status = p.ID, p.SHA, p.WebURL, p.Status
			// A pipeline runs the branch. With {sha} in the inputs the CI
			// can pin the build to head anyway; without it, say so if main
			// moved between the check above and this trigger.
			if p.SHA != "" && p.SHA != head && !d.Pinned {
				r.Error = "main moved while triggering; this pipeline builds " + short(p.SHA)
			}
		}
		d.Runs[i] = r
	})
	return d, nil
}

func fetchVersion(ctx context.Context, u, token string) (*Version, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		p, err := gl.pipeline(ctx, *runs[i].PipelineID)
		if err != nil || p.Status == runs[i].Status {
			return // GitLab unreachable: keep the last known status, retry next sync
		}
		errs[i] = f.store.UpdateRunStatus(ctx, runs[i].ID, p.Status)
	})
	return errors.Join(errs...)
}
