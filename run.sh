#!/bin/sh
set -eu

API_KEY="${1:-mem9-default-key}"
TENANT_NAME="${2:-Default Space}"
INGEST_MODE="${3:-raw}"
LLM_API_KEY="${4:-}"
LLM_BASE_URL="${5:-}"
LLM_MODEL="${6:-gpt-4o-mini}"
MYSQL_PASSWORD="${7:-}"

mkdir -p /lzcapp/var/uploads
export MNEMO_PORT=8080
export MNEMO_DB_BACKEND=tidb
export MNEMO_DB_FLAVOR=mysql
export MNEMO_DSN="mnemos:${MYSQL_PASSWORD}@tcp(mysql:3306)/mnemos?parseTime=true"
export MNEMO_TIDB_ZERO_ENABLED=false
export MNEMO_FTS_ENABLED=false
export MNEMO_INGEST_MODE="${INGEST_MODE}"
export MNEMO_UPLOAD_DIR=/lzcapp/var/uploads
export MNEMO_WORKER_CONCURRENCY=2
export MNEMO_DB_CONNECT_MAX_RETRIES=180
export MNEMO_DB_CONNECT_RETRY_DELAY=2s
export MNEMO_LLM_API_KEY="${LLM_API_KEY}"
export MNEMO_LLM_BASE_URL="${LLM_BASE_URL}"
export MNEMO_LLM_MODEL="${LLM_MODEL}"
export MNEMO_BOOTSTRAP_TENANT_ENABLED=true
export MNEMO_BOOTSTRAP_TENANT_ID="${API_KEY}"
export MNEMO_BOOTSTRAP_TENANT_NAME="${TENANT_NAME}"
export MNEMO_BOOTSTRAP_TENANT_DB_HOST=mysql
export MNEMO_BOOTSTRAP_TENANT_DB_PORT=3306
export MNEMO_BOOTSTRAP_TENANT_DB_USER=mnemos
export MNEMO_BOOTSTRAP_TENANT_DB_PASS="${MYSQL_PASSWORD}"
export MNEMO_BOOTSTRAP_TENANT_DB_NAME=mnemos
export MNEMO_BOOTSTRAP_TENANT_DB_TLS=false
exec /lzcapp/pkg/content/mnemo-server
