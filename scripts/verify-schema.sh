#!/usr/bin/env bash
# Applies every migration to a scratch database and runs the schema regression
# checks against it.
#
# Usage: scripts/verify-schema.sh [psql-connection-flags...]
#   PGDATABASE defaults to lumi_schema_verify and is dropped and recreated.
set -euo pipefail

cd "$(dirname "$0")/.."

DB="${LUMI_VERIFY_DB:-lumi_schema_verify}"
PSQL=(psql -v ON_ERROR_STOP=1 -q "$@")

"${PSQL[@]}" -d postgres -c "DROP DATABASE IF EXISTS ${DB}" -c "CREATE DATABASE ${DB}" >/dev/null

for migration in migrations/*.sql; do
    echo "applying ${migration}"
    "${PSQL[@]}" -d "${DB}" -f "${migration}" >/dev/null
done

echo "running checks"
"${PSQL[@]}" -d "${DB}" -f scripts/verify-schema.sql
