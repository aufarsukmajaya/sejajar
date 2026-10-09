-- resonate's own schema. Applied at startup; every statement is idempotent.

CREATE TABLE IF NOT EXISTS deploys (
    id         bigserial   PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text        NOT NULL,
    branch     text        NOT NULL,
    sha        text        NOT NULL
);

-- One GitLab pipeline per (site, service) per deploy.
CREATE TABLE IF NOT EXISTS deploy_runs (
    id           bigserial   PRIMARY KEY,
    deploy_id    bigint      NOT NULL REFERENCES deploys (id) ON DELETE CASCADE,
    site         text        NOT NULL,
    service      text        NOT NULL,
    postfix      text        NOT NULL DEFAULT '',
    pipeline_id  bigint,
    pipeline_sha text        NOT NULL DEFAULT '', -- what GitLab actually built
    web_url      text        NOT NULL DEFAULT '',
    status       text        NOT NULL,
    error        text        NOT NULL DEFAULT '',
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deploy_runs_deploy_id_idx ON deploy_runs (deploy_id);
CREATE INDEX IF NOT EXISTS deploy_runs_active_idx ON deploy_runs (id)
    WHERE status IN ('created', 'waiting_for_resource', 'preparing', 'pending', 'running', 'scheduled');

-- The commit + DB schema tracker: one row each time what a (site, service)
-- runs changes. source is "version" (the service's /version) or "gitlab"
-- (its environment's last successful deployment).
CREATE TABLE IF NOT EXISTS site_versions (
    id          bigserial   PRIMARY KEY,
    site        text        NOT NULL,
    service     text        NOT NULL,
    postfix     text        NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL DEFAULT now(),
    state       text        NOT NULL,
    source      text        NOT NULL DEFAULT '',
    commit_sha  text        NOT NULL DEFAULT '',
    schema_code text        NOT NULL DEFAULT '',
    schema_db   text        NOT NULL DEFAULT '',
    pending     jsonb       NOT NULL DEFAULT '[]',
    unknown     jsonb       NOT NULL DEFAULT '[]',
    error       text        NOT NULL DEFAULT ''
);

-- Runtime configuration, edited from the dashboard. Exactly one row.
-- Only DATABASE_URL and GITLAB_TOKEN come from the environment.
CREATE TABLE IF NOT EXISTS settings (
    id                       boolean     PRIMARY KEY DEFAULT true CHECK (id),
    gitlab_url               text        NOT NULL DEFAULT '',
    gitlab_project           text        NOT NULL DEFAULT '',
    gitlab_branch            text        NOT NULL DEFAULT 'main',
    -- GitLab environment each deploy job records; {site} {service} {postfix}.
    -- APIs may use their own (e.g. a "-stable" suffix); '' = same as the rest.
    environment_template     text        NOT NULL DEFAULT '{site}-{service}{postfix}',
    api_environment_template text        NOT NULL DEFAULT '',
    -- What a deploy pipeline is given, as spec:inputs or as CI variables.
    pipeline_inputs          jsonb       NOT NULL DEFAULT '{"SITE": "{site}", "SERVICE": "{service}", "POSTFIX": "{postfix}", "DEPLOY_SHA": "{sha}"}',
    trigger_as               text        NOT NULL DEFAULT 'inputs' CHECK (trigger_as IN ('inputs', 'variables')),
    version_token            text        NOT NULL DEFAULT '',
    poll_seconds             integer     NOT NULL DEFAULT 60 CHECK (poll_seconds >= 10),
    admin_user               text        NOT NULL DEFAULT 'admin',
    admin_password_hash      text        NOT NULL DEFAULT '',
    -- GitLab sign-in (an OAuth application on the GitLab instance). While
    -- either is empty, the local admin password is the only way in.
    oauth_client_id          text        NOT NULL DEFAULT '',
    oauth_client_secret      text        NOT NULL DEFAULT '',
    updated_at               timestamptz NOT NULL DEFAULT now(),
    updated_by               text        NOT NULL DEFAULT ''
);
INSERT INTO settings (id) VALUES (true) ON CONFLICT DO NOTHING;

-- The fleet's columns. version_url is a template ({site} {service} {postfix})
-- for the APIs' /version; '' when the site's APIs don't expose one.
CREATE TABLE IF NOT EXISTS sites (
    name        text PRIMARY KEY,
    version_url text NOT NULL DEFAULT ''
);

-- The fleet's rows. A postfix is a separately deployed variant of the same
-- service (name + postfix is its deployment). sites = [] means every site.
CREATE TABLE IF NOT EXISTS services (
    name    text  NOT NULL,
    postfix text  NOT NULL DEFAULT '',
    kind    text  NOT NULL CHECK (kind IN ('api', 'worker', 'job')),
    sites   jsonb NOT NULL DEFAULT '[]',
    PRIMARY KEY (name, postfix)
);

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

-- Upgrades for databases created by earlier versions (CREATE TABLE IF NOT
-- EXISTS leaves an existing table as it was). Keep every new column here too.
ALTER TABLE deploy_runs   ADD COLUMN IF NOT EXISTS service                  text  NOT NULL DEFAULT '';
ALTER TABLE deploy_runs   ADD COLUMN IF NOT EXISTS postfix                  text  NOT NULL DEFAULT '';
ALTER TABLE deploy_runs   ADD COLUMN IF NOT EXISTS pipeline_sha             text  NOT NULL DEFAULT '';
ALTER TABLE site_versions ADD COLUMN IF NOT EXISTS service                  text  NOT NULL DEFAULT '';
ALTER TABLE site_versions ADD COLUMN IF NOT EXISTS postfix                  text  NOT NULL DEFAULT '';
ALTER TABLE site_versions ADD COLUMN IF NOT EXISTS source                   text  NOT NULL DEFAULT '';
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS environment_template     text  NOT NULL DEFAULT '{site}-{service}{postfix}';
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS api_environment_template text  NOT NULL DEFAULT '';
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS pipeline_inputs          jsonb NOT NULL DEFAULT '{"SITE": "{site}", "SERVICE": "{service}", "POSTFIX": "{postfix}", "DEPLOY_SHA": "{sha}"}';
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS trigger_as               text  NOT NULL DEFAULT 'inputs' CHECK (trigger_as IN ('inputs', 'variables'));
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS oauth_client_id          text  NOT NULL DEFAULT '';
ALTER TABLE settings      ADD COLUMN IF NOT EXISTS oauth_client_secret      text  NOT NULL DEFAULT '';
ALTER TABLE sites         ALTER COLUMN version_url SET DEFAULT '';
DROP INDEX IF EXISTS site_versions_site_observed_idx; -- superseded by the per-cell index
CREATE INDEX IF NOT EXISTS site_versions_cell_observed_idx ON site_versions (site, service, postfix, observed_at DESC);
