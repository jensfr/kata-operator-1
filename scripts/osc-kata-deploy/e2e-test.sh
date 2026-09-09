#!/bin/bash
# OSC KataDeploy E2E test script.
#
# Deploys the operator via make deploy, creates a KataConfig selecting one
# node, verifies the full installation chain, and runs a kata workload
# using only runtimeClassName: kata.
#
# Prerequisites:
#   - KUBECONFIG pointing at a clean OCP 4.22+ cluster
#   - Operator image built and pushed (default uses the validated digest)
#   - quay.io/jensfr/osc-kata-deploy and quay.io/jensfr/osc-helper public
#
# Usage:
#   export KUBECONFIG=<path>
#   ./scripts/osc-kata-deploy/e2e-test.sh [node-name]
#
# If node-name is omitted, the first worker/master node is used.

set -euo pipefail

OPERATOR_IMG="${OPERATOR_IMG:-quay.io/jensfr/osc-operator@sha256:a4f2365b56cad6a850b1198465d56795cc3f66ebabb9747d5b03acfb447905bb}"
KATA_DEPLOY_IMG="${KATA_DEPLOY_IMG:-quay.io/jensfr/osc-kata-deploy@sha256:18ab8c571b8123482af95eab941def467531a17675766b774fed35ca4be68e25}"
HELPER_IMG="${HELPER_IMG:-quay.io/jensfr/osc-helper@sha256:ea1eeb97d480b84ce5b5d4103346daf06c27246112b517adbbb537ff0fe60c85}"
NS="openshift-sandboxed-containers-operator"

TARGET_NODE="${1:-$(oc get nodes -o jsonpath='{.items[0].metadata.name}')}"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

echo "Target node: $TARGET_NODE"
echo "Operator:    $OPERATOR_IMG"
echo ""

# Step 1: Deploy operator
echo "=== Step 1: Deploy operator ==="
cd "$(git rev-parse --show-toplevel)"
make deploy IMG="$OPERATOR_IMG" 2>&1 | tail -3

# Step 2: Webhook TLS via service-ca
echo ""
echo "=== Step 2: Webhook TLS ==="
oc annotate service webhook-service -n $NS \
  service.beta.openshift.io/serving-cert-secret-name=webhook-server-cert 2>&1
oc annotate validatingwebhookconfiguration validating-webhook-configuration \
  service.beta.openshift.io/inject-cabundle=true 2>&1

# Step 3: Environment and feature gate
echo ""
echo "=== Step 3: Environment ==="
oc set env deployment/controller-manager -n $NS \
  RELATED_IMAGE_KATA_DEPLOY="$KATA_DEPLOY_IMG" \
  RELATED_IMAGE_OSC_HELPER="$HELPER_IMG" 2>&1

cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: osc-feature-gates
  namespace: openshift-sandboxed-containers-operator
data:
  deploymentMode: "KataDeploy"
EOF

# Step 4: Wait for operator ready
echo ""
echo "=== Step 4: Wait for operator ==="
for i in $(seq 1 24); do
  READY=$(oc get pods -n $NS -l control-plane=controller-manager \
    -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
  [ "$READY" = "true" ] && break
  sleep 5
done

CERT=$(oc get secret webhook-server-cert -n $NS -o jsonpath='{.type}' 2>/dev/null || true)
[ "$CERT" = "kubernetes.io/tls" ] || fail "webhook cert missing"
pass "Operator ready"

# Step 5: Create KataConfig
echo ""
echo "=== Step 5: Create KataConfig ==="
oc label node "$TARGET_NODE" osc-kata-deploy-test=true --overwrite 2>&1

cat <<EOF | oc apply -f -
apiVersion: kataconfiguration.openshift.io/v1
kind: KataConfig
metadata:
  name: kata-test
spec:
  kataConfigPoolSelector:
    matchLabels:
      osc-kata-deploy-test: "true"
  logLevel: info
EOF

# Step 6: Wait for install
echo ""
echo "=== Step 6: Wait for install ==="
for i in $(seq 1 60); do
  READY_COUNT=$(oc get kataconfig kata-test \
    -o jsonpath='{.status.kataNodes.readyNodeCount}' 2>/dev/null || echo 0)
  [ "$READY_COUNT" = "1" ] && break
  sleep 10
done

[ "$READY_COUNT" = "1" ] || fail "Install did not complete (readyNodeCount=$READY_COUNT)"

# Verify install Job
JOB=$(oc get jobs -n $NS -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
echo "Job: $JOB"
oc get jobs -n $NS 2>&1

# Verify RuntimeClass
HANDLER=$(oc get runtimeclass kata -o jsonpath='{.handler}' 2>/dev/null)
SELECTOR=$(oc get runtimeclass kata -o jsonpath='{.scheduling.nodeSelector}' 2>/dev/null)
[ "$HANDLER" = "kata-qemu" ] || fail "RuntimeClass handler=$HANDLER, expected kata-qemu"
echo "$SELECTOR" | grep -q 'kata-runtime' || fail "RuntimeClass selector missing kata-runtime"

# Verify no stale GPU RC
oc get runtimeclass kata-nvidia-gpu 2>/dev/null && fail "Stale kata-nvidia-gpu exists"

# Verify KataConfig status
IN_PROGRESS=$(oc get kataconfig kata-test -o jsonpath='{.status.conditions[0].status}' 2>/dev/null)
[ "$IN_PROGRESS" = "False" ] || fail "InProgress=$IN_PROGRESS, expected False"

pass "Install complete"

# Step 7: Workload test
echo ""
echo "=== Step 7: Workload test ==="
cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: kata-e2e-test
  namespace: default
spec:
  runtimeClassName: kata
  containers:
  - name: test
    image: registry.access.redhat.com/ubi9/ubi-minimal:9.6
    command: ["sh", "-c", "uname -r; sleep 30"]
    resources:
      requests: {memory: "128Mi", cpu: "100m"}
      limits: {memory: "256Mi", cpu: "500m"}
EOF

for i in $(seq 1 24); do
  PHASE=$(oc get pod kata-e2e-test -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [ "$PHASE" = "Running" ] || [ "$PHASE" = "Succeeded" ] && break
  sleep 5
done

[ "$PHASE" = "Running" ] || [ "$PHASE" = "Succeeded" ] || fail "Pod phase=$PHASE"

SCHED_NODE=$(oc get pod kata-e2e-test -o jsonpath='{.spec.nodeName}')
GUEST=$(oc logs kata-e2e-test 2>/dev/null | head -1)
HOST=$(oc get node "$SCHED_NODE" -o jsonpath='{.status.nodeInfo.kernelVersion}')

echo "Scheduled to: $SCHED_NODE"
echo "Guest kernel: $GUEST"
echo "Host kernel:  $HOST"

[ "$GUEST" = "$HOST" ] || fail "Kernel mismatch: guest=$GUEST host=$HOST"
[ "$SCHED_NODE" = "$TARGET_NODE" ] || fail "Scheduled to $SCHED_NODE, expected $TARGET_NODE"

oc delete pod kata-e2e-test --grace-period=5 2>/dev/null || true

pass "Guest kernel matches host kernel"
echo ""
echo "==============================="
echo "PASS: Full operator E2E complete"
echo "==============================="
