#!/bin/bash
# run.sh <changelog> [goal] [extra mvn args...]
# Drives the end-to-end changelog against a PostgreSQL 17 on port 5559. Start one with:
#   docker run -d --name partitionctl-m4-integrate -e POSTGRES_PASSWORD=pw -p 5559:5432 postgres:17
# then load fixture.sql. MAVEN_OPTS=-Duser.timezone=UTC is required on macOS or the JDBC
# connect fails with: invalid value for parameter "TimeZone": "Asia/Calcutta".
BASE="$(cd "$(dirname "$0")" && pwd)"
CL="$1"; shift
GOAL="${1:-update}"; shift 2>/dev/null || true
cd "$BASE"
MAVEN_OPTS=-Duser.timezone=UTC mvn -B "liquibase:$GOAL" -Dchangelog="changelogs/$CL" \
  -Dmaven.repo.local="${PCTL_M2:-$HOME/.m2/repository}" "$@" 2>&1
exit ${PIPESTATUS[0]}
