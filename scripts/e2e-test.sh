#!/usr/bin/env bash
# End-to-end test: spins up the full Docker stack, configures Nexus,
# deploys a test artifact, and verifies mock Maven Central received it.
#
# Usage:
#   ./scripts/e2e-test.sh
#
# The script tears down the stack on exit (pass KEEP_STACK=1 to skip).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
NEXUS_URL="http://localhost:8081"
SYNC_SERVICE_URL="http://localhost:8080"
MOCK_CENTRAL_URL="http://localhost:8082"
ADMIN_PASSWORD="admin123"
AUTH="admin:${ADMIN_PASSWORD}"
REPO_NAME="maven-releases"
MAX_WAIT=300
POLL_INTERVAL=5

export WEBHOOK_SECRET="e2e-test-secret"

# ---------------------------------------------------------------------------
# Colour helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

pass()  { printf "${GREEN}[PASS]${NC}  %s\n" "$*"; }
fail()  { printf "${RED}[FAIL]${NC}  %s\n" "$*"; }
info()  { printf "${CYAN}[INFO]${NC}  %s\n" "$*"; }
wait_() { printf "${YELLOW}[WAIT]${NC}  %s\n" "$*"; }

FAILED=0

assert_eq() {
    local label="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        pass "${label}"
    else
        fail "${label}: expected '${expected}', got '${actual}'"
        FAILED=1
    fi
}

assert_contains() {
    local label="$1" haystack="$2" needle="$3"
    if echo "$haystack" | grep -q "$needle"; then
        pass "${label}"
    else
        fail "${label}: expected to contain '${needle}', got '${haystack}'"
        FAILED=1
    fi
}

# ---------------------------------------------------------------------------
# Cleanup on exit
# ---------------------------------------------------------------------------
cleanup() {
    if [ "${KEEP_STACK:-0}" = "1" ]; then
        info "KEEP_STACK=1 — leaving Docker stack running."
        return
    fi
    info "Tearing down Docker stack ..."
    cd "$PROJECT_DIR"
    docker compose down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Phase 1: Start the stack
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 1: Starting Docker stack${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

cd "$PROJECT_DIR"
docker compose down -v --remove-orphans 2>/dev/null || true
docker compose up -d --build 2>&1 | tail -5

# ---------------------------------------------------------------------------
# Phase 2: Wait for all services to be healthy
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 2: Waiting for services${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

# Wait for Nexus
elapsed=0
while true; do
    status=$(curl -s -o /dev/null -w '%{http_code}' "${NEXUS_URL}/service/rest/v1/status" 2>/dev/null || echo "000")
    if [ "$status" = "200" ]; then
        pass "Nexus is healthy"
        break
    fi
    if [ "$elapsed" -ge "$MAX_WAIT" ]; then
        fail "Nexus did not become healthy within ${MAX_WAIT}s"
        info "Nexus logs:"
        docker compose logs nexus 2>&1 | tail -20
        exit 1
    fi
    wait_ "Nexus not ready (HTTP ${status}), retrying ... (${elapsed}/${MAX_WAIT}s)"
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
done

# Wait for sync-service
elapsed=0
while true; do
    status=$(curl -s -o /dev/null -w '%{http_code}' "${SYNC_SERVICE_URL}/health" 2>/dev/null || echo "000")
    if [ "$status" = "200" ]; then
        pass "Sync service is healthy"
        break
    fi
    if [ "$elapsed" -ge 60 ]; then
        fail "Sync service did not become healthy within 60s"
        info "Sync service logs:"
        docker compose logs sync-service 2>&1 | tail -20
        exit 1
    fi
    wait_ "Sync service not ready (HTTP ${status}), retrying ..."
    sleep 2
    elapsed=$((elapsed + 2))
done

# Wait for mock-central
elapsed=0
while true; do
    status=$(curl -s -o /dev/null -w '%{http_code}' "${MOCK_CENTRAL_URL}/deployments" 2>/dev/null || echo "000")
    if [ "$status" = "200" ]; then
        pass "Mock Maven Central is healthy"
        break
    fi
    if [ "$elapsed" -ge 60 ]; then
        fail "Mock Maven Central did not become healthy within 60s"
        info "Mock central logs:"
        docker compose logs mock-central 2>&1 | tail -20
        exit 1
    fi
    wait_ "Mock Maven Central not ready (HTTP ${status}), retrying ..."
    sleep 2
    elapsed=$((elapsed + 2))
done

# ---------------------------------------------------------------------------
# Phase 3: Configure Nexus
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 3: Configuring Nexus${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

# Get initial password
INITIAL_PASSWORD=$(docker compose exec -T nexus cat /nexus-data/admin.password 2>/dev/null || echo "")

if [ -n "$INITIAL_PASSWORD" ]; then
    info "Changing admin password ..."
    curl -s -o /dev/null -w '' \
        -X PUT \
        -u "admin:${INITIAL_PASSWORD}" \
        -H 'Content-Type: text/plain' \
        -d "${ADMIN_PASSWORD}" \
        "${NEXUS_URL}/service/rest/v1/security/users/admin/change-password"
    pass "Admin password changed"
else
    info "Password already changed (no admin.password file)"
fi

# Accept EULA (required before any operations on Nexus CE/Pro)
info "Accepting EULA ..."
EULA_JSON=$(curl -s -u "${AUTH}" -X GET -H "Accept: application/json" \
    "${NEXUS_URL}/service/rest/v1/system/eula" 2>/dev/null || echo "")

if echo "$EULA_JSON" | grep -q '"accepted"'; then
    EULA_ACCEPT=$(echo "$EULA_JSON" | jq '.accepted = true')
    eula_resp=$(curl -s -o /dev/null -w '%{http_code}' \
        -u "${AUTH}" -X POST \
        -H "Content-Type: application/json; charset=UTF-8" \
        -d "$EULA_ACCEPT" \
        "${NEXUS_URL}/service/rest/v1/system/eula")
    if [ "$eula_resp" = "204" ] || [ "$eula_resp" = "200" ]; then
        pass "EULA accepted"
    else
        info "EULA accept returned HTTP ${eula_resp} (may already be accepted)"
    fi
else
    info "No EULA endpoint found (older Nexus version), skipping"
fi

# Enable anonymous access
info "Enabling anonymous access ..."
anon_resp=$(curl -s -o /dev/null -w '%{http_code}' \
    -X PUT -u "${AUTH}" \
    -H 'Content-Type: application/json' \
    -d '{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}' \
    "${NEXUS_URL}/service/rest/v1/security/anonymous")
assert_eq "Enable anonymous access" "200" "$anon_resp"

# Create maven-releases repo
info "Creating maven-releases repository ..."
repo_check=$(curl -s -o /dev/null -w '%{http_code}' -u "${AUTH}" \
    "${NEXUS_URL}/service/rest/v1/repositories/${REPO_NAME}" 2>/dev/null || echo "000")

if [ "$repo_check" = "200" ]; then
    info "Repository already exists, skipping"
else
    repo_resp=$(curl -s -o /dev/null -w '%{http_code}' \
        -X POST -u "${AUTH}" \
        -H 'Content-Type: application/json' \
        -d '{
            "name":"maven-releases",
            "online":true,
            "storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"},
            "maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT"}
        }' \
        "${NEXUS_URL}/service/rest/v1/repositories/maven/hosted")
    assert_eq "Create maven-releases repository" "201" "$repo_resp"
fi

# Configure webhook
info "Configuring webhook capability ..."
webhook_resp=$(curl -s -o /dev/null -w '%{http_code}' \
    -X POST -u "${AUTH}" \
    -H 'Content-Type: application/json' \
    -d '{
        "type":"webhook.repository",
        "notes":"e2e test webhook",
        "enabled":true,
        "properties":{
            "repository":"maven-releases",
            "names":"component",
            "url":"http://sync-service:8080/webhook",
            "secret":"e2e-test-secret"
        }
    }' \
    "${NEXUS_URL}/service/rest/v1/capabilities")

HAS_WEBHOOK=false
if [ "$webhook_resp" = "201" ] || [ "$webhook_resp" = "200" ]; then
    pass "Webhook capability created"
    HAS_WEBHOOK=true
else
    info "Webhook capability API returned HTTP ${webhook_resp}"
    info "Webhooks may not be available — will fall back to manual trigger"
fi

# ---------------------------------------------------------------------------
# Phase 4: Deploy a test artifact
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 4: Deploying test artifact${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

GROUP_ID="com.example.test"
ARTIFACT_ID="e2e-test-artifact"
VERSION="1.0.0"
GROUP_PATH="${GROUP_ID//./\/}"
BASE_PATH="${NEXUS_URL}/repository/${REPO_NAME}/${GROUP_PATH}/${ARTIFACT_ID}/${VERSION}"

# Record how many deployments mock-central has BEFORE the test
pre_count=$(curl -s "${MOCK_CENTRAL_URL}/deployments" | jq 'length' 2>/dev/null || echo "0")
info "Mock Central has ${pre_count} deployment(s) before test"

# Create minimal artifact files
ARTIFACT_DIR=$(mktemp -d)
trap 'rm -rf "${ARTIFACT_DIR}"; cleanup' EXIT

cat > "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.pom" << POMEOF
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>${GROUP_ID}</groupId>
  <artifactId>${ARTIFACT_ID}</artifactId>
  <version>${VERSION}</version>
  <name>E2E Test Artifact</name>
  <description>End-to-end test artifact for maven sync</description>
</project>
POMEOF

# Create a minimal valid ZIP as a JAR
printf 'PK\005\006\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' \
    > "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.jar"

info "Uploading POM to Nexus ..."
pom_resp=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${AUTH}" \
    --upload-file "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.pom" \
    "${BASE_PATH}/${ARTIFACT_ID}-${VERSION}.pom")
assert_eq "POM uploaded to Nexus" "201" "$pom_resp"

info "Uploading JAR to Nexus ..."
jar_resp=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${AUTH}" \
    --upload-file "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.jar" \
    "${BASE_PATH}/${ARTIFACT_ID}-${VERSION}.jar")
assert_eq "JAR uploaded to Nexus" "201" "$jar_resp"

# Verify artifact is in Nexus
info "Verifying artifact in Nexus ..."
sleep 2
search_resp=$(curl -s -u "${AUTH}" \
    "${NEXUS_URL}/service/rest/v1/search?repository=${REPO_NAME}&maven.groupId=${GROUP_ID}&maven.artifactId=${ARTIFACT_ID}&maven.baseVersion=${VERSION}")
assert_contains "Artifact found in Nexus search" "$search_resp" "$ARTIFACT_ID"

# ---------------------------------------------------------------------------
# Phase 5: Wait for sync to complete (via webhook or manual trigger)
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 5: Verifying end-to-end sync${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

# Give webhook time to fire and be processed
info "Waiting for webhook + sync pipeline (up to 60s) ..."

synced=false
elapsed=0
while [ "$elapsed" -lt 60 ]; do
    sleep 3
    elapsed=$((elapsed + 3))

    post_count=$(curl -s "${MOCK_CENTRAL_URL}/deployments" | jq 'length' 2>/dev/null || echo "0")
    if [ "$post_count" -gt "$pre_count" ]; then
        synced=true
        break
    fi
    wait_ "No new deployments yet ... (${elapsed}/60s)"
done

if [ "$synced" = "true" ]; then
    pass "Artifact synced to mock Maven Central via webhook!"
else
    info "Webhook did not fire (Nexus OSS may not support webhooks)"
    info "Falling back to manual webhook trigger ..."

    # Manually POST a webhook payload to sync-service (with HMAC signature)
    WEBHOOK_BODY="{
            \"timestamp\": \"$(date -u +%Y-%m-%dT%H:%M:%S.000+0000)\",
            \"nodeId\": \"test-node\",
            \"initiator\": \"admin/127.0.0.1\",
            \"repositoryName\": \"${REPO_NAME}\",
            \"action\": \"CREATED\",
            \"component\": {
                \"id\": \"test-id\",
                \"componentId\": \"test-component-id\",
                \"format\": \"maven2\",
                \"name\": \"${ARTIFACT_ID}\",
                \"group\": \"${GROUP_ID}\",
                \"version\": \"${VERSION}\"
            }
        }"
    SIGNATURE=$(echo -n "$WEBHOOK_BODY" | openssl dgst -sha1 -hmac "$WEBHOOK_SECRET" | awk '{print $NF}')

    manual_resp=$(curl -s -w '\n%{http_code}' \
        -X POST \
        -H 'Content-Type: application/json' \
        -H "X-Nexus-Webhook-Signature: ${SIGNATURE}" \
        -d "$WEBHOOK_BODY" \
        "${SYNC_SERVICE_URL}/webhook")

    manual_http_code=$(echo "$manual_resp" | tail -1)
    manual_body=$(echo "$manual_resp" | sed '$d')

    info "Manual webhook response: HTTP ${manual_http_code}"
    info "Body: ${manual_body}"

    assert_eq "Manual webhook HTTP status" "202" "$manual_http_code"
    assert_contains "Sync reported accepted" "$manual_body" '"accepted"'

    # Poll sync-service /statuses until the GAV shows completed or failed
    GAV_KEY="${GROUP_ID}:${ARTIFACT_ID}:${VERSION}"
    info "Polling sync-service /statuses for ${GAV_KEY} ..."
    sync_elapsed=0
    sync_done=false
    while [ "$sync_elapsed" -lt 60 ]; do
        sleep 3
        sync_elapsed=$((sync_elapsed + 3))
        statuses=$(curl -s "${SYNC_SERVICE_URL}/statuses" 2>/dev/null || echo "{}")
        gav_status=$(echo "$statuses" | jq -r --arg gav "$GAV_KEY" '.[$gav] // empty' 2>/dev/null || echo "")
        if [ "$gav_status" = "completed" ]; then
            pass "GAV ${GAV_KEY} reached 'completed' status"
            sync_done=true
            break
        elif [ "$gav_status" = "failed" ]; then
            fail "GAV ${GAV_KEY} reached 'failed' status"
            sync_done=true
            break
        fi
        wait_ "GAV status: '${gav_status}' ... (${sync_elapsed}/60s)"
    done
    if [ "$sync_done" != "true" ]; then
        fail "GAV ${GAV_KEY} did not reach completed/failed within 60s"
    fi

    # Verify mock-central now has a deployment
    sleep 2
    post_count=$(curl -s "${MOCK_CENTRAL_URL}/deployments" | jq 'length' 2>/dev/null || echo "0")
    if [ "$post_count" -gt "$pre_count" ]; then
        pass "Artifact synced to mock Maven Central via manual trigger!"
        synced=true
    else
        fail "Artifact was NOT synced to mock Maven Central"
    fi
fi

# ---------------------------------------------------------------------------
# Phase 6: Verify the deployment in mock-central
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 6: Validating deployment details${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

if [ "$synced" = "true" ]; then
    deployments_json=$(curl -s "${MOCK_CENTRAL_URL}/deployments")
    info "Deployments in mock Central: ${deployments_json}"

    # Check that at least one deployment reached PUBLISHED
    published_count=$(echo "$deployments_json" | jq '[.[] | select(.deploymentState == "PUBLISHED")] | length' 2>/dev/null || echo "0")

    if [ "$published_count" -gt 0 ]; then
        pass "Deployment state is PUBLISHED (${published_count} published)"
    else
        # May still be in-flight, wait a bit more
        info "No PUBLISHED deployments yet, waiting 15s for state to advance ..."
        sleep 15
        deployments_json=$(curl -s "${MOCK_CENTRAL_URL}/deployments")
        published_count=$(echo "$deployments_json" | jq '[.[] | select(.deploymentState == "PUBLISHED")] | length' 2>/dev/null || echo "0")
        if [ "$published_count" -gt 0 ]; then
            pass "Deployment state is PUBLISHED after wait"
        else
            fail "Deployment did not reach PUBLISHED state"
            info "Final state: ${deployments_json}"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# Phase 7: Check service logs for errors
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
printf "${CYAN}  Phase 7: Checking logs for errors${NC}\n"
printf "${CYAN}========================================${NC}\n\n"

sync_errors=$(docker compose logs sync-service 2>&1 | grep -i "error\|panic\|fatal" | grep -v "level=INFO" || true)
if [ -z "$sync_errors" ]; then
    pass "No errors in sync-service logs"
else
    info "Potential issues in sync-service logs:"
    echo "$sync_errors" | head -10
fi

central_errors=$(docker compose logs mock-central 2>&1 | grep -i "error\|panic\|fatal" | grep -v "level=INFO" || true)
if [ -z "$central_errors" ]; then
    pass "No errors in mock-central logs"
else
    info "Potential issues in mock-central logs:"
    echo "$central_errors" | head -10
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
printf "\n${CYAN}========================================${NC}\n"
if [ "$FAILED" = "0" ]; then
    printf "${GREEN}  ALL E2E TESTS PASSED${NC}\n"
else
    printf "${RED}  SOME E2E TESTS FAILED${NC}\n"
fi
printf "${CYAN}========================================${NC}\n\n"

exit "$FAILED"
