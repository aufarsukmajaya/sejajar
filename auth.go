package main

// Sign-in. With a GitLab OAuth application set in Settings, people sign in
// with the (self-hosted) GitLab and their project role decides what they can
// do. Until then, the local admin password is the only way in; it exists for
// first setup and as break-glass (reset-password turns GitLab sign-in off).

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aufarsukmajaya/sejajar/jet/sejajar/public/model"

	"github.com/go-jet/jet/v2/qrm"
)

const (
	sessionCookie = "sj_session"
	stateCookie   = "sj_oauth"
	sessionTTL    = 12 * time.Hour

	// GitLab access levels.
	levelReporter   = 20
	levelMaintainer = 40
	levelOwner      = 50
)

// User is who is signed in. ID 0 is the local password login.
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	AvatarURL   string `json:"avatar_url"`
	AccessLevel int    `json:"access_level"`
	token       string // the user's GitLab token; "" for the local login
}

func (u *User) canConfigure() bool { return u.AccessLevel >= levelMaintainer }

type userKey struct{}

func userFrom(ctx context.Context) *User { return ctx.Value(userKey{}).(*User) }

var errNoSession = errors.New("no session")

// requireUser lets a request through only with a live session, and only to
// Maintainers when admin is set.
func (s *server) requireUser(admin bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.sessionUser(r)
		if err != nil {
			if !errors.Is(err, errNoSession) {
				log.Printf("session: %v", err)
			}
			mode := "password"
			if cfg, err := s.store.GetConfig(r.Context()); err == nil && cfg.gitlabLogin() {
				mode = "gitlab"
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first", "login": mode})
			return
		}
		if admin && !u.canConfigure() {
			writeErr(w, http.StatusForbidden, errors.New("only project Maintainers can change settings"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// sessionUser resolves the session cookie. A GitLab session refreshes its
// token (and re-checks the user's role) when the token is about to expire;
// the dashboard polls, so an open tab keeps it fresh.
func (s *server) sessionUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, errNoSession
	}
	id := hashToken(c.Value)
	row, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, qrm.ErrNoRows) {
		return nil, errNoSession
	}
	if err != nil {
		return nil, err
	}
	cfg, err := s.store.GetConfig(r.Context())
	if err != nil {
		return nil, err
	}
	// Turning GitLab sign-in on or off ends the other kind of session.
	if cfg.gitlabLogin() != (row.UserID != 0) {
		return nil, errNoSession
	}
	if row.UserID != 0 && time.Until(row.TokenExpiresAt) < time.Minute {
		if row, err = s.refresh(r.Context(), cfg, id); err != nil {
			log.Printf("refresh %s: %v", row.Username, err)
			return nil, errNoSession // signing in again starts a fresh session
		}
	}
	if row.UserID != 0 && row.AccessLevel < levelReporter {
		return nil, errNoSession // lost project access since signing in
	}
	return &User{ID: row.UserID, Username: row.Username, Name: row.Name, AvatarURL: row.AvatarURL,
		AccessLevel: int(row.AccessLevel), token: row.AccessToken}, nil
}

// refresh swaps the session's refresh token for a new access token. GitLab
// rotates refresh tokens, so two concurrent refreshes would log the user out:
// the lock serialises them and the re-read skips work another request did.
// ponytail: in-process lock; run one replica, or move to SELECT ... FOR UPDATE for more.
func (s *server) refresh(ctx context.Context, cfg *Config, id string) (model.Sessions, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	row, err := s.store.GetSession(ctx, id)
	if err != nil || time.Until(row.TokenExpiresAt) >= time.Minute {
		return row, err
	}
	tok, err := oauthToken(ctx, cfg, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {row.RefreshToken}})
	if err != nil {
		return row, err
	}
	level, err := newGitlab(cfg, s.fleet.token).memberLevel(ctx, row.UserID)
	if err != nil {
		return row, err
	}
	if row.AccessLevel == levelOwner && level < levelOwner {
		level = levelOwner // instance admins keep the level they signed in with
	}
	row.AccessLevel, row.AccessToken, row.RefreshToken, row.TokenExpiresAt = int32(level), tok.AccessToken, tok.RefreshToken, tok.expiry()
	return row, s.store.UpdateSessionToken(ctx, id, row.AccessLevel, row.AccessToken, row.RefreshToken, row.TokenExpiresAt)
}

// ===== GITLAB OAUTH =====

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (t oauthTokenResponse) expiry() time.Time {
	if t.ExpiresIn <= 0 {
		return time.Now().Add(2 * time.Hour) // GitLab's default
	}
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

func oauthToken(ctx context.Context, cfg *Config, form url.Values) (oauthTokenResponse, error) {
	form.Set("client_id", cfg.OAuthClientID)
	form.Set("client_secret", cfg.OAuthSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gitlabRoot(cfg)+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var t oauthTokenResponse
	err = doJSON(req, &t)
	if err == nil && t.AccessToken == "" {
		err = errors.New("gitlab returned no access token")
	}
	return t, err
}

func gitlabRoot(cfg *Config) string { return strings.TrimRight(cfg.GitLab.URL, "/") }

// callbackURL is the redirect URI to register on the GitLab application.
func callbackURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// gitlabStart sends the browser to GitLab's consent page. The state and the
// PKCE verifier ride in a short-lived cookie scoped to /auth/.
func (s *server) gitlabStart(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetConfig(r.Context())
	if err != nil || !cfg.gitlabLogin() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state, verifier := rand.Text(), rand.Text()+rand.Text()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state + "." + verifier, Path: "/auth/", MaxAge: 600,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {cfg.OAuthClientID},
		"redirect_uri":          {callbackURL(r)},
		"response_type":         {"code"},
		"scope":                 {"api"}, // api, not read_user: deploys are triggered as the user
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, gitlabRoot(cfg)+"/oauth/authorize?"+q.Encode(), http.StatusFound)
}

// gitlabCallback finishes the sign-in. Failures go back to the login page
// as ?login_error=, which the page shows.
func (s *server) gitlabCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		http.Redirect(w, r, "/?login_error="+url.QueryEscape(msg), http.StatusFound)
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/", MaxAge: -1})
	c, err := r.Cookie(stateCookie)
	if err != nil {
		fail("sign-in expired, try again")
		return
	}
	state, verifier, _ := strings.Cut(c.Value, ".")
	if got := r.FormValue("state"); got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
		fail("sign-in state mismatch, try again")
		return
	}
	if e := r.FormValue("error"); e != "" {
		fail("GitLab: " + e)
		return
	}
	cfg, err := s.store.GetConfig(r.Context())
	if err != nil || !cfg.gitlabLogin() {
		fail("GitLab sign-in is not configured")
		return
	}
	tok, err := oauthToken(r.Context(), cfg, url.Values{"grant_type": {"authorization_code"}, "code": {r.FormValue("code")},
		"redirect_uri": {callbackURL(r)}, "code_verifier": {verifier}})
	if err != nil {
		log.Printf("oauth token: %v", err)
		fail("GitLab rejected the sign-in")
		return
	}
	var me struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
		IsAdmin   bool   `json:"is_admin"`
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, gitlabRoot(cfg)+"/api/v4/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if err := doJSON(req, &me); err != nil || me.ID == 0 {
		log.Printf("oauth user: %v", err)
		fail("could not read your GitLab profile")
		return
	}
	level, err := newGitlab(cfg, s.fleet.token).memberLevel(r.Context(), me.ID)
	if err != nil {
		log.Printf("member level of %s: %v", me.Username, err)
		fail("could not check your access to " + cfg.GitLab.Project)
		return
	}
	if me.IsAdmin {
		level = levelOwner
	}
	if level < levelReporter {
		fail(me.Username + " needs at least Reporter access to " + cfg.GitLab.Project)
		return
	}
	err = s.startSession(w, r, model.Sessions{UserID: me.ID, Username: me.Username, Name: me.Name, AvatarURL: me.AvatarURL,
		AccessLevel: int32(level), AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, TokenExpiresAt: tok.expiry()})
	if err != nil {
		log.Printf("start session: %v", err)
		fail("could not start a session")
		return
	}
	log.Printf("%s signed in with GitLab (access level %d)", me.Username, level)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ===== LOCAL PASSWORD =====

// passwordLogin is the setup / break-glass login; refused once GitLab sign-in is on.
func (s *server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if code, err := readJSON(w, r, &req); err != nil {
		writeErr(w, code, err)
		return
	}
	cfg, err := s.store.GetConfig(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if cfg.gitlabLogin() {
		writeErr(w, http.StatusForbidden, errors.New("password sign-in is off; sign in with GitLab"))
		return
	}
	user, hash, err := s.store.GetLogin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// ponytail: no rate limit; PBKDF2's cost slows guessing. Add one if this faces the internet.
	if subtle.ConstantTimeCompare([]byte(req.User), []byte(user)) != 1 || !checkPassword(hash, req.Password) {
		writeErr(w, http.StatusUnauthorized, errors.New("wrong user or password"))
		return
	}
	if err := s.startSession(w, r, model.Sessions{Username: user, Name: user, AccessLevel: levelOwner}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": user})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "can_configure": u.canConfigure()})
}

// startSession stores only the cookie's hash, so a database leak can't be
// replayed as a login.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, row model.Sessions) error {
	raw := rand.Text() + rand.Text()
	row.ID, row.ExpiresAt = hashToken(raw), time.Now().Add(sessionTTL)
	if row.TokenExpiresAt.IsZero() {
		row.TokenExpiresAt = row.ExpiresAt
	}
	if err := s.store.AddSession(r.Context(), row); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	return nil
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

const pbkdf2Iter = 600_000 // OWASP 2023 for PBKDF2-HMAC-SHA256

// hashPassword returns "pbkdf2-sha256$<iter>$<salt hex>$<key hex>".
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%x$%x", pbkdf2Iter, salt, key), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err1 := strconv.Atoi(parts[1])
	salt, err2 := hex.DecodeString(parts[2])
	want, err3 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// newPassword returns a random password for first run and reset-password.
func newPassword() string { return rand.Text() }
