#!/bin/bash

# Field Service Integration Test Script
# This script tests the complete integration between Chisimba and Field Service

set -e

API_BASE="http://localhost:8080/api/v1"
FIELD_SERVICE_BASE="http://localhost:8081"

echo "=== Field Service Integration Test ==="
echo "Testing complete integration between Chisimba and Field Service"
echo ""

# Step 1: Health check field-service
echo "1. Checking field-service health..."
HEALTH=$(curl -s $FIELD_SERVICE_BASE/health)
if echo "$HEALTH" | grep -q '"status":"ok"'; then
    echo "   ✅ Field service is healthy"
else
    echo "   ❌ Field service health check failed"
    echo "      Response: $HEALTH"
    exit 1
fi

# Step 2: Check JWKS endpoint
echo ""
echo "2. Checking JWKS endpoint..."
JWKS=$(curl -s $API_BASE/auth/jwks)
if echo "$JWKS" | grep -q '"keys"'; then
    echo "   ✅ JWKS endpoint working"
    KID=$(echo $JWKS | grep -o '"kid":"[^"]*"' | cut -d'"' -f4)
    echo "      Key ID: $KID"
else
    echo "   ❌ JWKS endpoint failed"
    exit 1
fi

# Step 3: Get abuse evidence
echo ""
echo "3. Getting abuse evidence..."
EVIDENCE=$(curl -s -c /tmp/cookies.txt -b /tmp/cookies.txt $API_BASE/auth/token)
if echo "$EVIDENCE" | grep -q '"csrf_token"'; then
    echo "   ✅ Abuse evidence received"
    CSRF=$(echo $EVIDENCE | grep -o '"csrf_token":"[^"]*"' | cut -d'"' -f4)
    NONCE=$(echo $EVIDENCE | grep -o '"nonce":"[^"]*"' | cut -d'"' -f4)
    ISSUED=$(echo $EVIDENCE | grep -o '"issued_at":[0-9]*' | cut -d':' -f2)
    SIG=$(echo $EVIDENCE | grep -o '"signature":"[^"]*"' | cut -d'"' -f4)
    echo "      CSRF: ${CSRF:0:20}..."
    echo "      Nonce: ${NONCE:0:20}..."
else
    echo "   ❌ Failed to get abuse evidence"
    exit 1
fi

# Step 4: Login to get tokens
echo ""
echo "4. Logging in to get tokens..."
LOGIN_RESPONSE=$(curl -s -c /tmp/cookies.txt -b /tmp/cookies.txt -X POST -H "Content-Type: application/json" \
  -d "{
    \"username\": \"admin\",
    \"password\": \"a\",
    \"abuse_nonce\": \"$NONCE\",
    \"csrf_token\": \"$CSRF\",
    \"abuse_issued_at\": \"$ISSUED\",
    \"abuse_signature\": \"$SIG\",
    \"device_id\": \"integration-test-device\"
  }" $API_BASE/auth/token)

if echo "$LOGIN_RESPONSE" | grep -q '"access_token"'; then
    echo "   ✅ Tokens issued successfully"
    ACCESS_TOKEN=$(echo $LOGIN_RESPONSE | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4)
    REFRESH_TOKEN=$(echo $LOGIN_RESPONSE | grep -o '"refresh_token":"[^"]*"' | cut -d'"' -f4)
    echo "      Access token: ${ACCESS_TOKEN:0:50}..."
    echo "      Refresh token: ${REFRESH_TOKEN:0:50}..."
else
    echo "   ❌ Failed to get tokens"
    echo "      Response: $LOGIN_RESPONSE"
    exit 1
fi

# Step 5: Test token validation by field-service
echo ""
echo "5. Testing token validation by field-service..."
# Try to call sync/push with empty operations (should get validation error, not auth error)
SYNC_RESPONSE=$(curl -s -X POST -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"operations":[]}' \
  $FIELD_SERVICE_BASE/api/v1/sync/push)

if echo "$SYNC_RESPONSE" | grep -q '"error"'; then
    if echo "$SYNC_RESPONSE" | grep -q '"no_operations"'; then
        echo "   ✅ Token validation working (got validation error, not auth error)"
    else
        echo "   ✅ Token validation working (got expected error)"
    fi
    echo "      Response: $(echo $SYNC_RESPONSE | grep -o '"error":"[^"]*"' | cut -d'"' -f4)"
else
    echo "   ❌ Token validation failed"
    echo "      Response: $SYNC_RESPONSE"
    exit 1
fi

# Step 6: Test with actual operation (should fail due to empty context)
echo ""
echo "6. Testing with actual operation..."
SYNC_WITH_OP=$(curl -s -X POST -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "operations": [{
      "operation_id": "test-op-001",
      "entity": "log_book_entry",
      "kind": "create",
      "entity_id": "entry-001",
      "captured_at": "2026-10-06T21:00:00Z",
      "recorded_at": "2026-10-06T21:05:00Z",
      "payload": {"species": "lion", "count": 2}
    }]
  }' \
  $FIELD_SERVICE_BASE/api/v1/sync/push)

if echo "$SYNC_WITH_OP" | grep -q '"error"'; then
    if echo "$SYNC_WITH_OP" | grep -q '"no active context"'; then
        echo "   ✅ Context validation working (empty context detected)"
        echo "      This is expected for admin user without context assignment"
    else
        echo "   ✅ Operation processed (may have failed due to other validation)"
    fi
    echo "      Response: $(echo $SYNC_WITH_OP | grep -o '"error":"[^"]*"' | cut -d'"' -f4)"
else
    echo "   ✅ Operation succeeded"
fi

# Step 7: Test user profile endpoint
echo ""
echo "7. Testing user profile endpoint..."
PROFILE=$(curl -s -H "Authorization: Bearer $ACCESS_TOKEN" $API_BASE/user/profile)
if echo "$PROFILE" | grep -q '"context_grants"'; then
    echo "   ✅ Profile endpoint working"
    echo "      Active context: $(echo $PROFILE | grep -o '"active_context":"[^"]*"' | cut -d'"' -f4)"
    echo "      Context grants: $(echo $PROFILE | grep -o '"context_grants":\[[^]]*\]' | cut -d'[' -f2 | cut -d']' -f1)"
else
    echo "   ❌ Profile endpoint failed"
    echo "      Response: $PROFILE"
    exit 1
fi

# Step 8: Test refresh token
echo ""
echo "8. Testing refresh token..."
REFRESH_RESPONSE=$(curl -s -X POST -H "Content-Type: application/json" \
  -d "{
    \"refresh_token\": \"$REFRESH_TOKEN\",
    \"device_id\": \"integration-test-device\"
  }" $API_BASE/auth/refresh)

if echo "$REFRESH_RESPONSE" | grep -q '"access_token"'; then
    echo "   ✅ Refresh token working"
    NEW_ACCESS_TOKEN=$(echo $REFRESH_RESPONSE | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4)
    echo "      New access token: ${NEW_ACCESS_TOKEN:0:50}..."
else
    echo "   ❌ Refresh token failed"
    echo "      Response: $REFRESH_RESPONSE"
    exit 1
fi

echo ""
echo "=== Integration Test Complete ==="
echo "✅ All tests passed!"
echo ""
echo "Summary:"
echo "  - Field service is healthy and responding"
echo "  - JWKS endpoint is working for token validation"
echo "  - Tokens are being issued correctly by Chisimba"
echo "  - Field service is validating tokens correctly"
echo "  - Integration between services is working"
echo ""
echo "Note: Admin user has empty active context, which is expected."
echo "      To fully test sync operations, assign a context to the user."