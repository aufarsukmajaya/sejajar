package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/aufarsukmajaya/resonate/jet/resonate/public/model"
	"github.com/aufarsukmajaya/resonate/jet/resonate/public/table"

	pg "github.com/go-jet/jet/v2/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

// activeStatuses are the GitLab pipeline states that can still change.
var activeStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

type Store struct{ db *sql.DB }

// openStore connects and applies schema.sql (idempotent DDL).
func openStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ===== DEPLOYS =====

// AddDeploy saves a deploy and its runs, filling in their ids and created time.
func (s *Store) AddDeploy(ctx context.Context, d *Deploy) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var row model.Deploys
	err = table.Deploys.
		INSERT(table.Deploys.CreatedBy, table.Deploys.Branch, table.Deploys.Sha).
		VALUES(d.By, d.Branch, d.SHA).
		RETURNING(table.Deploys.AllColumns).
		QueryContext(ctx, tx, &row)
	if err != nil {
		return fmt.Errorf("insert deploy: %w", err)
	}
	d.ID, d.At = row.ID, row.CreatedAt

	runs := make([]model.DeployRuns, len(d.Runs))
	for i, r := range d.Runs {
		runs[i] = model.DeployRuns{DeployID: row.ID, Site: r.Site, Service: r.Service, Postfix: r.Postfix,
			PipelineSha: r.PipelineSHA, WebURL: r.WebURL, Status: r.Status, Error: r.Error}
		if r.PipelineID != 0 {
			runs[i].PipelineID = &d.Runs[i].PipelineID
		}
	}
	var saved []model.DeployRuns
	err = table.DeployRuns.
		INSERT(table.DeployRuns.DeployID, table.DeployRuns.Site, table.DeployRuns.Service, table.DeployRuns.Postfix,
			table.DeployRuns.PipelineID, table.DeployRuns.PipelineSha, table.DeployRuns.WebURL, table.DeployRuns.Status, table.DeployRuns.Error).
		MODELS(runs).
		RETURNING(table.DeployRuns.ID, table.DeployRuns.Site, table.DeployRuns.Service, table.DeployRuns.Postfix).
		QueryContext(ctx, tx, &saved)
	if err != nil {
		return fmt.Errorf("insert deploy runs: %w", err)
	}
	ids := map[cellKey]int64{} // a deploy has each cell once (Config.resolve dedupes)
	for _, r := range saved {
		ids[cellKey{r.Site, r.Service, r.Postfix}] = r.ID
	}
	for i, r := range d.Runs {
		d.Runs[i].ID = ids[cellKey{r.Site, r.Service, r.Postfix}]
	}
	return tx.Commit()
}

// ListDeploys returns the newest deploys with their runs.
func (s *Store) ListDeploys(ctx context.Context, limit int64) ([]Deploy, error) {
	latest := pg.SELECT(table.Deploys.ID).
		FROM(table.Deploys).
		ORDER_BY(table.Deploys.ID.DESC()).
		LIMIT(limit)

	var rows []struct {
		model.Deploys
		Runs []model.DeployRuns
	}
	err := pg.SELECT(table.Deploys.AllColumns, table.DeployRuns.AllColumns).
		FROM(table.Deploys.LEFT_JOIN(table.DeployRuns, table.DeployRuns.DeployID.EQ(table.Deploys.ID))).
		WHERE(table.Deploys.ID.IN(latest)).
		ORDER_BY(table.Deploys.ID.DESC(), table.DeployRuns.ID.ASC()).
		QueryContext(ctx, s.db, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]Deploy, len(rows))
	for i, r := range rows {
		d := Deploy{ID: r.ID, At: r.CreatedAt, By: r.CreatedBy, Branch: r.Branch, SHA: r.Sha, Runs: []Run{}}
		for _, x := range r.Runs {
			run := Run{ID: x.ID, Site: x.Site, Service: x.Service, Postfix: x.Postfix, PipelineSHA: x.PipelineSha,
				WebURL: x.WebURL, Status: x.Status, Error: x.Error}
			if x.PipelineID != nil {
				run.PipelineID = *x.PipelineID
			}
			d.Runs = append(d.Runs, run)
		}
		out[i] = d
	}
	return out, nil
}

// ===== DEPLOY RUNS =====

// ListActiveRuns returns runs whose pipeline can still change state.
func (s *Store) ListActiveRuns(ctx context.Context) ([]model.DeployRuns, error) {
	statuses := make([]pg.Expression, len(activeStatuses))
	for i, st := range activeStatuses {
		statuses[i] = pg.String(st)
	}
	var rows []model.DeployRuns
	err := pg.SELECT(table.DeployRuns.AllColumns).
		FROM(table.DeployRuns).
		WHERE(table.DeployRuns.Status.IN(statuses...).AND(table.DeployRuns.PipelineID.IS_NOT_NULL())).
		QueryContext(ctx, s.db, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) UpdateRunStatus(ctx context.Context, id int64, status string) error {
	_, err := table.DeployRuns.
		UPDATE(table.DeployRuns.Status, table.DeployRuns.UpdatedAt).
		SET(pg.String(status), pg.NOW()).
		WHERE(table.DeployRuns.ID.EQ(pg.Int64(id))).
		ExecContext(ctx, s.db)
	return err
}

// ===== SITE VERSIONS =====

// SiteVersion is one row of the commit + schema tracker.
type SiteVersion struct {
	ObservedAt time.Time `json:"observed_at"`
	State      string    `json:"state"`
	Source     string    `json:"source"`
	Commit     string    `json:"commit"`
	SchemaCode string    `json:"schema_code"`
	SchemaDB   string    `json:"schema_db"`
	Pending    []string  `json:"pending"`
	Unknown    []string  `json:"unknown"`
	Error      string    `json:"error,omitempty"`
}

type cellKey struct{ site, service, postfix string }

// RecordSnapshot appends a site_versions row for every cell whose state,
// commit or schema differs from its last recorded row. Returns rows written.
func (s *Store) RecordSnapshot(ctx context.Context, snap *Snapshot) (int, error) {
	var last []model.SiteVersions
	err := pg.SELECT(table.SiteVersions.AllColumns).
		DISTINCT(table.SiteVersions.Site, table.SiteVersions.Service, table.SiteVersions.Postfix).
		FROM(table.SiteVersions).
		ORDER_BY(table.SiteVersions.Site.ASC(), table.SiteVersions.Service.ASC(), table.SiteVersions.Postfix.ASC(), table.SiteVersions.ObservedAt.DESC()).
		QueryContext(ctx, s.db, &last)
	if err != nil {
		return 0, err
	}
	prev := map[cellKey]model.SiteVersions{}
	for _, l := range last {
		prev[cellKey{l.Site, l.Service, l.Postfix}] = l
	}

	var changed []model.SiteVersions
	for _, c := range snap.Cells {
		// UNKNOWN means GitLab didn't answer, not that anything changed;
		// recording it would fill the history with flip-flops.
		if c.State == "UNKNOWN" {
			continue
		}
		row := model.SiteVersions{Site: c.Site, Service: c.Service, Postfix: c.Postfix, State: c.State, Source: c.Source,
			CommitSha: c.Commit, Error: c.Error, Pending: "[]", Unknown: "[]"}
		if c.Schema != nil {
			row.SchemaCode, row.SchemaDb = c.Schema.Code, c.Schema.DB
			row.Pending, row.Unknown = jsonList(c.Schema.Pending), jsonList(c.Schema.Unknown)
		}
		if p, ok := prev[cellKey{c.Site, c.Service, c.Postfix}]; ok && sameVersion(p, row) {
			continue
		}
		changed = append(changed, row)
	}
	if len(changed) == 0 {
		return 0, nil
	}
	_, err = table.SiteVersions.
		INSERT(table.SiteVersions.Site, table.SiteVersions.Service, table.SiteVersions.Postfix, table.SiteVersions.State,
			table.SiteVersions.Source, table.SiteVersions.CommitSha, table.SiteVersions.SchemaCode, table.SiteVersions.SchemaDb,
			table.SiteVersions.Pending, table.SiteVersions.Unknown, table.SiteVersions.Error).
		MODELS(changed).
		ExecContext(ctx, s.db)
	return len(changed), err
}

// ListSiteVersions returns one cell's tracker rows, newest first.
func (s *Store) ListSiteVersions(ctx context.Context, site, service, postfix string, limit int64) ([]SiteVersion, error) {
	var rows []model.SiteVersions
	err := pg.SELECT(table.SiteVersions.AllColumns).
		FROM(table.SiteVersions).
		WHERE(table.SiteVersions.Site.EQ(pg.String(site)).
			AND(table.SiteVersions.Service.EQ(pg.String(service))).
			AND(table.SiteVersions.Postfix.EQ(pg.String(postfix)))).
		ORDER_BY(table.SiteVersions.ObservedAt.DESC()).
		LIMIT(limit).
		QueryContext(ctx, s.db, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]SiteVersion, len(rows))
	for i, r := range rows {
		out[i] = SiteVersion{ObservedAt: r.ObservedAt, State: r.State, Source: r.Source, Commit: r.CommitSha, SchemaCode: r.SchemaCode,
			SchemaDB: r.SchemaDb, Pending: parseList(r.Pending), Unknown: parseList(r.Unknown), Error: r.Error}
	}
	return out, nil
}

// sameVersion compares what a site runs, ignoring the error text (a flaky
// site's timeouts differ in wording but are one outage).
func sameVersion(a, b model.SiteVersions) bool {
	return a.State == b.State && a.Source == b.Source && a.CommitSha == b.CommitSha && a.SchemaCode == b.SchemaCode && a.SchemaDb == b.SchemaDb &&
		slices.Equal(parseList(a.Pending), parseList(b.Pending)) && slices.Equal(parseList(a.Unknown), parseList(b.Unknown))
}

func jsonList(xs []string) string {
	if xs == nil {
		xs = []string{}
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// parseList reads a jsonb array back; Postgres re-formats jsonb, so compare
// parsed values, never the text.
func parseList(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// ===== SETTINGS =====

// GetConfig reads the runtime configuration and the fleet: sites by name,
// services by kind (api, worker, job) then name.
func (s *Store) GetConfig(ctx context.Context) (*Config, error) {
	var row model.Settings
	err := pg.SELECT(table.Settings.AllColumns).FROM(table.Settings).QueryContext(ctx, s.db, &row)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var sites []model.Sites
	err = pg.SELECT(table.Sites.AllColumns).FROM(table.Sites).ORDER_BY(table.Sites.Name.ASC()).QueryContext(ctx, s.db, &sites)
	if err != nil {
		return nil, fmt.Errorf("read sites: %w", err)
	}
	var services []model.Services
	err = pg.SELECT(table.Services.AllColumns).FROM(table.Services).
		ORDER_BY(table.Services.Name.ASC(), table.Services.Postfix.ASC()).
		QueryContext(ctx, s.db, &services)
	if err != nil {
		return nil, fmt.Errorf("read services: %w", err)
	}
	c := &Config{PollSeconds: int(row.PollSeconds), VersionToken: row.VersionToken, Sites: []Site{}, Services: []Service{},
		EnvironmentTemplate: row.EnvironmentTemplate, APIEnvironmentTemplate: row.APIEnvironmentTemplate, TriggerAs: row.TriggerAs,
		OAuthClientID: row.OAuthClientID, OAuthSecret: row.OAuthClientSecret}
	c.GitLab.URL, c.GitLab.Project, c.GitLab.Branch = row.GitlabURL, row.GitlabProject, row.GitlabBranch
	if err := json.Unmarshal([]byte(row.PipelineInputs), &c.PipelineInputs); err != nil {
		return nil, fmt.Errorf("read pipeline inputs: %w", err)
	}
	for _, x := range sites {
		c.Sites = append(c.Sites, Site{Name: x.Name, VersionURL: x.VersionURL})
	}
	for _, x := range services {
		c.Services = append(c.Services, Service{Name: x.Name, Postfix: x.Postfix, Kind: x.Kind, Sites: parseList(x.Sites)})
	}
	slices.SortStableFunc(c.Services, func(a, b Service) int { return slices.Index(kinds, a.Kind) - slices.Index(kinds, b.Kind) })
	return c, nil
}

// SaveConfig replaces the settings, sites and services in one transaction.
// A nil versionToken or oauthSecret keeps the stored one (the API never sends
// secrets back out).
func (s *Store) SaveConfig(ctx context.Context, c *Config, versionToken, oauthSecret *string, by string) error {
	inputs, err := json.Marshal(c.PipelineInputs)
	if err != nil {
		return err
	}
	if c.PipelineInputs == nil {
		inputs = []byte("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cols := pg.ColumnList{table.Settings.GitlabURL, table.Settings.GitlabProject, table.Settings.GitlabBranch,
		table.Settings.EnvironmentTemplate, table.Settings.APIEnvironmentTemplate, table.Settings.PipelineInputs, table.Settings.TriggerAs,
		table.Settings.PollSeconds, table.Settings.OAuthClientID, table.Settings.UpdatedAt, table.Settings.UpdatedBy}
	vals := []any{pg.String(c.GitLab.URL), pg.String(c.GitLab.Project), pg.String(c.GitLab.Branch),
		pg.String(c.EnvironmentTemplate), pg.String(c.APIEnvironmentTemplate), pg.Json(string(inputs)), pg.String(c.TriggerAs),
		pg.Int32(int32(c.PollSeconds)), pg.String(c.OAuthClientID), pg.NOW(), pg.String(by)}
	if versionToken != nil {
		cols = append(cols, table.Settings.VersionToken)
		vals = append(vals, pg.String(*versionToken))
	}
	if oauthSecret != nil {
		cols = append(cols, table.Settings.OAuthClientSecret)
		vals = append(vals, pg.String(*oauthSecret))
	}
	if _, err := table.Settings.UPDATE(cols).SET(vals[0], vals[1:]...).WHERE(table.Settings.ID.IS_TRUE()).ExecContext(ctx, tx); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	if _, err := table.Sites.DELETE().WHERE(pg.Bool(true)).ExecContext(ctx, tx); err != nil {
		return fmt.Errorf("clear sites: %w", err)
	}
	if len(c.Sites) > 0 {
		rows := make([]model.Sites, len(c.Sites))
		for i, x := range c.Sites {
			rows[i] = model.Sites{Name: x.Name, VersionURL: x.VersionURL}
		}
		if _, err := table.Sites.INSERT(table.Sites.AllColumns).MODELS(rows).ExecContext(ctx, tx); err != nil {
			return fmt.Errorf("insert sites: %w", err)
		}
	}
	if _, err := table.Services.DELETE().WHERE(pg.Bool(true)).ExecContext(ctx, tx); err != nil {
		return fmt.Errorf("clear services: %w", err)
	}
	if len(c.Services) > 0 {
		rows := make([]model.Services, len(c.Services))
		for i, x := range c.Services {
			rows[i] = model.Services{Name: x.Name, Postfix: x.Postfix, Kind: x.Kind, Sites: jsonList(x.Sites)}
		}
		if _, err := table.Services.INSERT(table.Services.AllColumns).MODELS(rows).ExecContext(ctx, tx); err != nil {
			return fmt.Errorf("insert services: %w", err)
		}
	}
	return tx.Commit()
}

// GetLogin returns the dashboard user and its password hash ("" = not set yet).
func (s *Store) GetLogin(ctx context.Context) (user, hash string, err error) {
	var row model.Settings
	err = pg.SELECT(table.Settings.AdminUser, table.Settings.AdminPasswordHash).FROM(table.Settings).QueryContext(ctx, s.db, &row)
	return row.AdminUser, row.AdminPasswordHash, err
}

func (s *Store) SetLogin(ctx context.Context, user, hash string) error {
	_, err := table.Settings.
		UPDATE(table.Settings.AdminUser, table.Settings.AdminPasswordHash, table.Settings.UpdatedAt).
		SET(pg.String(user), pg.String(hash), pg.NOW()).
		WHERE(table.Settings.ID.IS_TRUE()).
		ExecContext(ctx, s.db)
	return err
}

// ClearOAuth turns GitLab sign-in off, so the local password works again.
func (s *Store) ClearOAuth(ctx context.Context) error {
	_, err := table.Settings.
		UPDATE(table.Settings.OAuthClientID, table.Settings.UpdatedAt).
		SET(pg.String(""), pg.NOW()).
		WHERE(table.Settings.ID.IS_TRUE()).
		ExecContext(ctx, s.db)
	return err
}

// ===== SESSIONS =====

// AddSession stores a new session and drops expired ones.
func (s *Store) AddSession(ctx context.Context, row model.Sessions) error {
	if _, err := table.Sessions.DELETE().WHERE(table.Sessions.ExpiresAt.LT(pg.NOW())).ExecContext(ctx, s.db); err != nil {
		return err
	}
	_, err := table.Sessions.INSERT(table.Sessions.AllColumns.Except(table.Sessions.CreatedAt)).MODEL(row).ExecContext(ctx, s.db)
	return err
}

// GetSession returns a live session; qrm.ErrNoRows when missing or expired.
func (s *Store) GetSession(ctx context.Context, id string) (model.Sessions, error) {
	var row model.Sessions
	err := pg.SELECT(table.Sessions.AllColumns).
		FROM(table.Sessions).
		WHERE(table.Sessions.ID.EQ(pg.String(id)).AND(table.Sessions.ExpiresAt.GT(pg.NOW()))).
		QueryContext(ctx, s.db, &row)
	return row, err
}

// UpdateSessionToken saves a refreshed GitLab token and the re-checked role.
func (s *Store) UpdateSessionToken(ctx context.Context, id string, level int32, access, refresh string, expires time.Time) error {
	_, err := table.Sessions.
		UPDATE(table.Sessions.AccessLevel, table.Sessions.AccessToken, table.Sessions.RefreshToken, table.Sessions.TokenExpiresAt).
		SET(pg.Int32(level), pg.String(access), pg.String(refresh), pg.TimestampzT(expires)).
		WHERE(table.Sessions.ID.EQ(pg.String(id))).
		ExecContext(ctx, s.db)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := table.Sessions.DELETE().WHERE(table.Sessions.ID.EQ(pg.String(id))).ExecContext(ctx, s.db)
	return err
}
