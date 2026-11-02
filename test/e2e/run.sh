#!/usr/bin/env bash
# End-to-end test: runs the CCM from the Helm chart in a local kind cluster, against
# a mock PhoenixNAP API (test/e2e/mockapi), and checks node initialization and the
# Service type=LoadBalancer lifecycle.
#
# Requires docker, kind, kubectl and helm on the PATH.
# Set KEEP_CLUSTER=1 to leave the cluster running afterwards.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLUSTER="${CLUSTER:-pnap-e2e}"
NODE_IMAGE="${NODE_IMAGE:-kindest/node:v1.30.10}"
CCM_IMAGE="cloud-provider-phoenixnap:e2e"
MOCK_IMAGE="pnap-mockapi:e2e"
NETWORK_ID="net-e2e"
NS="kube-system"
CTX="kind-${CLUSTER}"
TMP="$(mktemp -d)"
FAILED=0

k() { kubectl --context "$CTX" "$@"; }
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
# retry <seconds> <command...>: retry until the command succeeds or time runs out
retry() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -ge "$deadline" ] && return 1
    sleep 3
  done
}
mock_state() { k get --raw "/api/v1/namespaces/${NS}/services/pnap-mock:8080/proxy/state"; }

cleanup() {
  if [ "$FAILED" -ne 0 ]; then
    echo "--- CCM logs"; k -n "$NS" logs deploy/ccm-cloud-provider-phoenixnap --tail=80 || true
    echo "--- mock API logs"; k -n "$NS" logs deploy/pnap-mock --tail=40 || true
  fi
  if [ "${KEEP_CLUSTER:-0}" != "1" ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== building images"
docker build -q -t "$CCM_IMAGE" "$ROOT" >/dev/null
docker build -q -t "$MOCK_IMAGE" -f - "$ROOT" >/dev/null <<'EOF'
FROM golang:1.27 AS build
WORKDIR /src
COPY test/e2e/mockapi/main.go .
RUN CGO_ENABLED=0 go build -o /mockapi main.go
FROM gcr.io/distroless/static:nonroot
COPY --from=build /mockapi /mockapi
ENTRYPOINT ["/mockapi"]
EOF

echo "== creating kind cluster ${CLUSTER} (kubelets with --cloud-provider=external)"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
cat > "$TMP/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        cloud-provider: external
        # kind sets provider-id to kind://...; clear it so the CCM must look the server up by name
        provider-id: ""
- role: worker
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        cloud-provider: external
        # kind sets provider-id to kind://...; clear it so the CCM must look the server up by name
        provider-id: ""
EOF
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$TMP/kind.yaml" --wait 0s >/dev/null
kind load docker-image --name "$CLUSTER" "$CCM_IMAGE" "$MOCK_IMAGE" >/dev/null

# the mock knows one server per node, named like the node, with the node's real IP
SERVERS=$(k get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.addresses[?(@.type=="InternalIP")].address},{end}')
if [ -z "$(k get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}')" ]; then
  # with an external cloud provider the kubelet may not report an InternalIP; use docker's view
  SERVERS=""
  for n in $(kind get nodes --name "$CLUSTER"); do
    SERVERS+="${n}=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$n"),"
  done
fi
echo "== mock servers: ${SERVERS%,}"

echo "== deploying mock PhoenixNAP API"
k apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: pnap-mock, namespace: ${NS}}
spec:
  selector: {matchLabels: {app: pnap-mock}}
  template:
    metadata: {labels: {app: pnap-mock}}
    spec:
      tolerations: [{operator: Exists}]
      containers:
      - name: mock
        image: ${MOCK_IMAGE}
        imagePullPolicy: Never
        env: [{name: SERVERS, value: "${SERVERS%,}"}]
        ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: pnap-mock, namespace: ${NS}}
spec:
  selector: {app: pnap-mock}
  ports: [{port: 8080}]
EOF
k -n "$NS" rollout status deploy/pnap-mock --timeout=180s >/dev/null
# the CCM uses host networking and the node's DNS, so address the mock by ClusterIP
MOCK_URL="http://$(k -n "$NS" get svc pnap-mock -o jsonpath='{.spec.clusterIP}'):8080"

echo "== installing CCM chart (base-url ${MOCK_URL})"
cat > "$TMP/values.yaml" <<EOF
image:
  repository: ${CCM_IMAGE%:*}
  tag: ${CCM_IMAGE#*:}
  pullPolicy: Never
config:
  clientID: e2e-client
  clientSecret: e2e-secret
  base-url: ${MOCK_URL}
  token-url: ${MOCK_URL}/token
  location: ASH
  loadbalancer: kube-vip://${NETWORK_ID}
additionalCommands:
  - --v=2
EOF
helm --kube-context "$CTX" install ccm "$ROOT/deploy/chart" -n "$NS" -f "$TMP/values.yaml" >/dev/null
if k -n "$NS" rollout status deploy/ccm-cloud-provider-phoenixnap --timeout=180s >/dev/null; then
  pass "CCM deployment is available"
else
  fail "CCM deployment did not become available"; exit 1
fi

echo "== checking node initialization"
for node in $(k get nodes -o jsonpath='{.items[*].metadata.name}'); do
  if retry 120 bash -c "kubectl --context $CTX get node $node -o jsonpath='{.spec.providerID}' | grep -q '^phoenixnap://server-'"; then
    pass "$node has providerID $(k get node "$node" -o jsonpath='{.spec.providerID}')"
  else
    fail "$node was not given a phoenixnap providerID"; continue
  fi
  taints=$(k get node "$node" -o jsonpath='{.spec.taints[*].key}')
  if echo "$taints" | grep -q node.cloudprovider.kubernetes.io/uninitialized; then
    fail "$node still has the uninitialized taint"
  else
    pass "$node uninitialized taint removed"
  fi
  itype=$(k get node "$node" -o jsonpath='{.metadata.labels.node\.kubernetes\.io/instance-type}')
  region=$(k get node "$node" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/region}')
  [ "$itype" = "s1.c1.small" ] && pass "$node instance-type=$itype" || fail "$node instance-type '$itype'"
  [ "$region" = "ASH" ] && pass "$node region=$region" || fail "$node region '$region'"
done

echo "== checking Service type=LoadBalancer lifecycle"
k create service loadbalancer web --tcp=80:80 >/dev/null
if retry 120 bash -c "kubectl --context $CTX get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}' | grep -q ."; then
  ip=$(k get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  pass "Service got ingress IP $ip"
  [[ "$ip" == 198.51.100.* ]] && pass "ingress IP is from the mock's IP block" || fail "ingress IP $ip is not from the mock's IP block"
  [ "$(k get svc web -o jsonpath='{.spec.loadBalancerIP}')" = "$ip" ] && pass "spec.loadBalancerIP=$ip" || fail "spec.loadBalancerIP not set to $ip"
  [ "$(k get svc web -o jsonpath='{.metadata.annotations.kube-vip\.io/loadbalancerIPs}')" = "$ip" ] && pass "kube-vip.io/loadbalancerIPs=$ip" || fail "kube-vip annotation not set to $ip"
else
  fail "Service never got an ingress IP"
fi
state=$(mock_state)
if echo "$state" | grep -q "\"assignedResourceId\":\"${NETWORK_ID}\""; then
  pass "IP block is assigned to public network ${NETWORK_ID}"
else
  fail "no IP block assigned to ${NETWORK_ID}: $state"
fi

k delete service web >/dev/null
# the reaper runs every 30s: one pass to unassign the block, one to delete it
if retry 150 bash -c "kubectl --context $CTX get --raw /api/v1/namespaces/${NS}/services/pnap-mock:8080/proxy/state | grep -q '\"blocks\":\[\]'"; then
  pass "IP block unassigned and deleted after Service deletion"
else
  fail "IP block not cleaned up: $(mock_state)"
fi

if k -n "$NS" logs deploy/ccm-cloud-provider-phoenixnap | grep -E "^panic:|nil pointer" >/dev/null; then
  fail "CCM logged a panic"
else
  pass "no panics in CCM logs"
fi

echo
if [ "$FAILED" -eq 0 ]; then echo "E2E PASSED"; else echo "E2E FAILED"; exit 1; fi
