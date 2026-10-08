# Sejajar

*Sejajar* (Indonesian: "aligned") keeps every site on the same code and DB schema as `main`.

- **Fleet matrix** (`serve`): one repo, many sites, each running many services
  (APIs, workers, jobs). Services are rows, sites are columns. Each cell shows
  the commit it runs, how far that is behind `main`, and for APIs the DB
  migration state. Select cells, rows or whole sites and deploy them at the
  branch head; watch each GitLab pipeline finish.
- **Schema tracker**: every change in what a cell runs (commit, state,
  applied/pending/unknown migrations) is stored in Postgres. Open a cell to
  see its history.
- **CLI** (`status`, `deploy`): the same checks from a terminal. `status` exits 1
  when any cell is not OK, so CI can use it as a gate.
- **GitLab sign-in**: people sign in with your (self-hosted) GitLab. Their role
  on the project decides what they can do, and deploys run as them.

One Go binary that serves the API and an embedded single-page UI. Storage is
Postgres through [go-jet](https://github.com/go-jet/jet).

## Run

The environment holds only two values: `DATABASE_URL` and `GITLAB_TOKEN` (api scope
on the app project). Everything else lives in the database and is edited from
the dashboard's **Settings**: the GitLab URL, project and branch, the sites and
services, how deploy pipelines are triggered, the poll interval, the version API
token and GitLab sign-in. Changes apply without a restart.

```bash
cp .env.example .env        # set GITLAB_TOKEN; DATABASE_URL defaults to the dev postgres container
make run                    # dashboard on http://localhost:8090
```

The database must exist (`docker exec postgres psql -U postgres -c "CREATE DATABASE sejajar"`);
the tables are created at startup.

## How a cell is read and deployed

| | APIs | Workers, jobs |
|---|---|---|
| What runs | the API's own `/version` (live; see `VERSION_API.md`), including DB migrations | the last successful **GitLab deployment** to the cell's environment |
| Where | the site's version URL template, e.g. `https://{site}.example.com/{service}{postfix}/version` | the environment template, e.g. `production-{site}-{service}{postfix}` |

An API on a site with no version URL is read from GitLab deployments too.
APIs often record a different environment (a `-stable` suffix with canaries), so
there is a separate API environment template.

A **postfix** is a separately deployed variant of a service (`api-core` +
`-partner` is its own deployment and environment). It is its own row, and it
can be limited to the sites that run it.

**Deploying** a cell creates one pipeline on the branch with the configured
pipeline inputs, for example `SITE={site}`, `SERVICE={service}`,
`POSTFIX={postfix}`, `DEPLOY_ENV=production`. They are sent as `spec:inputs`
(GitLab 17.10+) or as CI variables. A pipeline builds the branch, not a SHA.
Sejajar refuses the deploy if `main` moved since you looked, and flags any
pipeline that GitLab started on a newer commit.

**Jobs** are one-off. "Select out of date" and row or site selection skip them;
select them cell by cell (or name them with `-services`).

## Sign-in

1. **First run.** The log prints a random password for `admin`. Sign in with it
   and set the GitLab URL, project and sites in Settings.
2. **Turn on GitLab sign-in.** In GitLab, create an application (Admin Area →
   Applications, or the group's Settings → Applications) with:
   - redirect URI: the one shown in Settings (`https://<sejajar-host>/auth/callback`)
   - scope: `api`, because deploys are triggered with the signed-in user's token
   - confidential: yes

   Paste its Application ID and Secret into Settings. From then on only GitLab
   sign-in works, and the setup password is retired.
3. **Who can do what** follows each person's access level on the GitLab project
   (inherited group roles count):

   | Role | Can |
   |---|---|
   | Reporter | view status, history and deploys |
   | Developer | deploy (GitLab still applies protected branch and environment rules) |
   | Maintainer, Owner, instance admin | also change Settings |

   The pipeline shows the real person as its creator. The role is re-checked
   each time the GitLab token refreshes (about every 2 hours).
4. **Break-glass.** If GitLab sign-in is misconfigured or GitLab is down, run
   `./bin/sejajar reset-password`. It prints a new admin password and turns
   GitLab sign-in off until the Application ID is set again.

Behind a TLS proxy, pass `X-Forwarded-Proto: https`, so that cookies are marked
Secure and the redirect URI uses https. If your GitLab uses an internal CA,
add it to the system trust store, or set `SSL_CERT_FILE` in the container.

To load the configuration from a file instead of typing it in, run
`./bin/sejajar import sites.json`. The format is in `sites.example.json`, and
the import replaces the sites and services.

The terminal commands read the same database:

```bash
./bin/sejajar status                                    # one line per cell; exits 1 if any is not OK
./bin/sejajar -sites site-a -services api-orders status
./bin/sejajar -sites site-a,site-b deploy               # dry run: every non-job service there
./bin/sejajar -services job-backfill -yes deploy        # a job, on every site that runs it
```

Container image: `docker build -t sejajar .` and pass the two env vars.

## Demo

```bash
make demo   # own database (sejajar_demo), fake GitLab, 5 sites x 6 services, http://127.0.0.1:8090
curl -X POST localhost:9999/mock/push                  # merge a commit (with a new migration) to main
curl -X POST localhost:9999/mock/sites/makassar/toggle  # take a site's DB down / bring it back
```

The demo has GitLab sign-in on. The fake GitLab's consent page lets you pick a
user: `dewi` (Maintainer), `budi` (Developer), `tamu` (Reporter: GitLab refuses
their pipelines) or `luar` (not a member: sign-in refused).

## HTTP API (session cookie)

| Method | Path | What it does |
|---|---|---|
| GET | `/api/status` | Snapshot: branch head, sites, services and every cell's state. Records changes. |
| GET | `/api/deploys` | The last 30 deploys with each pipeline's status. Refreshes in-flight pipelines from GitLab. |
| POST | `/api/deploys` | `{"targets": [{"site", "service", "postfix"}], "sha": "<head>"}` triggers one pipeline per target. Returns 409 if main moved. |
| GET | `/api/versions?site=&service=&postfix=` | One cell's version history, newest first. |
| GET | `/api/settings` | Maintainers only. Settings, sites and services. Secrets are never returned, only `*_set` flags. |
| PUT | `/api/settings` | Maintainers only. Replace settings, sites and services. Omit `version_token` or `oauth_secret` to keep it. |
| GET | `/api/me` | Who is signed in and whether they can change settings. |
| GET | `/auth/gitlab`, `/auth/callback` | GitLab OAuth sign-in (PKCE). |
| POST | `/auth/password` | Setup sign-in `{"user", "password"}`; refused once GitLab sign-in is on. |
| POST | `/auth/logout` | End the session. |
| GET | `/healthz` | Liveness, no auth. |

## States

| State | Meaning |
|---|---|
| `OK` | Running the `main` head with no pending migrations. |
| `BEHIND` | Running an older commit. Deploy to fix. |
| `SCHEMA_PENDING` | The code has migrations the DB lacks, so the migrate step failed or was skipped. |
| `DB_AHEAD` | The DB has migrations the running code doesn't know. A newer build migrated it and was rolled back. Investigate before you deploy. |
| `DOWN` | An API's `/version` couldn't be reached or returned an error. |
| `NEVER` | GitLab has no successful deployment to the cell's environment. |
| `UNKNOWN` | GitLab couldn't be asked about the cell's environment. |

## Files

| Path | What |
|---|---|
| `main.go` | Flags and subcommands (`serve`, `status`, `deploy`, `import`, `reset-password`) |
| `fleet.go` | The config model (sites x services), snapshot, classification, deploy fan-out, pipeline sync |
| `gitlab.go` | The GitLab API calls used |
| `store.go`, `schema.sql`, `jet/` | Postgres storage. `jet/` is generated (`make gen-jet`), so don't hand-edit it |
| `auth.go` | GitLab OAuth, sessions, roles, setup password (PBKDF2) |
| `server.go`, `web/` | HTTP API, embedded dashboard (Cormorant Garamond, OFL, in `web/fonts`) |
| `internal/mock`, `cmd/sejajar-mock` | Fake GitLab (API + OAuth) and fake sites for the demo and the end-to-end test |
| `VERSION_API.md` | The contract each API must serve at `/version` |
| `gitlab-ci.example.yml` | A deploy pipeline sejajar can drive: inputs, environment naming, migrate then roll out |

`make test` runs the unit tests plus an end-to-end test (API → mock GitLab → Postgres)
against the `sejajar_test` database.
