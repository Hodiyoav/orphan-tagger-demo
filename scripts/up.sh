#!/usr/bin/env bash
# Builds the whole lab: kind + fake AWS + Crossplane + 1 AWS provider + Kyverno + orphan-tagger.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
mkdir -p "$STATE_DIR"

for bin in docker kind kubectl helm; do
  command -v "$bin" >/dev/null || { echo "missing prerequisite: $bin" >&2; exit 1; }
done

step "1/6 kind cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  explain "cluster already exists, reusing it"
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
else
  kind create cluster --name "$CLUSTER" --config "$ROOT/kind/cluster.yaml" --kubeconfig "$KUBECONFIG"
fi

step "2/6 fake AWS (moto)"
k create namespace aws-mock --dry-run=client -o yaml | k apply -f -
k -n aws-mock create configmap tagging-shim --from-file="$ROOT/mock-aws/tagging-shim.py" \
  --dry-run=client -o yaml | k apply -f -
k apply -f "$ROOT/mock-aws/moto.yaml"
k -n aws-mock rollout status deploy/moto --timeout=5m
k -n aws-mock rollout status deploy/aws-cli --timeout=5m

step "3/6 Crossplane + ONE AWS provider (iam) pointed at the fake AWS"
helm repo add crossplane-stable https://charts.crossplane.io/stable >/dev/null
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null
helm repo update >/dev/null
helm upgrade --install --atomic --timeout 10m crossplane crossplane-stable/crossplane \
  --kube-context "kind-$CLUSTER" -n "$NS" --create-namespace --version "$CROSSPLANE_CHART"
k apply -f "$ROOT/crossplane/provider.yaml"
k wait provider.pkg.crossplane.io/provider-aws-iam --for=condition=Healthy --timeout=10m
until k get crd providerconfigs.aws.upbound.io >/dev/null 2>&1; do sleep 3; done
k wait crd/providerconfigs.aws.upbound.io --for=condition=Established --timeout=2m
k apply -f "$ROOT/crossplane/providerconfig.yaml"

step "4/6 Kyverno + the orphan-add-finalizer policy"
helm upgrade --install --atomic --timeout 10m kyverno kyverno/kyverno \
  --kube-context "kind-$CLUSTER" -n kyverno --create-namespace --version "$KYVERNO_CHART" \
  --set backgroundController.enabled=false --set cleanupController.enabled=false \
  --set reportsController.enabled=false
k apply -f "$ROOT/kyverno/orphan-add-finalizer.yaml"
k wait clusterpolicy/orphan-add-finalizer --for=condition=Ready --timeout=2m

step "5/6 orphan-tagger watcher (built from ./orphan-tagger/watcher)"
docker build -q -t orphan-tagger-watcher:demo "$ROOT/orphan-tagger"
kind load docker-image orphan-tagger-watcher:demo --name "$CLUSTER"
existed=$(k -n "$NS" get deploy orphan-tagger-watcher -o name 2>/dev/null || true)
k apply -f "$ROOT/orphan-tagger/watcher.yaml"
# re-run: same image tag, so restart to pick up the rebuilt image
[[ -n "$existed" ]] && k -n "$NS" rollout restart deploy/orphan-tagger-watcher >/dev/null
k -n "$NS" rollout status deploy/orphan-tagger-watcher --timeout=3m

"$ROOT/scripts/status.sh"

step "6/6 ready"
printf '%s   Lab is up. Now run:  make create   then   make delete%s\n' "$G" "$N"
explain "To use kubectl/kubectx yourself (your ~/.kube/config is untouched):"
explain "  export KUBECONFIG=$KUBECONFIG    # context: kind-$CLUSTER"
