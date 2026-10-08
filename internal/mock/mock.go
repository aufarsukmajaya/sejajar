// Package mock fakes the slice of GitLab that sejajar calls, plus a fleet
// of sites serving /version, so the whole flow runs locally and in tests.
// A triggered pipeline "deploys" after Delay: the site moves to DEPLOY_SHA and
// its DB gains that commit's migrations.
package mock

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The OAuth application the mock accepts; put these in Settings.
const (
	ClientID     = "sejajar-demo"
	ClientSecret = "demo-secret"
)

// User is a GitLab account. Level is its access on the project, 0 = not a member.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Level    int    `json:"-"`
}

// Users: a Maintainer, a Developer, a Reporter (can look, GitLab refuses
// their pipelines) and an outsider.
var Users = []User{
	{7, "dewi", "Dewi Lestari", 40},
	{8, "budi", "Budi Santoso", 30},
	{9, "tamu", "Tamu Reporter", 20},
	{10, "luar", "Orang Luar", 0},
}

type grant struct {
	user                User
	challenge, redirect string
}

type commit struct {
	sha        string
	migrations []string
}

type site struct {
	commit  string
	applied []string
	down    bool
}

type pipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
	User   struct {
		Username string `json:"username"`
	} `json:"user"`
	site string
	sha  string
}

type Server struct {
	Delay     time.Duration
	TokenTTL  time.Duration // how long an OAuth access token lives
	mu        sync.Mutex
	codes     map[string]grant
	access    map[string]User // OAuth access token -> user
	refresh   map[string]User
	commits   []commit // oldest first; the last one is the branch head
	sites     map[string]*site
	pipelines map[int64]*pipeline
	nextID    int64
	mux       *http.ServeMux
}

// SiteNames are the demo sites, in the order sites.demo.json lists them.
var SiteNames = []string{"jakarta", "surabaya", "medan", "bandung", "makassar"}

func New() *Server {
	migs := []string{
		"M20260105090000_init",
		"M20260302100000_invoice_department",
		"m20260611120000_invoice_attachment",
		"M20260913120000_job_runs",
	}
	s := &Server{Delay: 4 * time.Second, TokenTTL: 2 * time.Hour, sites: map[string]*site{}, pipelines: map[int64]*pipeline{}, nextID: 1000,
		codes: map[string]grant{}, access: map[string]User{}, refresh: map[string]User{}}
	for i, n := range []int{1, 2, 2, 3, 3, 4} { // how many migrations each commit carries
		s.commits = append(s.commits, commit{sha: fakeSHA(i), migrations: slices.Clone(migs[:n])})
	}
	sha := func(i int) string { return s.commits[i].sha }
	s.sites["jakarta"] = &site{commit: sha(5), applied: slices.Clone(migs)}                  // OK
	s.sites["surabaya"] = &site{commit: sha(3), applied: slices.Clone(migs[:3])}             // BEHIND by 2
	s.sites["medan"] = &site{commit: sha(5), applied: slices.Clone(migs[:3])}                // SCHEMA_PENDING
	s.sites["bandung"] = &site{commit: sha(1), applied: slices.Clone(migs[:3])}              // DB_AHEAD (rolled back)
	s.sites["makassar"] = &site{commit: sha(4), applied: slices.Clone(migs[:3]), down: true} // DOWN

	m := http.NewServeMux()
	m.HandleFunc("GET /api/v4/projects/{project}/repository/branches/{branch}", s.branch)
	m.HandleFunc("GET /api/v4/projects/{project}/repository/compare", s.compare)
	m.HandleFunc("POST /api/v4/projects/{project}/pipeline", s.createPipeline)
	m.HandleFunc("GET /api/v4/projects/{project}/pipelines/{id}", s.getPipeline)
	m.HandleFunc("GET /pipelines/{id}", s.getPipeline)
	m.HandleFunc("GET /api/v4/projects/{project}/members/all/{id}", s.member)
	m.HandleFunc("GET /api/v4/user", s.currentUser)
	m.HandleFunc("GET /oauth/authorize", s.authorize)
	m.HandleFunc("POST /oauth/token", s.token)
	m.HandleFunc("GET /sites/{site}/version", s.version)
	m.HandleFunc("POST /mock/push", s.push)
	m.HandleFunc("POST /mock/sites/{site}/toggle", s.toggle)
	s.mux = m
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func fakeSHA(i int) string {
	h := sha1.Sum([]byte("commit-" + strconv.Itoa(i)))
	return hex.EncodeToString(h[:])
}

func (s *Server) index(sha string) int {
	return slices.IndexFunc(s.commits, func(c commit) bool { return c.sha == sha })
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply(w, 200, map[string]any{"commit": map[string]string{"id": s.commits[len(s.commits)-1].sha}})
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, to := s.index(r.URL.Query().Get("from")), s.index(r.URL.Query().Get("to"))
	if from < 0 || to < 0 {
		reply(w, 404, map[string]string{"message": "404 Not found"})
		return
	}
	commits := []map[string]string{}
	for i := from + 1; i <= to; i++ {
		commits = append(commits, map[string]string{"id": s.commits[i].sha})
	}
	reply(w, 200, map[string]any{"commits": commits})
}

func (s *Server) createPipeline(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Variables []struct{ Key, Value string } `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, 400, map[string]string{"message": err.Error()})
		return
	}
	vars := map[string]string{}
	for _, v := range body.Variables {
		vars[v.Key] = v.Value
	}
	s.mu.Lock()
	by := "deploy-bot"
	if u, ok := s.access[bearer(r)]; ok {
		if u.Level < 30 {
			s.mu.Unlock()
			reply(w, 403, map[string]string{"message": "403 Forbidden - " + u.Username + " is not allowed to run pipelines"})
			return
		}
		by = u.Username
	}
	s.nextID++
	p := &pipeline{ID: s.nextID, Status: "running", WebURL: fmt.Sprintf("http://%s/pipelines/%d", r.Host, s.nextID), site: vars["SITE"], sha: vars["DEPLOY_SHA"]}
	p.User.Username = by
	s.pipelines[p.ID] = p
	out := *p
	s.mu.Unlock()

	time.AfterFunc(s.Delay, func() { s.finish(p) })
	reply(w, 201, out)
}

// finish plays the deploy job: migrate the DB, then roll the code.
func (s *Server) finish(p *pipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, i := s.sites[p.site], s.index(p.sha)
	if st == nil || st.down || i < 0 {
		p.Status = "failed"
		return
	}
	for _, m := range s.commits[i].migrations {
		if !slices.Contains(st.applied, m) {
			st.applied = append(st.applied, m)
		}
	}
	st.commit = p.sha
	p.Status = "success"
}

func (s *Server) getPipeline(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pipelines[id]
	if !ok {
		reply(w, 404, map[string]string{"message": "404 Not found"})
		return
	}
	reply(w, 200, p)
}

// version implements VERSION_API.md for a fake site.
func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sites[r.PathValue("site")]
	if !ok {
		reply(w, 404, map[string]string{"error": "no such site"})
		return
	}
	if st.down {
		reply(w, 503, map[string]string{"commit": st.commit, "error": "db unreachable"})
		return
	}
	code := s.commits[s.index(st.commit)].migrations
	pending, unknown, last := []string{}, []string{}, ""
	for _, m := range code {
		if slices.Contains(st.applied, m) {
			last = m
		} else {
			pending = append(pending, m)
		}
	}
	for _, m := range st.applied {
		if !slices.Contains(code, m) {
			unknown = append(unknown, m)
		}
	}
	reply(w, 200, map[string]any{
		"commit":   st.commit,
		"built_at": "2026-10-01T00:00:00Z",
		"schema":   map[string]any{"code": code[len(code)-1], "db": last, "pending": pending, "unknown": unknown},
	})
}

// push simulates a merge to main that carries one new migration.
func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.commits)
	migs := append(slices.Clone(s.commits[n-1].migrations), fmt.Sprintf("M202610%02d120000_change_%d", n, n))
	s.commits = append(s.commits, commit{sha: fakeSHA(n), migrations: migs})
	reply(w, 201, map[string]string{"head": fakeSHA(n)})
}

// toggle takes a site down or brings it back.
func (s *Server) toggle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sites[r.PathValue("site")]
	if !ok {
		reply(w, 404, map[string]string{"error": "no such site"})
		return
	}
	st.down = !st.down
	reply(w, 200, map[string]bool{"down": st.down})
}

// TriggeredBy is the username a pipeline was created as.
func (s *Server) TriggeredBy(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pipelines[id]; ok {
		return p.User.Username
	}
	return ""
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) member(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	i := slices.IndexFunc(Users, func(u User) bool { return u.ID == id })
	if i < 0 || Users[i].Level == 0 {
		reply(w, 404, map[string]string{"message": "404 Not found"})
		return
	}
	reply(w, 200, map[string]any{"id": id, "username": Users[i].Username, "access_level": Users[i].Level})
}

func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	u, ok := s.access[bearer(r)]
	s.mu.Unlock()
	if !ok {
		reply(w, 401, map[string]string{"message": "401 Unauthorized"})
		return
	}
	reply(w, 200, map[string]any{"id": u.ID, "username": u.Username, "name": u.Name, "avatar_url": "", "is_admin": false})
}

// authorize plays GitLab's consent page: pick a user (or pass mock_user=)
// and it redirects back with a code.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "unknown application or missing PKCE", 400)
		return
	}
	name := q.Get("mock_user")
	i := slices.IndexFunc(Users, func(u User) bool { return u.Username == name })
	if i < 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta name=viewport content="width=device-width"><body style="font:16px system-ui;background:#111;color:#eee;padding:40px"><h2>Mock GitLab: sign in as</h2>`)
		for _, u := range Users {
			q.Set("mock_user", u.Username)
			fmt.Fprintf(w, `<p><a style="color:#d4af37" href="?%s">%s</a> (level %d)</p>`, html.EscapeString(q.Encode()), html.EscapeString(u.Username), u.Level)
		}
		return
	}
	code := rand.Text()
	s.mu.Lock()
	s.codes[code] = grant{user: Users[i], challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	s.mu.Unlock()
	back := url.Values{"code": {code}, "state": {q.Get("state")}}
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("client_id") != ClientID || r.FormValue("client_secret") != ClientSecret {
		reply(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var u User
	switch r.FormValue("grant_type") {
	case "authorization_code":
		g, ok := s.codes[r.FormValue("code")]
		delete(s.codes, r.FormValue("code"))
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.FormValue("redirect_uri") != g.redirect {
			reply(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		u = g.user
	case "refresh_token":
		var ok bool
		if u, ok = s.refresh[r.FormValue("refresh_token")]; !ok {
			reply(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		delete(s.refresh, r.FormValue("refresh_token")) // GitLab rotates refresh tokens
	default:
		reply(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	access, refresh := "at-"+rand.Text(), "rt-"+rand.Text()
	s.access[access], s.refresh[refresh] = u, u
	reply(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": int(s.TokenTTL.Seconds())})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
