#!/usr/bin/env bash
# smoke.sh — boot `kritika serve` the way the chart deploys it and check it
# comes up: two replicas in a throwaway kind cluster, against a CloudNativePG
# Postgres with VectorChord mounted as an image volume extension, the owner,
# application and runner roles as the docs set them up. It fails on a pod
# that never becomes ready (a startup panic, a refused configuration, a
# database the roles cannot use), on `helm test`, and when the replicas do
# not settle on exactly one leader with the configuration applied.
#
# Needs docker, kind, kubectl and helm on PATH. `mise run smoke` runs it.
#   KRITIKA_SMOKE_IMAGE    an image already built and loadable by docker, to
#                          skip the build; a pulled multi-platform image
#                          needs a local tag first (kind cannot load the
#                          manifest list a pull leaves behind)
#   KRITIKA_SMOKE_CLUSTER  the kind cluster's name (kritika-smoke)
#   KRITIKA_SMOKE_KEEP=1   leave the cluster running afterwards
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# renovate: datasource=github-releases depName=cloudnative-pg/cloudnative-pg
CNPG_VERSION=v1.30.1
# renovate: datasource=docker depName=ghcr.io/cloudnative-pg/postgresql
POSTGRES_TAG=18.6-standard-bookworm
# renovate: datasource=docker depName=ghcr.io/tensorchord/vchord-scratch
VCHORD_TAG=pg18-v1.1.1

cluster="${KRITIKA_SMOKE_CLUSTER:-kritika-smoke}"
image="${KRITIKA_SMOKE_IMAGE:-ghcr.io/home-operations/kritika:smoke}"
ns=kritika
export KUBECONFIG="${TMPDIR:-/tmp}/kind-${cluster}.kubeconfig"

log() { echo "==> $*"; }

# Everything worth reading when a step fails, before the cluster goes.
diagnose() {
  echo "--- pods"
  kubectl get pods -A -o wide || true
  echo "--- kritika"
  kubectl -n "$ns" describe pods -l app.kubernetes.io/name=kritika || true
  kubectl -n "$ns" logs -l app.kubernetes.io/name=kritika --all-containers --prefix --tail=200 || true
  echo "--- postgres"
  kubectl -n "$ns" get cluster,database,databaserole -o wide || true
  kubectl -n "$ns" describe cluster kritika-postgres || true
  kubectl -n "$ns" logs -l cnpg.io/cluster=kritika-postgres --tail=100 || true
  echo "--- cnpg operator"
  kubectl -n cnpg-system logs deployment/cnpg-controller-manager --tail=100 || true
}

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "!!! smoke test failed (exit $status)"
    diagnose
  fi
  if [ "${KRITIKA_SMOKE_KEEP:-}" = 1 ]; then
    log "keeping cluster $cluster (KUBECONFIG=$KUBECONFIG)"
  else
    kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
    rm -f "$KUBECONFIG"
  fi
  exit "$status"
}
trap cleanup EXIT

if [ -z "${KRITIKA_SMOKE_IMAGE:-}" ]; then
  log "building $image"
  docker build \
    --build-arg GO_VERSION="$(mise config get -f .mise/config.toml tools.go)" \
    --build-arg NODE_VERSION="$(mise config get -f .mise/config.toml tools.node)" \
    --build-arg VERSION=smoke --build-arg REVISION="$(git rev-parse --short HEAD 2>/dev/null || echo local)" \
    -t "$image" .
fi

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  log "creating kind cluster $cluster"
  kind create cluster --name "$cluster" --wait 2m
else
  log "reusing kind cluster $cluster"
  kind export kubeconfig --name "$cluster"
fi
log "loading $image"
kind load docker-image --name "$cluster" "$image"

log "installing CloudNativePG $CNPG_VERSION"
# The release manifest lives on the minor's release branch.
cnpg="${CNPG_VERSION#v}"
kubectl apply --server-side -f \
  "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${cnpg%.*}/releases/cnpg-${cnpg}.yaml"
kubectl -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=3m

log "creating the roles' Secrets"
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
password() { head -c 24 /dev/urandom | base64 | tr -d '/+=\n'; }
host="kritika-postgres-rw.${ns}.svc"
# As docs/database.md: a basic-auth Secret per role, with the URI kritika
# reads under uri. The owner's is also the Cluster's bootstrap Secret.
role_secret() {
  local name=$1 role=$2 pw
  pw="$(password)"
  kubectl -n "$ns" create secret generic "$name" --type=kubernetes.io/basic-auth \
    --from-literal=username="$role" --from-literal=password="$pw" \
    --from-literal=uri="postgres://${role}:${pw}@${host}:5432/kritika?sslmode=require&connect_timeout=10" \
    --dry-run=client -o yaml | kubectl apply -f -
}
role_secret kritika-postgres-credentials kritika
role_secret kritika-postgres-app kritika_app
role_secret kritika-postgres-runner kritika_runner
kubectl -n "$ns" create secret generic kritika-smoke-auth --from-literal=password="$(password)" \
  --dry-run=client -o yaml | kubectl apply -f -
# The configuration file names an app, whose key must be a PEM; nothing
# here ever signs with it.
kubectl -n "$ns" create secret generic kritika-smoke-bot \
  --from-literal=private-key.pem="$(openssl genrsa 2048 2>/dev/null)" \
  --from-literal=webhook-secret="$(password)" \
  --dry-run=client -o yaml | kubectl apply -f -

log "creating the Postgres cluster"
# docs/database.md's Cluster, Database and DatabaseRoles, with one instance.
kubectl -n "$ns" apply -f - <<MANIFEST
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: kritika-postgres
spec:
  instances: 1
  imageName: ghcr.io/cloudnative-pg/postgresql:${POSTGRES_TAG}
  enableSuperuserAccess: false
  postgresql:
    extensions:
      - name: vchord
        image:
          reference: ghcr.io/tensorchord/vchord-scratch:${VCHORD_TAG}
        dynamic_library_path:
          - /usr/lib/postgresql/18/lib/
        extension_control_path:
          - /usr/share/postgresql/18/
    shared_preload_libraries:
      - vchord
  storage:
    size: 1Gi
  bootstrap:
    initdb:
      database: kritika
      owner: kritika
      secret:
        name: kritika-postgres-credentials
---
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: kritika
spec:
  cluster:
    name: kritika-postgres
  name: kritika
  owner: kritika
  extensions:
    - name: vector
      ensure: present
    - name: vchord
      version: "${VCHORD_TAG#pg18-v}"
      ensure: present
---
apiVersion: postgresql.cnpg.io/v1
kind: DatabaseRole
metadata:
  name: kritika-app
spec:
  cluster:
    name: kritika-postgres
  name: kritika_app
  login: true
  passwordSecret:
    name: kritika-postgres-app
---
apiVersion: postgresql.cnpg.io/v1
kind: DatabaseRole
metadata:
  name: kritika-runner
spec:
  cluster:
    name: kritika-postgres
  name: kritika_runner
  login: true
  passwordSecret:
    name: kritika-postgres-runner
MANIFEST
kubectl -n "$ns" wait cluster/kritika-postgres --for=condition=Ready --timeout=5m
kubectl -n "$ns" wait database/kritika databaserole/kritika-app databaserole/kritika-runner \
  --for=jsonpath='{.status.applied}'=true --timeout=2m

log "installing the chart"
# --wait returns once both replicas are ready: each has loaded its file,
# reached the database and serves the webhooks and the dashboard.
helm upgrade --install kritika charts/kritika -n "$ns" --values scripts/smoke/values.yaml \
  --set image.repository="${image%:*}" --set image.tag="${image##*:}" \
  --wait --timeout 5m
helm test kritika -n "$ns" --logs

log "checking the replicas settle on one leader with the configuration applied"
# Each pod's /metrics through the API server's pod proxy; the leader lock
# follows the migration, so the sum takes a moment to reach 1.
metric() {
  kubectl get --raw "/api/v1/namespaces/${ns}/pods/$1:8081/proxy/metrics" | awk -v m="$2" '$1 == m { print $2 }'
}
mapfile -t pods < <(kubectl -n "$ns" get pods -l app.kubernetes.io/name=kritika,app.kubernetes.io/component=server \
  -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n')
[ "${#pods[@]}" -eq 2 ] || { echo "expected 2 server pods, found ${#pods[@]}"; exit 1; }
for _ in $(seq 1 60); do
  leaders=0
  for pod in "${pods[@]}"; do
    leaders=$((leaders + $(metric "$pod" kritika_leader)))
  done
  [ "$leaders" -eq 1 ] && break
  sleep 2
done
[ "$leaders" -eq 1 ] || { echo "kritika_leader sums to $leaders across ${pods[*]}, want 1"; exit 1; }
for pod in "${pods[@]}"; do
  for m in kritika_config_error kritika_config_drift; do
    v="$(metric "$pod" "$m")"
    [ "$v" = 0 ] || { echo "$pod: $m = $v, want 0"; exit 1; }
  done
  restarts="$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  [ "$restarts" = 0 ] || { echo "$pod restarted $restarts times"; exit 1; }
done
log "ok: ${pods[*]} ready, one leader, configuration applied"
