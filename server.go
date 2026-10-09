package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	fleet     *Fleet
	store     *Store
	refreshMu sync.Mutex

	// The last snapshot, shared by every open dashboard: a sweep is hundreds
	// of calls, so N tabs must not cost N sweeps. There is no background
	// sweep: nobody looking means no calls. snapMu serialises sweeps; a
	// settings save bumps configGen, which invalidates the cached one.
	snapMu    sync.Mutex
	snap      *Snapshot
	snapAt    time.Time
	snapTTL   time.Duration
	snapGen   int64
	configGen atomic.Int64

	// Deploy history is polled by every open tab while a deploy runs; one
	// GitLab sync per runSyncEvery serves them all.
	syncMu sync.Mutex
	syncAt time.Time
}

// snapshotTimeout only stops a sweep that hangs; each GitLab, /version and
// pod call has its own short timeout.
const snapshotTimeout = 5 * time.Minute

// runSyncEvery is a var so tests can sync on every request.
var runSyncEvery = 2 * time.Second

func newServer(f *Fleet) *server { return &server{fleet: f, store: f.store} }

func newHandler(f *Fleet) http.Handler { return newServer(f).routes() }

func (s *server) routes() http.Handler {
	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	// The page itself holds no data; it shows the sign-in screen until /api/me answers.
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /auth/gitlab", s.gitlabStart)
	mux.HandleFunc("GET /auth/callback", s.gitlabCallback)
	mux.HandleFunc("POST /auth/password", s.passwordLogin)
	mux.HandleFunc("POST /auth/logout", s.logout)

	mux.Handle("GET /api/me", s.requireUser(false, s.me))
	mux.Handle("GET /api/status", s.requireUser(false, s.status))
	mux.Handle("GET /api/deploys", s.requireUser(false, s.listDeploys))
	mux.Handle("POST /api/deploys", s.requireUser(false, s.createDeploy))
	mux.Handle("GET /api/versions", s.requireUser(false, s.cellVersions))
	mux.Handle("GET /api/settings", s.requireUser(true, s.getSettings))
	mux.Handle("PUT /api/settings", s.requireUser(true, s.putSettings))
	return mux
}

// observe returns a snapshot at most poll_seconds old (fresh forces a new
// one) and records every cell whose version changed.
func (s *server) observe(ctx context.Context, fresh bool) (*Snapshot, error) {
	asked := time.Now()
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	gen := s.configGen.Load()
	if s.snap != nil && s.snapGen == gen {
		// A sweep that finished while this caller waited is as fresh as any.
		if s.snapAt.After(asked) || !fresh && time.Since(s.snapAt) < s.snapTTL {
			return s.snap, nil
		}
	}
	// The result is shared, so one caller going away must not abort it.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	snap, cfg, err := s.fleet.snapshot(sctx)
	if err == nil && sctx.Err() != nil {
		// Cells checked after the deadline failed for that reason alone;
		// don't serve or record them as outages.
		err = fmt.Errorf("checking the fleet took over %s", snapshotTimeout)
	}
	if err != nil {
		return nil, err
	}
	s.snap, s.snapAt, s.snapTTL, s.snapGen = snap, time.Now(), time.Duration(cfg.PollSeconds)*time.Second, gen
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer rcancel()
	if n, err := s.store.RecordSnapshot(rctx, snap); err != nil {
		log.Printf("record snapshot: %v", err)
	} else if n > 0 {
		log.Printf("recorded %d version change(s)", n)
	}
	return snap, nil
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	snap, err := s.observe(r.Context(), r.URL.Query().Has("fresh"))
	if errors.Is(err, ErrNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *server) listDeploys(w http.ResponseWriter, r *http.Request) {
	s.syncRuns(r.Context())
	list, err := s.store.ListDeploys(r.Context(), 30)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) syncRuns(ctx context.Context) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if time.Since(s.syncAt) < runSyncEvery {
		return
	}
	if err := s.fleet.SyncRuns(context.WithoutCancel(ctx)); err != nil {
		log.Printf("sync runs: %v", err)
	}
	s.syncAt = time.Now()
}

// readJSON decodes a write request's body. Requiring a JSON content type
// forces a CORS preflight, which this server never answers, so another site
// cannot make a logged-in browser deploy or change settings.
func readJSON(w http.ResponseWriter, r *http.Request, v any) (int, error) {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		return http.StatusUnsupportedMediaType, errors.New("content type must be application/json")
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(v); err != nil {
		return http.StatusBadRequest, fmt.Errorf("bad body: %w", err)
	}
	return 0, nil
}

func (s *server) createDeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Targets []Target `json:"targets"`
		SHA     string   `json:"sha"`
	}
	if code, err := readJSON(w, r, &req); err != nil {
		writeErr(w, code, err)
		return
	}
	if len(req.Targets) == 0 || req.SHA == "" {
		writeErr(w, http.StatusBadRequest, errors.New(`body must be {"targets": [{"site", "service", "postfix"}, ...], "sha": "<head sha>"}`))
		return
	}
	u := userFrom(r.Context())
	by := u.Username
	d, err := s.fleet.Deploy(r.Context(), by, req.Targets, req.SHA, u.token)
	if errors.Is(err, ErrStale) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The pipelines exist now; record them even if the client went away.
	if err := s.store.AddDeploy(context.WithoutCancel(r.Context()), d); err != nil {
		// The pipelines are already running; say so instead of pretending it failed.
		log.Printf("deploy of %s triggered but not saved: %v", short(d.SHA), err)
		writeJSON(w, http.StatusCreated, map[string]any{"deploy": d, "warning": "triggered, but not saved to history: " + err.Error()})
		return
	}
	log.Printf("deploy %d by %s: %s to %d site(s)", d.ID, by, short(d.SHA), len(d.Runs))
	writeJSON(w, http.StatusCreated, map[string]any{"deploy": d})
}

// cellVersions is one (site, service) cell's history: ?site=&service=&postfix=
func (s *server) cellVersions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.store.ListSiteVersions(r.Context(), q.Get("site"), q.Get("service"), q.Get("postfix"), 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// settingsView is what GET /api/settings returns: the config without the
// version token, which is write-only.
type settingsView struct {
	*Config
	VersionTokenSet bool   `json:"version_token_set"`
	OAuthSecretSet  bool   `json:"oauth_secret_set"`
	RedirectURI     string `json:"redirect_uri"` // to register on the GitLab application
	AdminUser       string `json:"admin_user"`
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetConfig(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	user, _, err := s.store.GetLogin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsView{Config: cfg, VersionTokenSet: cfg.VersionToken != "", OAuthSecretSet: cfg.OAuthSecret != "",
		RedirectURI: callbackURL(r), AdminUser: user})
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config
		VersionToken *string `json:"version_token"` // nil keeps the stored token, "" clears it
		OAuthSecret  *string `json:"oauth_secret"`  // same
		AdminUser    string  `json:"admin_user"`
		Password     string  `json:"password"` // "" keeps the current password
	}
	if code, err := readJSON(w, r, &req); err != nil {
		writeErr(w, code, err)
		return
	}
	req.AdminUser = strings.TrimSpace(req.AdminUser)
	if err := req.Config.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Password != "" && len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, errors.New("password must be at least 8 characters"))
		return
	}
	by := userFrom(r.Context()).Username
	if err := s.store.SaveConfig(r.Context(), &req.Config, req.VersionToken, req.OAuthSecret, by); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.configGen.Add(1)
	if req.AdminUser != "" || req.Password != "" {
		user, hash, err := s.store.GetLogin(r.Context())
		if err == nil && req.AdminUser != "" {
			user = req.AdminUser
		}
		if err == nil && req.Password != "" {
			hash, err = hashPassword(req.Password)
		}
		if err == nil {
			err = s.store.SetLogin(r.Context(), user, hash)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("settings saved, login not changed: %w", err))
			return
		}
	}
	log.Printf("settings saved by %s: %d site(s)", by, len(req.Sites))
	s.getSettings(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
