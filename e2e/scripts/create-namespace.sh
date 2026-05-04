#!/bin/sh
# Register the default Temporal namespace for the e2e stack.
#
# Adapted from
# https://github.com/temporalio/samples-server/blob/main/compose/scripts/create-namespace.sh
# (run by the temporal-create-namespace admin-tools service after the temporal
# server is healthy and before the watcher/worker services start).
set -eu

NAMESPACE=${DEFAULT_NAMESPACE:-default}
TEMPORAL_ADDRESS=${TEMPORAL_ADDRESS:-temporal:7233}
MAX_ATTEMPTS=${TEMPORAL_HEALTH_CHECK_MAX_ATTEMPTS:-30}
SLEEP_SECONDS=${TEMPORAL_HEALTH_CHECK_SLEEP_SECONDS:-5}

echo "Waiting for Temporal server port to be available..."
SERVER_HOST=$(echo "$TEMPORAL_ADDRESS" | cut -d: -f1)
SERVER_PORT=$(echo "$TEMPORAL_ADDRESS" | cut -d: -f2)
attempt=1
while ! nc -z -w 10 "$SERVER_HOST" "$SERVER_PORT"; do
  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "Temporal server port did not become available after $MAX_ATTEMPTS attempts"
    exit 1
  fi

  echo "Temporal server port not ready yet, waiting... (attempt $attempt/$MAX_ATTEMPTS)"
  attempt=$((attempt + 1))
  sleep "$SLEEP_SECONDS"
done
echo "Temporal server port is available"

echo "Waiting for Temporal server to be healthy..."
attempt=1
while :; do
  if temporal operator cluster health --address "$TEMPORAL_ADDRESS"; then
    break
  fi

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "Server did not become healthy after $MAX_ATTEMPTS attempts"
    exit 1
  fi

  echo "Server not ready yet, waiting... (attempt $attempt/$MAX_ATTEMPTS)"
  attempt=$((attempt + 1))
  sleep "$SLEEP_SECONDS"
done

echo "Server is healthy, creating namespace '$NAMESPACE'..."

attempt=1
while :; do
  if temporal operator namespace describe -n "$NAMESPACE" --address "$TEMPORAL_ADDRESS" >/dev/null 2>&1; then
    echo "Namespace '$NAMESPACE' already exists"
    break
  fi

  if temporal operator namespace create -n "$NAMESPACE" --address "$TEMPORAL_ADDRESS" >/dev/null 2>&1; then
    echo "Namespace '$NAMESPACE' created"
    break
  fi

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "Failed to create namespace '$NAMESPACE' after $MAX_ATTEMPTS attempts"
    exit 1
  fi

  echo "Namespace operation not ready yet, waiting... (attempt $attempt/$MAX_ATTEMPTS)"
  attempt=$((attempt + 1))
  sleep "$SLEEP_SECONDS"
done

echo "Creating search attributes for namespace '$NAMESPACE'..."
SEARCH_ATTRIBUTES="MediaFilePath:Keyword MediaTitle:Text MediaType:Keyword MediaMappingName:Keyword MediaWatchRoot:Keyword"
attempt=1
while :; do
  existing=$(temporal operator search-attribute list --namespace "$NAMESPACE" --address "$TEMPORAL_ADDRESS" 2>/dev/null || true)

  missing_args=""
  for attr in $SEARCH_ATTRIBUTES; do
    name=${attr%:*}
    type=${attr#*:}
    if ! echo "$existing" | grep -wq "$name"; then
      missing_args="$missing_args --name $name --type $type"
    fi
  done

  if [ -z "$missing_args" ]; then
    echo "Search attributes already exist"
    break
  fi

  # shellcheck disable=SC2086
  if temporal operator search-attribute create --namespace "$NAMESPACE" --address "$TEMPORAL_ADDRESS" $missing_args >/dev/null 2>&1; then
    echo "Search attributes created:$missing_args"
    break
  fi

  if [ "$attempt" -ge "$MAX_ATTEMPTS" ]; then
    echo "Failed to create search attributes after $MAX_ATTEMPTS attempts"
    exit 1
  fi

  echo "Search attribute creation not ready yet, waiting... (attempt $attempt/$MAX_ATTEMPTS)"
  attempt=$((attempt + 1))
  sleep "$SLEEP_SECONDS"
done
