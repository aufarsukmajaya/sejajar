# Resonate

*Resonate* keeps every site on the same code and DB schema as `main`: each service is a gem that resonates when it runs what `main` runs.

- **Fleet matrix** (`serve`): one repo, many sites, each running many services
  (APIs, workers, jobs). Each site is a mountain and each service is a gem on
  it (sapphire for APIs, emerald for workers, amethyst for jobs):
  - at `main`'s head it vibrates and gives off white light;
  - it loses colour with each commit it falls behind;
  - more than 10 commits behind (`DRIFT` in `web/index.html`), it sways and flickers;
  - when the service is down, it drops from its socket to the floor;
  - pending or unknown DB migrations make it pulse amber or red.

  The **Pixel / Ink** switch above the matrix changes the style: a pixel-art sprite
  range, or ink-brush ensō circles on misty mountains.
  Each viewer's choice is saved in their own browser.

  Select gems, services or whole mountains and deploy them at the branch head;
  watch each GitLab pipeline finish.
- **Schema tracker**: every change in what a cell runs (commit, state,
  applied/pending/unknown migrations) is stored in Postgres. Open a cell to
  see its history. Checks only run while someone has the dashboard open (all
  open tabs share one check), so the history has gaps when nobody is looking.
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
services, how deploy pipelines are triggered, how often to re-check, the version API
token and GitLab sign-in. Changes apply without a restart.

```bash
cp .env.example .env        # set GITLAB_TOKEN; DATABASE_URL defaults to the dev postgres container
make run                    # dashboard on http://localhost:8090
```

The database must exist (`docker exec postgres psql -U postgres -c "CREATE DATABASE resonate"`);
the tables are created at startup.

## How a cell is read and deployed

| | APIs | Workers, jobs |
|---|---|---|
| What runs | the API's own `/version` (live; see `VERSION_API.md`), including DB migrations | the last successful **GitLab deployment** to the cell's environment |
| Where | the site's version URL template, e.g. `https://{site}.example.com/{service}{postfix}/version` | the environment template, e.g. `production-{site}-{service}{postfix}` |

An API on a site with no version URL is read from GitLab deployments too.
APIs often record a different environment (a `-stable` suffix with canaries), so
there is a separate API environment template.

### Is it actually running?

A deployment record says what was deployed, not whether it still runs. For
cells read from GitLab (workers, and APIs without a version URL), Resonate
also checks:

- **Pods**, through the [GitLab agent for Kubernetes](https://docs.gitlab.com/user/clusters/agent/).
  Give each site the ID of the agent in its cluster (Settings → Sites → Agent ID),
  and set the pod namespace and label selector templates, e.g. `production` and
  `app={service}{postfix}`. When none of a cell's pods are ready, its gem falls
  to the floor. Jobs finish by design and are never checked. No cluster
  credentials are needed: calls go through GitLab's Kubernetes proxy (KAS) as
  `pat:<agent id>:<GITLAB_TOKEN>`, so Resonate can run in any cluster. For this:
  - `GITLAB_TOKEN` needs the `k8s_proxy` scope as well as `api`;
  - each agent's config (`.gitlab/agents/<agent>/config.yaml`) grants the app
    project user access, and the token's user must be a Developer or above on it:
    ```yaml
    user_access:
      access_as:
        agent: {}
      projects:
        - id: group/app
    ```
  - the proxy is `https://kas.gitlab.com/k8s-proxy` on GitLab.com and
    `<gitlab>/-/kubernetes-agent/k8s-proxy` when self-managed. Override it in
    Settings if your KAS lives elsewhere.

  If the agent can't be reached, the cell keeps what its deployment says and
  the side panel shows the error.
- **Stopped environments.** A cell whose GitLab environment is stopped is down.
- **Failed deploys.** When the environment's latest deployment failed or was
  canceled, the cell still runs the one before it; its gem gets a red dot and
  the side panel links the failed pipeline.

A **postfix** is a separately deployed variant of a service (`api-core` +
`-partner` is its own deployment and environment). It is its own row, and it
can be limited to the sites that run it.

**Deploying** a cell creates one pipeline on the branch with the configured
pipeline inputs, for example `SITE={site}`, `SERVICE={service}`,
`POSTFIX={postfix}`, `DEPLOY_SHA={sha}`, `DEPLOY_ENV=production`. They are sent
as `spec:inputs` (GitLab 17.10+) or as CI variables. Settings refuses templates
that can't tell two cells apart (no `{postfix}` while variants exist, or a
version URL without `{service}`).

A pipeline always runs the branch head. Resonate refuses the deploy if `main`
moved since you looked. Passing `{sha}` lets the CI build the confirmed commit
(see `gitlab-ci.example.yml`), so a large deploy stays on one commit even if
`main` moves while the pipelines are being created. Without it, Resonate flags
any pipeline that GitLab started on a newer commit.

GitLab records a deployment against the pipeline's own commit (the branch head
when it started), not `DEPLOY_SHA`. So when `main` moves during a pinned
deploy, Resonate shows workers and jobs at the commit it pinned, which it
remembers for every pipeline it started. A pipeline started outside Resonate
reads as GitLab recorded it. APIs are unaffected, because their `/version`
reports the real build.

**Jobs** are one-off. "Select out of date" and row or site selection skip them;
select them cell by cell (or name them with `-services`).

## Sign-in

1. **First run.** The log prints a random password for `admin`. Sign in with it
   and set the GitLab URL, project and sites in Settings.
2. **Turn on GitLab sign-in.** In GitLab, create an application (Admin Area →
   Applications, or the group's Settings → Applications) with:
   - redirect URI: the one shown in Settings (`https://<resonate-host>/auth/callback`)
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
   `./bin/resonate reset-password`. It prints a new admin password and turns
   GitLab sign-in off until the Application ID is set again.

Behind a TLS proxy, pass `X-Forwarded-Proto: https`, so that cookies are marked
Secure and the redirect URI uses https. If your GitLab uses an internal CA,
add it to the system trust store, or set `SSL_CERT_FILE` in the container.

To load the configuration from a file instead of typing it in, run
`./bin/resonate import sites.json`. The format is in `sites.example.json`, and
the import replaces the sites and services.

The terminal commands read the same database:

```bash
./bin/resonate status                                    # one line per cell; exits 1 if any is not OK
./bin/resonate -sites site-a -services api-orders status
./bin/resonate -sites site-a,site-b deploy               # dry run: every non-job service there
./bin/resonate -services job-backfill -yes deploy        # a job, on every site that runs it
```

Container image: `docker build -t resonate .` and pass the two env vars. For Kubernetes, edit the `CHANGE ME` lines in `deployment.yaml` and `kubectl apply -f deployment.yaml`.

## Demo

```bash
make demo   # own database (resonate_demo), fake GitLab, 5 sites x 6 services, http://127.0.0.1:8090
curl -X POST localhost:9999/mock/push                  # merge a commit (with a new migration) to main
curl -X POST localhost:9999/mock/sites/makassar/toggle  # take a site's DB down / bring it back
curl -X POST localhost:9999/mock/pods/jakarta/worker-sync/toggle  # make a worker's pods crash-loop / recover
```

The demo starts with three workers that look fine by deployment alone:
makassar's `worker-mailer` crash-loops (0 of 2 pods ready), its `worker-sync`
environment is stopped, and surabaya's `worker-mailer` failed to deploy the head.

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
| `internal/mock`, `cmd/resonate-mock` | Fake GitLab (API + OAuth) and fake sites for the demo and the end-to-end test |
| `VERSION_API.md` | The contract each API must serve at `/version` |
| `gitlab-ci.example.yml` | A deploy pipeline resonate can drive: inputs, environment naming, migrate then roll out |

`make test` runs the unit tests plus an end-to-end test (API → mock GitLab → Postgres)
against the `resonate_test` database.
