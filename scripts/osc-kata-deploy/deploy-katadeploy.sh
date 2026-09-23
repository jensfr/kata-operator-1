#!/bin/bash
# Deploy the KataDeploy-based OSC operator on an OpenShift cluster.
#
# Prerequisites:
#   - oc logged in to the target cluster
#   - quay.io/jensfr/osc-operator:kata-deploy accessible
#   - quay.io/jensfr/osc-kata-deploy:source accessible
#
# Usage:
#   ./deploy-katadeploy.sh              # deploy operator + create KataConfig
#   ./deploy-katadeploy.sh --undeploy   # remove everything
#
# Images (override with env vars):
#   OPERATOR_IMG   - operator image
#   KATA_DEPLOY_IMG - combined kata-deploy installer image
#   HELPER_IMG     - helper image for OSC host stages

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

OPERATOR_IMG="${OPERATOR_IMG:-quay.io/jensfr/osc-operator:kata-deploy}"
KATA_DEPLOY_IMG="${KATA_DEPLOY_IMG:-quay.io/jensfr/osc-kata-deploy:source}"
HELPER_IMG="${HELPER_IMG:-registry.access.redhat.com/ubi9/ubi:latest}"
NS="openshift-sandboxed-containers-operator"

undeploy() {
    echo "--- Removing KataConfig ---"
    oc delete kataconfig --all --wait=false 2>/dev/null || true
    sleep 5
    # Remove finalizers if stuck
    for kc in $(oc get kataconfig -o name 2>/dev/null); do
        oc patch "$kc" --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' 2>/dev/null || true
    done
    sleep 5

    echo "--- Undeploying operator ---"
    cd "$REPO_ROOT"
    make undeploy ignore-not-found=true 2>/dev/null || true

    echo "--- Cleaning up ---"
    oc delete runtimeclass kata kata-nvidia-gpu 2>/dev/null || true
    oc delete crd peerpods.confidentialcontainers.org 2>/dev/null || true
    echo "Done."
}

deploy() {
    echo "=== Deploying KataDeploy OSC Operator ==="
    echo "Operator:    $OPERATOR_IMG"
    echo "Kata-Deploy: $KATA_DEPLOY_IMG"
    echo "Helper:      $HELPER_IMG"
    echo ""

    # Step 1: Deploy operator with make deploy
    echo "--- Step 1: make deploy ---"
    cd "$REPO_ROOT"
    make deploy IMG="$OPERATOR_IMG"

    # Step 2: Apply PeerPod CRD (operator crashes without it)
    echo "--- Step 2: PeerPod CRD ---"
    oc apply -f config/peerpods/podvm/cloud-api-adaptor/src/peerpod-ctrl/chart/crds/confidentialcontainers.org_peerpods.yaml 2>/dev/null || true

    # Step 3: Fix webhook TLS
    echo "--- Step 3: Webhook TLS ---"
    oc annotate service webhook-service -n "$NS" \
        service.beta.openshift.io/serving-cert-secret-name=webhook-server-cert \
        --overwrite 2>/dev/null || true
    oc annotate validatingwebhookconfiguration validating-webhook-configuration \
        service.beta.openshift.io/inject-cabundle=true \
        --overwrite 2>/dev/null || true

    # Step 4: Set correct operator image (make deploy may use wrong default)
    echo "--- Step 4: Configure operator image ---"
    oc set image deployment/controller-manager "manager=$OPERATOR_IMG" -n "$NS"

    # Step 5: Set kata-deploy related images
    echo "--- Step 5: Set RELATED_IMAGE env vars ---"
    oc set env deployment/controller-manager -n "$NS" \
        RELATED_IMAGE_KATA_DEPLOY="$KATA_DEPLOY_IMG" \
        RELATED_IMAGE_OSC_HELPER="$HELPER_IMG"

    # Step 6: Set feature gate
    echo "--- Step 6: Feature gate ---"
    cat <<EOF | oc apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: osc-feature-gates
  namespace: $NS
data:
  deploymentMode: KataDeploy
EOF

    # Step 7: Wait for operator pod
    echo "--- Step 7: Waiting for operator ---"
    sleep 15
    oc wait --for=condition=Available deployment/controller-manager -n "$NS" --timeout=120s

    # Step 8: Create KataConfig
    echo "--- Step 8: Creating KataConfig ---"
    cat <<EOF | oc apply -f -
apiVersion: kataconfiguration.openshift.io/v1
kind: KataConfig
metadata:
  name: example-kataconfig
spec:
  enablePeerPods: false
EOF

    echo ""
    echo "=== Deployment started ==="
    echo "Watch progress with:"
    echo "  oc get jobs -n $NS -w"
    echo "  oc get kataconfig"
    echo ""
    echo "When InProgress=False, test with:"
    echo "  oc run kata-test --rm -it --restart=Never --image=registry.access.redhat.com/ubi9-micro --overrides='{\"spec\":{\"runtimeClassName\":\"kata\"}}' -- sh -c 'echo works && uname -r'"
}

if [[ "${1:-}" == "--undeploy" ]]; then
    undeploy
else
    deploy
fi
