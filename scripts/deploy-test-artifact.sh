#!/usr/bin/env bash
# Deploy a sample test artifact to the local Nexus Maven repository.
# Useful for re-testing the webhook / sync pipeline without re-running
# the full setup.
#
# Usage:
#   chmod +x scripts/deploy-test-artifact.sh
#   ./scripts/deploy-test-artifact.sh              # defaults: version 1.0.0
#   ./scripts/deploy-test-artifact.sh 2.0.0        # custom version

set -e

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
NEXUS_URL="${NEXUS_URL:-http://localhost:8081}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin123}"
REPO_NAME="${REPO_NAME:-maven-releases}"
AUTH="admin:${ADMIN_PASSWORD}"

GROUP_ID="com.example.test"
ARTIFACT_ID="test-artifact"
VERSION="${1:-1.0.0}"

# ---------------------------------------------------------------------------
# Colour helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()  { printf "${GREEN}[OK]${NC}    %s\n" "$*"; }
err()   { printf "${RED}[FAIL]${NC}  %s\n" "$*"; }
step()  { printf "\n${GREEN}>>>${NC} %s\n" "$*"; }

# ---------------------------------------------------------------------------
# Build group path from dotted groupId  (com.example.test -> com/example/test)
# ---------------------------------------------------------------------------
GROUP_PATH="${GROUP_ID//./\/}"
BASE_PATH="${NEXUS_URL}/repository/${REPO_NAME}/${GROUP_PATH}/${ARTIFACT_ID}/${VERSION}"

# ---------------------------------------------------------------------------
# Create temporary artifact files
# ---------------------------------------------------------------------------
step "Preparing test artifact: ${GROUP_ID}:${ARTIFACT_ID}:${VERSION}"

ARTIFACT_DIR=$(mktemp -d)
trap 'rm -rf "${ARTIFACT_DIR}"' EXIT

cat > "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.pom" << POMEOF
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>${GROUP_ID}</groupId>
  <artifactId>${ARTIFACT_ID}</artifactId>
  <version>${VERSION}</version>
  <name>Test Artifact</name>
  <description>Test artifact for maven sync</description>
</project>
POMEOF

# Minimal valid ZIP (empty central directory) to act as a JAR.
printf 'PK\005\006\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000\000' \
    > "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.jar"

# ---------------------------------------------------------------------------
# Upload POM
# ---------------------------------------------------------------------------
step "Uploading POM ..."

pom_status=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${AUTH}" \
    --upload-file "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.pom" \
    "${BASE_PATH}/${ARTIFACT_ID}-${VERSION}.pom")

if [ "$pom_status" = "201" ] || [ "$pom_status" = "200" ]; then
    info "POM uploaded."
elif [ "$pom_status" = "400" ]; then
    err "POM upload returned 400 -- artifact version may already exist in a release repo."
    exit 1
else
    err "POM upload failed (HTTP ${pom_status})."
    exit 1
fi

# ---------------------------------------------------------------------------
# Upload JAR
# ---------------------------------------------------------------------------
step "Uploading JAR ..."

jar_status=$(curl -s -o /dev/null -w '%{http_code}' \
    -u "${AUTH}" \
    --upload-file "${ARTIFACT_DIR}/${ARTIFACT_ID}-${VERSION}.jar" \
    "${BASE_PATH}/${ARTIFACT_ID}-${VERSION}.jar")

if [ "$jar_status" = "201" ] || [ "$jar_status" = "200" ]; then
    info "JAR uploaded."
elif [ "$jar_status" = "400" ]; then
    err "JAR upload returned 400 -- artifact version may already exist in a release repo."
    exit 1
else
    err "JAR upload failed (HTTP ${jar_status})."
    exit 1
fi

# ---------------------------------------------------------------------------
# Verify
# ---------------------------------------------------------------------------
step "Verifying artifact is accessible ..."

verify_status=$(curl -s -o /dev/null -w '%{http_code}' \
    "${BASE_PATH}/${ARTIFACT_ID}-${VERSION}.pom")

if [ "$verify_status" = "200" ]; then
    info "Artifact verified at ${BASE_PATH}"
else
    err "Artifact verification failed (HTTP ${verify_status}). It may require authentication."
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
printf "\n${GREEN}Test artifact deployed successfully.${NC}\n"
printf "  GAV:  ${GROUP_ID}:${ARTIFACT_ID}:${VERSION}\n"
printf "  URL:  ${BASE_PATH}/\n"
printf "\n"
