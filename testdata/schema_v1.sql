-- resonate's own schema. Applied at startup; every statement is idempotent.

CREATE TABLE IF NOT EXISTS deploys (
    id         bigserial   PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text        NOT NULL,
    branch     text        NOT NULL,
    sha        text        NOT NULL
);

-- One GitLab pipeline per site per deploy.
CREATE TABLE IF NOT EXISTS deploy_runs (
    id          bigserial   PRIMARY KEY,
    deploy_id   bigint      NOT NULL REFERENCES deploys (id) ON DELETE CASCADE,
    site        text        NOT NULL,
    pipeline_id bigint,
    web_url     text        NOT NULL DEFAULT '',
    status      text        NOT NULL,
    error       text        NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deploy_runs_deploy_id_idx ON deploy_runs (deploy_id);
CREATE INDEX IF NOT EXISTS deploy_runs_active_idx ON deploy_runs (id)
    WHERE status IN ('created', 'waiting_for_resource', 'preparing', 'pending', 'running', 'scheduled');

-- The commit + DB schema tracker: one row each time what a site runs changes.
CREATE TABLE IF NOT EXISTS site_versions (
    id          bigserial   PRIMARY KEY,
    site        text        NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    state       text        NOT NULL,
    commit_sha  text        NOT NULL DEFAULT '',
    schema_code text        NOT NULL DEFAULT '',
    schema_db   text        NOT NULL DEFAULT '',
    pending     jsonb       NOT NULL DEFAULT '[]',
    unknown     jsonb       NOT NULL DEFAULT '[]',
    error       text        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS site_versions_site_observed_idx ON site_versions (site, observed_at DESC);

-- Runtime configuration, edited from the dashboard. Exactly one row.
-- Only DATABASE_URL and GITLAB_TOKEN come from the environment.
CREATE TABLE IF NOT EXISTS settings (
    id                  boolean     PRIMARY KEY DEFAULT true CHECK (id),
    gitlab_url          text        NOT NULL DEFAULT '',
    gitlab_project      text        NOT NULL DEFAULT '',
    gitlab_branch       text        NOT NULL DEFAULT 'main',
    version_token       text        NOT NULL DEFAULT '',
    poll_seconds        integer     NOT NULL DEFAULT 60 CHECK (poll_seconds >= 10),
    admin_user          text        NOT NULL DEFAULT 'admin',
    admin_password_hash text        NOT NULL DEFAULT '',
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          text        NOT NULL DEFAULT ''
);
INSERT INTO settings (id) VALUES (true) ON CONFLICT DO NOTHING;

-- GitLab sign-in (an OAuth application on the GitLab instance). While either
-- is empty, the local admin password is the only way in.
ALTER TABLE settings ADD COLUMN IF NOT EXISTS oauth_client_id     text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN IF NOT EXISTS oauth_client_secret text NOT NULL DEFAULT '';

-- Signed-in browsers. id is sha256(cookie); user_id 0 is the local password
-- login. access_token is the user's own GitLab token, used to trigger deploys.
CREATE TABLE IF NOT EXISTS sessions (
    id               text        PRIMARY KEY,
    user_id          bigint      NOT NULL,
    username         text        NOT NULL,
    name             text        NOT NULL DEFAULT '',
    avatar_url       text        NOT NULL DEFAULT '',
    access_level     integer     NOT NULL,
    access_token     text        NOT NULL DEFAULT '',
    refresh_token    text        NOT NULL DEFAULT '',
    token_expires_at timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions (expires_at);

-- The fleet. name is the GitLab environment, passed to CI as $SITE.
CREATE TABLE IF NOT EXISTS sites (
    name        text PRIMARY KEY,
    version_url text NOT NULL
);
