# The only two env vars. Put them in .env (KEY=value, no quotes) or export them.
-include .env
DATABASE_URL ?= postgres://postgres:mysecretpassword@127.0.0.1:5432/sejajar?sslmode=disable
export DATABASE_URL GITLAB_TOKEN

TEST_DSN ?= postgres://postgres:mysecretpassword@127.0.0.1:5432/sejajar_test?sslmode=disable
DEMO_DSN ?= postgres://postgres:mysecretpassword@127.0.0.1:5432/sejajar_demo?sslmode=disable
ADDR     ?= :8090

build:
	go build -o bin/sejajar . && go build -o bin/sejajar-mock ./cmd/sejajar-mock

# Dashboard on $(ADDR). Everything else is configured in the dashboard's Settings.
run: build
	./bin/sejajar -addr '$(ADDR)' serve

test:
	SEJAJAR_TEST_DATABASE_URL='$(TEST_DSN)' go test -count=1 ./...

# Regenerate jet/ after changing schema.sql (apply it first: run the server once, or psql -f schema.sql).
gen-jet:
	jet -dsn='$(DATABASE_URL)' -schema=public -path=./jet

# Local demo on its own database: fake GitLab (with sign-in) + 5 fake sites on :9999, dashboard on :8090.
demo: build
	docker exec postgres psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'sejajar_demo'" | grep -q 1 || \
		docker exec postgres psql -U postgres -c "CREATE DATABASE sejajar_demo"
	./bin/sejajar-mock & trap 'kill $$!' EXIT; \
	export DATABASE_URL='$(DEMO_DSN)' GITLAB_TOKEN=demo; \
	./bin/sejajar reset-password demo-pass && ./bin/sejajar import sites.demo.json && \
	./bin/sejajar -addr 127.0.0.1:8090 serve

.PHONY: build run test gen-jet demo
