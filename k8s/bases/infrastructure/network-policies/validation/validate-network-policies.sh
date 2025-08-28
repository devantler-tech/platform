#!/bin/bash
# Network Policy Validation Script
# This script helps validate that network policies are working correctly

set -e

echo "🔒 Network Policy Validation Script"
echo "=================================="

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to test connectivity
test_connectivity() {
    local description="$1"
    local namespace="$2"
    local image="$3"
    local command="$4"
    local expected_result="$5"
    
    echo -n "Testing: $description ... "
    
    # Run the test with timeout
    if kubectl run validation-test-$(date +%s) --rm -i --restart=Never \
        --namespace="$namespace" \
        --image="$image" \
        --timeout=30s \
        -- sh -c "$command" >/dev/null 2>&1; then
        result="success"
    else
        result="failure"
    fi
    
    if [ "$result" = "$expected_result" ]; then
        echo -e "${GREEN}✓ PASS${NC}"
        return 0
    else
        echo -e "${RED}✗ FAIL${NC} (expected $expected_result, got $result)"
        return 1
    fi
}

echo ""
echo "🌐 Testing External Connectivity (Should Work)"
echo "---------------------------------------------"

# Test homepage can reach external APIs
test_connectivity \
    "Homepage → Cloudflare API" \
    "homepage" \
    "curlimages/curl:latest" \
    "curl -s --connect-timeout 10 https://api.cloudflare.com/client/v4/user/tokens/verify" \
    "success"

# Test nextcloud can reach external services
test_connectivity \
    "Nextcloud → External Downloads" \
    "nextcloud" \
    "curlimages/curl:latest" \
    "curl -s --connect-timeout 10 -I https://download.nextcloud.com" \
    "success"

echo ""
echo "🏠 Testing Internal Connectivity (Should Work)"
echo "---------------------------------------------"

# Test DNS resolution
test_connectivity \
    "DNS Resolution from Homepage" \
    "homepage" \
    "busybox:latest" \
    "nslookup kubernetes.default.svc.cluster.local" \
    "success"

# Test whoami service access from testkube
test_connectivity \
    "Testkube → Whoami Service" \
    "testkube" \
    "curlimages/curl:latest" \
    "curl -s --connect-timeout 10 http://whoami.whoami.svc.cluster.local" \
    "success"

echo ""
echo "🚫 Testing Blocked Connectivity (Should Fail)"
echo "--------------------------------------------"

# Test cross-namespace access (should be blocked)
test_connectivity \
    "Whoami → External Internet (blocked)" \
    "whoami" \
    "curlimages/curl:latest" \
    "curl -s --connect-timeout 5 https://www.google.com" \
    "failure"

# Test unauthorized cross-namespace communication
test_connectivity \
    "Nextcloud → Homepage Service (blocked)" \
    "nextcloud" \
    "curlimages/curl:latest" \
    "curl -s --connect-timeout 5 http://homepage.homepage.svc.cluster.local:3000" \
    "failure"

echo ""
echo "📊 Testing System Components"
echo "---------------------------"

# Test API server access from goldilocks
test_connectivity \
    "Goldilocks → API Server" \
    "goldilocks" \
    "bitnami/kubectl:latest" \
    "kubectl get pods --request-timeout=10s" \
    "success"

echo ""
echo "🎯 Summary"
echo "--------"
echo "Network policy validation complete!"
echo ""
echo "To manually verify specific connectivity:"
echo "  kubectl run debug-pod --rm -i -n <namespace> --image=curlimages/curl:latest -- curl <target>"
echo ""
echo "To view Cilium network policies:"
echo "  kubectl get cnp,ccnp -A"
echo ""
echo "To monitor traffic with Hubble:"
echo "  hubble observe --namespace <namespace>"