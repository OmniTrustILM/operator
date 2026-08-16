#!/usr/bin/env bash
# Local SonarQube analysis using an ephemeral Docker container.
# Usage: ./hack/sonar-local.sh
set -euo pipefail

CONTAINER_NAME="ilm-operator-sonarqube"
SONAR_PORT="${SONAR_PORT:-9000}"
PROJECT_KEY="ilm-operator"
SONAR_URL="http://localhost:${SONAR_PORT}"

cleanup() {
    echo "Stopping SonarQube..."
    docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}

# Clean up on exit
trap cleanup EXIT

# Remove any leftover container
docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true

echo "Starting SonarQube Community on port ${SONAR_PORT}..."
docker run -d --name "${CONTAINER_NAME}" -p "${SONAR_PORT}:9000" sonarqube:community >/dev/null

# A cold Elasticsearch boot on a loaded machine can take several minutes.
STARTUP_DEADLINE_SECONDS=300
echo "Waiting for SonarQube to be ready (up to $((STARTUP_DEADLINE_SECONDS / 60)) minutes)..."
for i in $(seq 1 "${STARTUP_DEADLINE_SECONDS}"); do
    if curl -sf "${SONAR_URL}/api/system/status" 2>/dev/null | grep -q '"status":"UP"'; then
        echo "SonarQube is ready."
        break
    fi
    if [ "$i" -eq "${STARTUP_DEADLINE_SECONDS}" ]; then
        echo "ERROR: SonarQube failed to start within $((STARTUP_DEADLINE_SECONDS / 60)) minutes."
        exit 1
    fi
    sleep 1
done

# SonarQube Community forces password change on first login.
# Change from default admin/admin to admin/Admin12345678!.
echo "Configuring SonarQube..."
CHANGE_RESP=$(curl -s -o /dev/null -w "%{http_code}" -u admin:admin -X POST \
    "${SONAR_URL}/api/users/change_password?login=admin&previousPassword=admin&password=Admin12345678!" 2>/dev/null || true)

# Determine which credentials work
if curl -sf -u admin:Admin12345678! "${SONAR_URL}/api/system/status" >/dev/null 2>&1; then
    SONAR_CREDS="admin:Admin12345678!"
elif curl -sf -u admin:admin "${SONAR_URL}/api/system/status" >/dev/null 2>&1; then
    SONAR_CREDS="admin:admin"
else
    echo "ERROR: Cannot authenticate to SonarQube."
    exit 1
fi

# Generate a token
TOKEN_NAME="ci-$(date +%s)"
TOKEN=$(curl -sf -u "${SONAR_CREDS}" -X POST \
    "${SONAR_URL}/api/user_tokens/generate?name=${TOKEN_NAME}" \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")

if [ -z "${TOKEN}" ]; then
    echo "ERROR: Failed to generate SonarQube token."
    exit 1
fi

echo "Running sonar-scanner..."
sonar-scanner \
    -Dsonar.projectKey="${PROJECT_KEY}" \
    -Dsonar.sources=api,cmd,internal,pkg \
    -Dsonar.tests=internal,pkg \
    -Dsonar.test.inclusions="**/*_test.go" \
    -Dsonar.exclusions="**/*_test.go,**/zz_generated*.go,test/**" \
    -Dsonar.go.coverage.reportPaths=cover.out \
    -Dsonar.go.golangci-lint.reportPaths=golangci-lint-report.xml \
    -Dsonar.host.url="${SONAR_URL}" \
    -Dsonar.token="${TOKEN}" \
    -Dsonar.cpd.minimumTokens=100 \
    -Dsonar.issue.ignore.multicriteria=todoconv,todoconvtest \
    -Dsonar.issue.ignore.multicriteria.todoconv.ruleKey=go:S1135 \
    -Dsonar.issue.ignore.multicriteria.todoconv.resourceKey=pkg/convert/convert.go \
    -Dsonar.issue.ignore.multicriteria.todoconvtest.ruleKey=go:S1135 \
    -Dsonar.issue.ignore.multicriteria.todoconvtest.resourceKey=pkg/convert/convert_test.go

echo ""
echo "=== SonarQube Results ==="
echo "Dashboard: ${SONAR_URL}/dashboard?id=${PROJECT_KEY}"
echo ""

# Wait for analysis to be processed
sleep 10

echo "Quality Gate:"
curl -sf -u "${SONAR_CREDS}" \
    "${SONAR_URL}/api/qualitygates/project_status?projectKey=${PROJECT_KEY}" \
    | python3 -c "
import sys, json
d = json.load(sys.stdin)['projectStatus']
print(f'  Status: {d[\"status\"]}')
for c in d.get('conditions', []):
    print(f'  {c[\"metricKey\"]}: {c[\"actualValue\"]} (threshold: {c[\"errorThreshold\"]}) - {c[\"status\"]}')
"

echo ""
echo "Issues:"
curl -sf -u "${SONAR_CREDS}" \
    "${SONAR_URL}/api/issues/search?projectKeys=${PROJECT_KEY}&statuses=OPEN&ps=100" \
    | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(f'  Total: {d[\"total\"]}')
for i in d['issues']:
    comp = i['component'].split(':')[-1]
    line = i.get('line', '?')
    print(f'  [{i[\"severity\"]}] {comp}:{line} - {i[\"message\"]}')
"

echo ""
echo "Done. SonarQube will be stopped automatically."
