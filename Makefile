# PartitionCTL
#
# `make` on its own prints the target list.
#
# The engine and adapters import no database/sql driver, which is what keeps the
# whole tree testable with in-memory fakes. So `make test` and `make check` need
# nothing installed, and the `db-*` and `demo-*` targets need `make driver` once.

SHELL := /bin/bash
.DEFAULT_GOAL := help
.PHONY: help build install test test-race cover fmt vet lint check clean \
        driver driver-status db-up db-down db-wait db-fixture db-psql db-reset \
        demo demo-plan demo-render demo-dry-run demo-execute demo-verify \
        demo-status demo-clean tree \
        lb-build lb-db-up lb-db-down lb-fixture lb-inspect lb-preview \
        lb-e2e lb-progress lb-adopter lb-clean

BIN        := build/partitionctl
PKG        := ./cmd/partitionctl

# lib/pq rather than pgx: pgx v5 requires Go 1.25 and this tree targets 1.22.
PQ_VERSION ?= v1.10.9

PG_CONTAINER ?= partitionctl-pg
PG_IMAGE     ?= postgres:17
PG_PORT      ?= 5432

# The Liquibase plugin is an independent product and gets its own container and port, so a
# `make lb-e2e` can never disturb a Go CLI demo running against $(PG_CONTAINER).
LB_DIR          ?= liquibase-partitionctl
LB_E2E          ?= docs/experiments/poc-trees/m4-e2e
LB_PG_CONTAINER ?= partitionctl-lb
LB_PG_PORT      ?= 5559
PG_PASSWORD  ?= pw
PG_DB        ?= postgres
PG_USER      ?= postgres

DSN ?= postgres://$(PG_USER):$(PG_PASSWORD)@localhost:$(PG_PORT)/$(PG_DB)?sslmode=disable

DRIVER    ?= postgres
SPEC      ?= examples/local/orders-idx.json
PLAN      ?= build/migration.plan
FIXTURE   ?= examples/local/fixture.sql
ACTOR     ?= $(USER)

# The demo uses the file state store so a reset is `rm -rf build/state` and so
# `demo-status` keeps working when the container is stopped (AC-25). Pass
# STATE=sql for the production-shaped path.
STATE     ?= file
STATE_DIR ?= build/state

CONN := -driver $(DRIVER) -state $(STATE) -state-dir $(STATE_DIR) -actor $(ACTOR)

##@ Development

help: ## Print this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nPartitionCTL\n\nusage: make <target>\n"} \
	     /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next} \
	     /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' \
	     $(MAKEFILE_LIST)
	@echo

build: ## Compile the binary to build/partitionctl
	@mkdir -p build
	go build -o $(BIN) $(PKG)
	@echo "built $(BIN)"

install: ## go install the binary onto your PATH
	go install $(PKG)

test: ## Run the whole suite (no database required)
	go test ./...

test-race: ## Run the suite under the race detector
	go test -race -count=1 ./...

cover: ## Write build/coverage.html and print the total
	@mkdir -p build
	go test -coverprofile=build/coverage.out ./...
	go tool cover -html=build/coverage.out -o build/coverage.html
	@go tool cover -func=build/coverage.out | tail -1
	@echo "report: build/coverage.html"

fmt: ## Rewrite unformatted files in place
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: ## Fail if anything is unformatted
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi
	@echo "gofmt: clean"

check: lint vet ## The release gate: gofmt, vet, build, uncached test (TRD §12)
	go build ./...
	go test -count=1 ./...
	@echo
	@echo "gofmt clean · vet clean · build clean · tests pass"

tree: ## Count files and tests
	@printf "go files:   %s\n" "$$(find . -name '*.go' -not -path './.*' | wc -l | tr -d ' ')"
	@printf "test funcs: %s\n" "$$(grep -rh '^func Test' --include='*_test.go' . | wc -l | tr -d ' ')"
	@printf "t.Skip:     %s\n" "$$(grep -rE '\bt\.Skip[f]?\(|\bt\.SkipNow\(' --include='*_test.go' . | wc -l | tr -d ' ')"

clean: ## Remove build artifacts and demo state
	rm -rf build
	@echo "removed build/"

##@ Driver

driver: ## Add lib/pq so the binary can actually connect (one-time)
	go get github.com/lib/pq@$(PQ_VERSION)
	@if ! grep -qE '^[[:space:]]*_ "github.com/lib/pq"' cmd/partitionctl/main.go; then \
	  echo; \
	  echo "Module added. Now register it by adding this import to cmd/partitionctl/main.go:"; \
	  echo; \
	  echo '    import _ "github.com/lib/pq" // registers "postgres"'; \
	  echo; \
	  echo "Nothing under engine/ or adapters/ may import a driver: that is what keeps"; \
	  echo "the tree testable offline. Only main registers one."; \
	  exit 1; \
	fi
	@$(MAKE) --no-print-directory driver-status

driver-status: ## Report whether a driver is registered in main
	@if grep -qE '^[[:space:]]*_ "github.com/lib/pq"' cmd/partitionctl/main.go; then \
	  echo "driver: lib/pq registered in cmd/partitionctl/main.go"; \
	else \
	  echo "driver: NONE registered. plan/execute/verify will refuse to connect."; \
	  echo "        run: make driver"; \
	fi

##@ Local database

db-up: ## Start a throwaway PostgreSQL container
	@docker start $(PG_CONTAINER) >/dev/null 2>&1 || \
	 docker run -d --name $(PG_CONTAINER) \
	   -e POSTGRES_PASSWORD=$(PG_PASSWORD) \
	   -p $(PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@$(MAKE) --no-print-directory db-wait
	@echo "postgres up on localhost:$(PG_PORT)"

db-wait: ## Block until the container accepts connections
	@for i in $$(seq 1 60); do \
	  docker exec $(PG_CONTAINER) pg_isready -U $(PG_USER) >/dev/null 2>&1 && exit 0; \
	  sleep 1; \
	done; \
	echo "postgres did not become ready in 60s"; exit 1

db-down: ## Stop and remove the container
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	@echo "removed $(PG_CONTAINER)"

db-fixture: ## Load the partitioned fixture table
	@docker exec -i $(PG_CONTAINER) psql -q -v ON_ERROR_STOP=1 \
	  -U $(PG_USER) -d $(PG_DB) < $(FIXTURE)

db-psql: ## Open psql against the container
	@docker exec -it $(PG_CONTAINER) psql -U $(PG_USER) -d $(PG_DB)

db-reset: db-down db-up db-fixture ## Recreate the container and reload the fixture

##@ Demo loop
#
# Go's flag package stops parsing at the first non-flag argument, so every flag
# must precede the <plan> positional. `execute <plan> -dry-run` silently passes
# -dry-run as a second positional and fails.

demo-plan: build ## Compile a plan from the example spec (read-only)
	@mkdir -p build
	PARTITIONCTL_DSN='$(DSN)' $(BIN) plan -spec $(SPEC) -o $(PLAN) -force $(CONN)

demo-render: build ## Print the SQL runbook (offline, no connection)
	@test -f $(PLAN) || { echo "no plan at $(PLAN) - run: make demo-plan"; exit 1; }
	@$(BIN) render $(PLAN)

demo-dry-run: build ## Live pre-flight: digest, fingerprint, lock, no DDL
	PARTITIONCTL_DSN='$(DSN)' $(BIN) execute -dry-run $(CONN) $(PLAN)

demo-execute: build ## Run the plan for real
	PARTITIONCTL_DSN='$(DSN)' $(BIN) execute $(CONN) $(PLAN)

demo-verify: build ## Assert the end state from the catalog
	PARTITIONCTL_DSN='$(DSN)' $(BIN) verify -end-state $(CONN) $(PLAN)

demo-status: build ## Show run state from the state store alone
	PARTITIONCTL_DSN='$(DSN)' $(BIN) status $(CONN)

demo: demo-plan demo-render demo-dry-run demo-execute demo-verify ## Whole loop end to end

demo-all: ## Full scripted walkthrough, every command echoed (add PAUSE=1 to step through)
	@examples/local/demo.sh $(if $(PAUSE),-p,)

demo-clean: ## Drop the demo plan and state, leaving the database alone
	rm -rf $(PLAN) $(STATE_DIR)
	@echo "removed $(PLAN) and $(STATE_DIR)"

##@ Liquibase plugin
#
# The plugin is a separate product from the Go CLI above: it shares no code, no SQL and no
# naming, and `make lb-e2e` never touches the Go targets or the $(PG_CONTAINER) database.
# It runs its own PostgreSQL on $(LB_PG_PORT) so a plugin run cannot disturb a CLI demo.

lb-build: ## mvn clean install the extension (always clean; Maven reuses stale classes)
	cd $(LB_DIR) && mvn -B clean install

lb-db-up: ## Start the plugin's own throwaway PostgreSQL on $(LB_PG_PORT)
	@docker start $(LB_PG_CONTAINER) >/dev/null 2>&1 || \
	 docker run -d --name $(LB_PG_CONTAINER) \
	   -e POSTGRES_PASSWORD=$(PG_PASSWORD) \
	   -p $(LB_PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 60); do \
	  docker exec $(LB_PG_CONTAINER) pg_isready -U $(PG_USER) >/dev/null 2>&1 && break; \
	  sleep 1; \
	done
	@echo "postgres up on localhost:$(LB_PG_PORT) as $(LB_PG_CONTAINER)"

lb-db-down: ## Stop and remove the plugin's PostgreSQL
	@docker rm -f $(LB_PG_CONTAINER) >/dev/null 2>&1 || true
	@echo "removed $(LB_PG_CONTAINER)"

lb-fixture: lb-db-up ## Load the 12-partition, 1.2M-row orders fixture
	@docker exec -i $(LB_PG_CONTAINER) psql -q -v ON_ERROR_STOP=1 \
	  -U $(PG_USER) -d $(PG_DB) < $(LB_E2E)/fixture.sql

lb-inspect: ## Read the whole tree straight from pg_catalog (never from Liquibase's log)
	@docker exec -i $(LB_PG_CONTAINER) psql -q -U $(PG_USER) -d $(PG_DB) < $(LB_E2E)/inspect.sql

lb-preview: ## liquibase updateSQL for the end-to-end changelog, no database changes
	@cd $(LB_E2E) && ./run.sh e2e.xml updateSQL >/dev/null
	@cat $(LB_E2E)/target/liquibase/migrate.sql

lb-e2e: lb-build lb-fixture ## create, gate, reindex, drop in one changelog, catalog-checked at each stage
	@$(LB_E2E)/e2e.sh

lb-progress: ## Just the create stage, timestamped, to judge the progress output format
	@docker exec -i $(LB_PG_CONTAINER) psql -q -v ON_ERROR_STOP=1 \
	  -U $(PG_USER) -d $(PG_DB) < $(LB_E2E)/fixture.sql
	@cd $(LB_E2E) && ./run.sh e2e.xml update -Dliquibase.toTag=created 2>&1 \
	  | perl -MTime::HiRes=time -ne 'BEGIN{$$|=1;$$s=time()} printf("%7.2f  %s", time()-$$s, $$_)' \
	  | grep -E 'partitionctl|Running Changeset|BUILD'

lb-adopter: lb-fixture ## Prove the PUBLISHED artifact: examples/liquibase resolved from JitPack, verified from pg_catalog
	@echo "== examples/liquibase: mvn liquibase:update, extension resolved from JitPack =="
	@cd examples/liquibase && MAVEN_OPTS=-Duser.timezone=UTC mvn -B liquibase:update
	@echo
	@echo "== verdict, read from pg_catalog rather than from the log above =="
	@docker exec -i $(LB_PG_CONTAINER) psql -q -U $(PG_USER) -d $(PG_DB) -c " \
	  WITH p AS (SELECT c.oid, i.indisvalid FROM pg_class c \
	               JOIN pg_index i ON i.indexrelid = c.oid \
	              WHERE c.relname = 'idx_orders_created' AND c.relkind = 'I') \
	  SELECT (SELECT indisvalid FROM p) AS parent_valid, \
	         (SELECT count(*) FROM pg_inherits WHERE inhparent = (SELECT oid FROM p)) AS attached, \
	         (SELECT count(*) FROM pg_inherits ii JOIN pg_index ix ON ix.indexrelid = ii.inhrelid \
	           WHERE ii.inhparent = (SELECT oid FROM p) AND ix.indisvalid) AS attached_and_valid, \
	         (SELECT count(*) FROM orders) AS rows_intact;"

lb-clean: lb-db-down ## Remove the plugin database and the harness build output
	@rm -rf $(LB_E2E)/target $(LB_DIR)/target examples/liquibase/target
	@echo "removed $(LB_E2E)/target, $(LB_DIR)/target and examples/liquibase/target"
