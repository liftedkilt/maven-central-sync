#!/usr/bin/env bash
# Setup script for local Nexus 3 instance.
# Automates: health check, password change, anonymous access,
# repository creation, webhook capability, and test artifact deploy.
#
# Usage:
#   chmod +x scripts/setup-nexus.sh
#   ./scripts/setup-nexus.sh

set -e

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
NEXUS_URL="http://localhost:8081"
NEW_ADMIN_PASSWORD="admin123"
REPO_NAME="maven-releases"
WEBHOOK_URL="http://sync-service:8080/webhook"
MAX_WAIT_SECONDS=300
POLL_INTERVAL=10

# ---------------------------------------------------------------------------
# Colour helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No colour

info()    { printf "${GREEN}[OK]${NC}    %s\n" "$*"; }
warn()    { printf "${YELLOW}[WAIT]${NC}  %s\n" "$*"; }
err()     { printf "${RED}[FAIL]${NC}  %s\n" "$*"; }
step()    { printf "\n${GREEN}>>>${NC} %s\n" "$*"; }

# ---------------------------------------------------------------------------
# 1. Wait for Nexus to be healthy
# ---------------------------------------------------------------------------
step "Waiting for Nexus to become healthy (polling ${NEXUS_URL}) ..."

elapsed=0
while true; do
    status_code=$(curl -s -o /dev/null -w '%{http_code}' "${NEXUS_URL}/service/rest/v1/status" 2>/dev/null || true)
    if [ "$status_code" = "200" ]; then
        info "Nexus is healthy."
        break
    fi
    if [ "$elapsed" -ge "$MAX_WAIT_SECONDS" ]; then
        err "Nexus did not become healthy within ${MAX_WAIT_SECONDS}s. Aborting."
        exit 1
    fi
    warn "Nexus not ready (HTTP ${status_code}). Retrying in ${POLL_INTERVAL}s ... (${elapsed}/${MAX_WAIT_SECONDS}s)"
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
done

# ---------------------------------------------------------------------------
# 2. Read the initial admin password from the Docker container
# ---------------------------------------------------------------------------
step "Reading initial admin password from Nexus container ..."

INITIAL_PASSWORD=$(docker compose exec -T nexus cat /nexus-data/admin.password 2>/dev/null || true)

if [ -z "$INITIAL_PASSWORD" ]; then
    warn "Could not read /nexus-data/admin.password (may already be changed)."
    warn "Assuming password is already '${NEW_ADMIN_PASSWORD}'."
    INITIAL_PASSWORD="$NEW_ADMIN_PASSWORD"
else
    info "Retrieved initial admin password."
fi

# ---------------------------------------------------------------------------
# 3. Change the admin password
# ---------------------------------------------------------------------------
step "Changing admin password ..."

# Check if the new password already works.
auth_check=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${NEW_ADMIN_PASSWORD}" \
    "${NEXUS_URL}/service/rest/v1/status/check" 2>/dev/null || true)

if [ "$auth_check" = "200" ]; then
    info "Admin password is already set to the desired value. Skipping."
    ADMIN_PASSWORD="$NEW_ADMIN_PASSWORD"
else
    response=$(curl -s -o /dev/null -w '%{http_code}' \
        -X PUT \
        -u "admin:${INITIAL_PASSWORD}" \
        -H 'Content-Type: text/plain' \
        -d "${NEW_ADMIN_PASSWORD}" \
        "${NEXUS_URL}/service/rest/v1/security/users/admin/change-password")

    if [ "$response" = "204" ] || [ "$response" = "200" ]; then
        info "Admin password changed successfully."
        ADMIN_PASSWORD="$NEW_ADMIN_PASSWORD"
    else
        err "Failed to change admin password (HTTP ${response})."
        err "If the password was already changed, re-run should succeed."
        exit 1
    fi
fi

ADMIN_PASSWORD="${NEW_ADMIN_PASSWORD}"
AUTH="admin:${ADMIN_PASSWORD}"

# ---------------------------------------------------------------------------
# 3b. Accept EULA (required before any operations)
# ---------------------------------------------------------------------------
step "Accepting EULA ..."

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
        info "EULA accepted."
    else
        warn "EULA accept returned HTTP ${eula_resp} (may already be accepted)."
    fi
else
    warn "No EULA endpoint found, skipping."
fi

# ---------------------------------------------------------------------------
# 4. Enable anonymous access
# ---------------------------------------------------------------------------
step "Enabling anonymous access ..."

anon_response=$(curl -s -o /dev/null -w '%{http_code}' \
    -X PUT \
    -u "${AUTH}" \
    -H 'Content-Type: application/json' \
    -d '{
        "enabled": true,
        "userId": "anonymous",
        "realmName": "NexusAuthorizingRealm"
    }' \
    "${NEXUS_URL}/service/rest/v1/security/anonymous")

if [ "$anon_response" = "200" ] || [ "$anon_response" = "204" ]; then
    info "Anonymous access enabled."
else
    err "Failed to enable anonymous access (HTTP ${anon_response})."
    exit 1
fi

# ---------------------------------------------------------------------------
# 5. Create hosted Maven releases repository
# ---------------------------------------------------------------------------
step "Creating '${REPO_NAME}' hosted Maven repository ..."

# Check if the repository already exists.
repo_check=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${AUTH}" \
    "${NEXUS_URL}/service/rest/v1/repositories/${REPO_NAME}" 2>/dev/null || true)

if [ "$repo_check" = "200" ]; then
    info "Repository '${REPO_NAME}' already exists. Skipping."
else
    repo_response=$(curl -s -o /dev/null -w '%{http_code}' \
        -X POST \
        -u "${AUTH}" \
        -H 'Content-Type: application/json' \
        -d "{
            \"name\": \"${REPO_NAME}\",
            \"online\": true,
            \"storage\": {
                \"blobStoreName\": \"default\",
                \"strictContentTypeValidation\": true,
                \"writePolicy\": \"ALLOW\"
            },
            \"maven\": {
                \"versionPolicy\": \"RELEASE\",
                \"layoutPolicy\": \"STRICT\"
            }
        }" \
        "${NEXUS_URL}/service/rest/v1/repositories/maven/hosted")

    if [ "$repo_response" = "201" ] || [ "$repo_response" = "200" ]; then
        info "Repository '${REPO_NAME}' created."
    else
        err "Failed to create repository (HTTP ${repo_response})."
        exit 1
    fi
fi

# ---------------------------------------------------------------------------
# 6. Configure webhook capability
# ---------------------------------------------------------------------------
step "Configuring webhook capability for '${REPO_NAME}' ..."

# List existing capabilities to check for duplicates.
existing=$(curl -s -u "${AUTH}" "${NEXUS_URL}/service/rest/v1/capabilities" 2>/dev/null || echo "[]")

if echo "$existing" | grep -q '"type"\s*:\s*"webhook.repository"' 2>/dev/null && \
   echo "$existing" | grep -q "${REPO_NAME}" 2>/dev/null; then
    info "Webhook capability for '${REPO_NAME}' appears to already exist. Skipping."
else
    webhook_response=$(curl -s -o /dev/null -w '%{http_code}' \
        -X POST \
        -u "${AUTH}" \
        -H 'Content-Type: application/json' \
        -d "{
            \"type\": \"webhook.repository\",
            \"notes\": \"Maven sync webhook\",
            \"enabled\": true,
            \"properties\": {
                \"repository\": \"${REPO_NAME}\",
                \"names\": \"component\",
                \"url\": \"${WEBHOOK_URL}\",
                \"secret\": \"\"
            }
        }" \
        "${NEXUS_URL}/service/rest/v1/capabilities")

    if [ "$webhook_response" = "201" ] || [ "$webhook_response" = "200" ]; then
        info "Webhook capability created (${WEBHOOK_URL})."
    else
        warn "Could not create webhook capability via REST API (HTTP ${webhook_response})."
        warn "This may need to be configured manually in the Nexus admin UI:"
        warn "  1. Go to Administration > System > Capabilities"
        warn "  2. Create a new 'Webhook: Repository' capability"
        warn "  3. Set repository=${REPO_NAME}, event=component.created, url=${WEBHOOK_URL}"
    fi
fi

# ---------------------------------------------------------------------------
# 7. Deploy a sample test artifact
# ---------------------------------------------------------------------------
step "Deploying sample test artifact ..."

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -x "${SCRIPT_DIR}/deploy-test-artifact.sh" ]; then
    "${SCRIPT_DIR}/deploy-test-artifact.sh"
else
    # Inline fallback if companion script is not available.
    bash "${SCRIPT_DIR}/deploy-test-artifact.sh" 2>/dev/null || {
        warn "Companion script deploy-test-artifact.sh not found. Deploying inline."

        ARTIFACT_DIR=$(mktemp -d)
        trap 'rm -rf "${ARTIFACT_DIR}"' EXIT

        cat > "${ARTIFACT_DIR}/test-artifact-1.0.0.pom" << 'POMEOF'
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example.test</groupId>
  <artifactId>test-artifact</artifactId>
  <version>1.0.0</version>
  <name>Test Artifact</name>
  <description>Test artifact for maven sync</description>
</project>
POMEOF

        printf 'PK\005\006\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' \
            > "${ARTIFACT_DIR}/test-artifact-1.0.0.jar"

        BASE_PATH="${NEXUS_URL}/repository/${REPO_NAME}/com/example/test/test-artifact/1.0.0"

        curl -s -u "${AUTH}" --upload-file "${ARTIFACT_DIR}/test-artifact-1.0.0.pom" \
            "${BASE_PATH}/test-artifact-1.0.0.pom"
        curl -s -u "${AUTH}" --upload-file "${ARTIFACT_DIR}/test-artifact-1.0.0.jar" \
            "${BASE_PATH}/test-artifact-1.0.0.jar"

        info "Test artifact deployed to ${BASE_PATH}"
    }
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
printf "\n${GREEN}============================================${NC}\n"
printf "${GREEN}  Nexus setup complete!${NC}\n"
printf "${GREEN}============================================${NC}\n"
printf "  Nexus UI:    ${NEXUS_URL}\n"
printf "  Credentials: admin / ${ADMIN_PASSWORD}\n"
printf "  Repository:  ${NEXUS_URL}/repository/${REPO_NAME}/\n"
printf "\n"
