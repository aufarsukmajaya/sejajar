# Version API spec

Every API service exposes this endpoint; resonate polls it on each site, at the
site's version URL template (`{site}`, `{service}`, `{postfix}` filled in).
Workers and jobs don't need it: resonate reads their last successful GitLab
deployment instead.

## Request

```
GET /version
Authorization: Bearer <VERSION_TOKEN>   (optional, see Security)
```

## Response `200 application/json`

```json
{
  "commit": "3f1c9e0d2b7a4c5e8f9a0b1c2d3e4f5a6b7c8d9e",
  "built_at": "2026-10-08T03:12:00Z",
  "schema": {
    "code": "M20260913120000_job_runs",
    "db": "M20260913120000_job_runs",
    "pending": [],
    "unknown": []
  }
}
```

| Field | Required | Meaning |
|---|---|---|
| `commit` | yes | The full 40-char git SHA the binary was built from. Inject it at build time: `go build -ldflags "-X main.commit=$CI_COMMIT_SHA"`. **Never** read it at runtime from a file, env var, or git. |
| `built_at` | no | The build time, in RFC 3339 UTC. Informational only. |
| `schema.code` | yes | The ID of the **last** migration compiled into this binary, in the migration list's own order (do not sort the IDs as strings, because prefixes can differ in case, as in `M2026…` vs `m2026…`). |
| `schema.db` | yes | The ID of the last migration from the code's list that is applied in the DB (with gormigrate, the `migrations.id` rows). The value is `""` if none are applied. |
| `schema.pending` | yes | The IDs in the code's list that are **not** in the DB table, in code order. Use `[]` when there are none, not `null`. |
| `schema.unknown` | yes | The IDs in the DB table that are **not** in the code's list. A non-empty list means a newer build migrated this DB and the running build is older (a rollback onto a forward schema). Use `[]` when there are none. |

Compute `pending` and `unknown` as set differences between the compiled
migration list and `SELECT id FROM migrations`, rather than by comparing the
"latest" ID. That keeps the result correct when a migration ID is out of order or
differs only by case.

## Behavior

- The endpoint must answer even while migrations are pending. Do not gate it
  behind the readiness probe.
- If the DB can't be reached, return `503` with `{"commit": "...", "error": "db unreachable"}`.
  Resonate shows the cell as DOWN.
- Run one indexed `SELECT` per call. A short cache (≤ 30s) is fine.

## Security

The response reveals the commit and migration names. Serve it only on an
internal ingress, or require a static bearer token (`VERSION_TOKEN`, the same
one on every site, entered in resonate's Settings and kept in GitLab CI/CD variables as masked + protected). Never
include connection strings, hostnames or env values in the response.

## Example (Go, gormigrate)

```go
var commit = "dev" // set by -ldflags

func version(db *gorm.DB, list []*gormigrate.Migration) (map[string]any, error) {
	var applied []string
	if err := db.Table("migrations").Pluck("id", &applied).Error; err != nil {
		return nil, err
	}
	inDB := map[string]bool{}
	for _, id := range applied {
		inDB[id] = true
	}
	inCode := map[string]bool{}
	pending, unknown, last := []string{}, []string{}, ""
	for _, m := range list {
		inCode[m.ID] = true
		if inDB[m.ID] {
			last = m.ID
		} else {
			pending = append(pending, m.ID)
		}
	}
	for _, id := range applied {
		if !inCode[id] {
			unknown = append(unknown, id)
		}
	}
	code := ""
	if len(list) > 0 {
		code = list[len(list)-1].ID
	}
	return map[string]any{"commit": commit, "schema": map[string]any{
		"code": code, "db": last, "pending": pending, "unknown": unknown,
	}}, nil
}
```
