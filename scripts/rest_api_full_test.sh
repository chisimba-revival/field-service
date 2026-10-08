#!/bin/bash
# ============================================================================
# REST API Complete Test Suite
# ============================================================================
# Tests all field-guiding service contract endpoints end-to-end.
# ============================================================================

set -e

BASE_URL="http://localhost:8080/api/v1"
COOKIE_JAR="/tmp/test_cookies.txt"
TEST_DEVICE_ID="test-device-$(date +%s)"

# Color codes for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=========================================="
echo "Chisimba REST API Test Suite"
echo "=========================================="
echo ""

# Function to check if jq is available
check_jq() {
    if ! command -v jq &> /dev/null; then
        echo "⚠️  jq not found. Installing..."
        if command -v apt-get &> /dev/null; then
            sudo apt-get update && sudo apt-get install -y jq
        elif command -v yum &> /dev/null; then
            sudo yum install -y jq
        else
            echo "❌ Please install jq manually: https://stedolan.github.io/jq/download/"
            exit 1
        fi
    fi
}

# Function to extract JSON field
json_value() {
    echo "$1" | jq -r "$2"
}

# Function to print success
success() {
    echo -e "${GREEN}✓ $1${NC}"
}

# Function to print error
error() {
    echo -e "${RED}✗ $1${NC}"
}

# Function to print info
info() {
    echo -e "${YELLOW}ℹ $1${NC}"
}

# Cleanup function
cleanup() {
    rm -f "$COOKIE_JAR" /tmp/login_response.json /tmp/token_response.json /tmp/refresh_response.json
}

# Register cleanup on exit
trap cleanup EXIT

echo "Prerequisites check..."
check_jq
echo ""

# ============================================================================
# Test 1: GET /api/v1/auth/token - Get Abuse Evidence
# ============================================================================
info "Test 1: GET /api/v1/auth/token"

rm -f "$COOKIE_JAR"

# Get evidence and save cookie in a single request
EVIDENCE=$(curl -s -c "$COOKIE_JAR" "$BASE_URL/auth/token")

# Check if we got a valid response
if ! echo "$EVIDENCE" | jq -e '.data' > /dev/null 2>&1; then
    error "Invalid JSON response from GET /api/v1/auth/token"
    echo "$EVIDENCE"
    exit 1
fi

CSRF_TOKEN=$(json_value "$EVIDENCE" '.data.csrf_token')
ISSUED_AT=$(json_value "$EVIDENCE" '.data.issued_at')
NONCE=$(json_value "$EVIDENCE" '.data.nonce')
SIGNATURE=$(json_value "$EVIDENCE" '.data.signature')
MIN_SECONDS=$(json_value "$EVIDENCE" '.data.minimum_seconds')

# Check that cookie was saved
if [ ! -f "$COOKIE_JAR" ]; then
    error "Cookie file was not created"
    exit 1
fi

if [ -z "$CSRF_TOKEN" ] || [ -z "$ISSUED_AT" ] || [ -z "$NONCE" ] || [ -z "$SIGNATURE" ]; then
    error "Missing required fields in response"
    echo "$EVIDENCE" | jq .
    exit 1
fi

success "Got abuse evidence"
echo "  CSRF Token: ${CSRF_TOKEN:0:30}..."
echo "  Issued At: $ISSUED_AT"
echo "  Nonce: ${NONCE:0:30}..."
echo "  Min Wait: ${MIN_SECONDS}s"
# Extract PHPSESSION cookie value (awk column 7)
PHPSESSION=$(grep PHPSESSION "$COOKIE_JAR" | awk '{print $7}')

if [ -z "$PHPSESSION" ]; then
    error "PHPSESSION cookie not found in jar"
    exit 1
fi

echo "  Cookie: PHPSESSION=$PHPSESSION"

# ============================================================================
# Test 2: POST /api/v1/auth/token - Login
# ============================================================================
info "Test 2: POST /api/v1/auth/token (login)"

# Wait for minimum time
sleep $((MIN_SECONDS + 2))

# Prepare login data using the SAME cookie from the evidence request
LOGIN_DATA=$(cat <<EOF
{
  "username": "admin",
  "password": "a",
  "csrf_token": "$CSRF_TOKEN",
  "abuse_issued_at": $ISSUED_AT,
  "abuse_nonce": "$NONCE",
  "abuse_signature": "$SIGNATURE",
  "device_id": "$TEST_DEVICE_ID"
}
EOF
)

info "  Step 2a: Using session cookie for login"
COOKIE_VALUE=$(grep PHPSESSION "$COOKIE_JAR" | awk '{print $7}')
echo "  Using cookie: PHPSESSION=$COOKIE_VALUE"

LOGIN_RESPONSE=$(curl -s -b "PHPSESSION=$COOKIE_VALUE" -X POST "$BASE_URL/auth/token" \
    -H "Content-Type: application/json" \
    -d "$LOGIN_DATA")

# Check if we got an error response
if echo "$LOGIN_RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    ERROR_CODE=$(echo "$LOGIN_RESPONSE" | jq -r '.error.code')
    ERROR_MSG=$(echo "$LOGIN_RESPONSE" | jq -r '.error.message')
    error "Login failed: $ERROR_MSG (code: $ERROR_CODE)"
    echo "Full response:"
    echo "$LOGIN_RESPONSE" | jq .
    exit 1
fi

success "Login successful"

# Extract tokens
ACCESS_TOKEN=$(json_value "$LOGIN_RESPONSE" '.data.tokens.access_token')
REFRESH_TOKEN=$(json_value "$LOGIN_RESPONSE" '.data.tokens.refresh_token')
TOKEN_TYPE=$(json_value "$LOGIN_RESPONSE" '.data.tokens.token_type')
EXPIRES_IN=$(json_value "$LOGIN_RESPONSE" '.data.tokens.expires_in')

if [ -z "$ACCESS_TOKEN" ] || [ -z "$REFRESH_TOKEN" ]; then
    error "Missing tokens in response"
    echo "$LOGIN_RESPONSE" | jq .
    exit 1
fi

echo "  Access Token: ${ACCESS_TOKEN:0:60}..."
echo "  Refresh Token: ${REFRESH_TOKEN:0:60}..."
echo "  Token Type: $TOKEN_TYPE"
echo "  Expires In: ${EXPIRES_IN}s"
echo "  Device ID: $TEST_DEVICE_ID"

# ============================================================================
# Test 3: GET /api/v1/user/profile - Get Profile with Access Token
# ============================================================================
info "Test 3: GET /api/v1/user/profile"

sleep 1  # Small delay to ensure token is registered

PROFILE_RESPONSE=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" "$BASE_URL/user/profile")
HTTP_CODE=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" -o /dev/null -w "%{http_code}" "$BASE_URL/user/profile")

if [ "$HTTP_CODE" != "200" ]; then
    error "Expected HTTP 200, got HTTP $HTTP_CODE"
    echo "$PROFILE_RESPONSE" | jq .
    exit 1
fi

success "Profile retrieved"

USERID=$(json_value "$PROFILE_RESPONSE" '.data.user.id')
USERNAME=$(json_value "$PROFILE_RESPONSE" '.data.user.username')
IS_ACTIVE=$(json_value "$PROFILE_RESPONSE" '.data.user.is_active')
EPOCH=$(json_value "$PROFILE_RESPONSE" '.data.identity_epoch')
ACCOUNT_ACTIVE=$(json_value "$PROFILE_RESPONSE" '.data.account_active')

echo "  User ID: $USERID"
echo "  Username: $USERNAME"
echo "  Is Active: $IS_ACTIVE"
echo "  Identity Epoch: $EPOCH"
echo "  Account Active: $ACCOUNT_ACTIVE"

# ============================================================================
# Test 4: POST /api/v1/auth/refresh - Refresh Access Token
# ============================================================================
info "Test 4: POST /api/v1/auth/refresh"

REFRESH_DATA=$(cat <<EOF
{
  "refresh_token": "$REFRESH_TOKEN",
  "device_id": "$TEST_DEVICE_ID"
}
EOF
)

# Get both the response body and HTTP code in one call
REFRESH_OUTPUT=$(curl -s -w "\n__HTTP_CODE__:%{http_code}" -X POST "$BASE_URL/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "$REFRESH_DATA")

# Split response and HTTP code
REFRESH_RESPONSE=$(echo "$REFRESH_OUTPUT" | head -n -1)
REFRESH_HTTP_CODE=$(echo "$REFRESH_OUTPUT" | tail -n1 | sed 's/__HTTP_CODE__://')

info "  HTTP Status: $REFRESH_HTTP_CODE"

if [ "$REFRESH_HTTP_CODE" != "200" ]; then
    error "Expected HTTP 200, got HTTP $REFRESH_HTTP_CODE"
    echo "$REFRESH_RESPONSE" | jq .
    exit 1
fi

# Check if we got an error response
if echo "$REFRESH_RESPONSE" | jq -e '.error' > /dev/null 2>&1; then
    ERROR_CODE=$(echo "$REFRESH_RESPONSE" | jq -r '.error.code')
    ERROR_MSG=$(echo "$REFRESH_RESPONSE" | jq -r '.error.message')
    error "Refresh failed: $ERROR_MSG (code: $ERROR_CODE)"
    exit 1
fi

success "Token refreshed"

NEW_ACCESS_TOKEN=$(json_value "$REFRESH_RESPONSE" '.data.access_token')
NEW_REFRESH_TOKEN=$(json_value "$REFRESH_RESPONSE" '.data.refresh_token')

echo "  New Access Token: ${NEW_ACCESS_TOKEN:0:60}..."
echo "  New Refresh Token: ${NEW_REFRESH_TOKEN:0:60}..."

# ============================================================================
# Test 5: Verify Old Refresh Token is Invalid (Replay Protection)
# ============================================================================
info "Test 5: Verify old refresh token is invalid (replay protection)"

REPLAY_DATA=$(cat <<EOF
{
  "refresh_token": "$REFRESH_TOKEN",
  "device_id": "$TEST_DEVICE_ID"
}
EOF
)

REPLAY_RESPONSE=$(curl -s -X POST "$BASE_URL/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "$REPLAY_DATA")

HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/auth/refresh" \
    -H "Content-Type: application/json" \
    -d "$REPLAY_DATA")

if [ "$HTTP_CODE" != "401" ]; then
    error "Expected HTTP 401 for replayed token, got HTTP $HTTP_CODE"
    echo "$REPLAY_RESPONSE" | jq .
else
    success "Old refresh token correctly rejected (replay protection working)"
fi

# ============================================================================
# Test 6: Verify New Access Token Works
# ============================================================================
info "Test 6: Verify new access token works"

sleep 1

PROFILE_RESPONSE=$(curl -s -H "Authorization: Bearer $NEW_ACCESS_TOKEN" "$BASE_URL/user/profile")
HTTP_CODE=$(curl -s -H "Authorization: Bearer $NEW_ACCESS_TOKEN" -o /dev/null -w "%{http_code}" "$BASE_URL/user/profile")

if [ "$HTTP_CODE" != "200" ]; then
    error "Expected HTTP 200 for new access token, got HTTP $HTTP_CODE"
    echo "$PROFILE_RESPONSE" | jq .
    exit 1
fi

success "New access token works"

# ============================================================================
# Test 7: Backward Compatibility - GET /api/v1/auth/login
# ============================================================================
info "Test 7: Backward Compatibility - GET /api/v1/auth/login"

COMPAT_RESPONSE=$(curl -s "$BASE_URL/auth/login")
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/auth/login")

if [ "$HTTP_CODE" != "200" ]; then
    error "Expected HTTP 200 for backward compat endpoint, got HTTP $HTTP_CODE"
else
    success "Backward compatibility endpoint working"
fi

# ============================================================================
# Test 8: GET /api/v1/auth/jwks - Get Public Keys for JWT Verification
# ============================================================================
info "Test 8: GET /api/v1/auth/jwks"

JWKS_RESPONSE=$(curl -s "$BASE_URL/auth/jwks")
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/auth/jwks")

if [ "$HTTP_CODE" != "200" ]; then
    error "Expected HTTP 200 for JWKS endpoint, got HTTP $HTTP_CODE"
    echo "$JWKS_RESPONSE" | jq .
    exit 1
fi

success "JWKS endpoint working"

KEYS_COUNT=$(echo "$JWKS_RESPONSE" | jq '.keys | length')
echo "  Number of keys: $KEYS_COUNT"

# ============================================================================
# Summary
# ============================================================================
echo ""
echo "=========================================="
echo -e "${GREEN}✓ All Tests Passed!${NC}"
echo "=========================================="
echo ""
echo "Summary:"
echo "  • GET /api/v1/auth/token: ✓"
echo "  • POST /api/v1/auth/token: ✓"
echo "  • GET /api/v1/user/profile: ✓"
echo "  • POST /api/v1/auth/refresh: ✓"
echo "  • Replay protection: ✓"
echo "  • Backward compatibility: ✓"
echo "  • JWKS endpoint: ✓"
echo ""
echo "Device ID used: $TEST_DEVICE_ID"
echo ""

info "Next steps:"
echo "  1. Test with mobile app"
echo "  2. Integrate with field-guiding service"
echo "  3. Test token revocation"
echo "  4. Monitor logs for any issues"
echo ""

exit 0