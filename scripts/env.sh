# Shared by every script. Never touches ~/.kube/config: the demo has its own kubeconfig.
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLUSTER=${CLUSTER:-orphan-tagger-demo}   # override (CLUSTER=x make up) to run a second lab side by side
STATE_DIR=/tmp/$CLUSTER
export KUBECONFIG="$STATE_DIR/kubeconfig"
export HELM_CONFIG_HOME="$STATE_DIR/helm/config" HELM_CACHE_HOME="$STATE_DIR/helm/cache" HELM_DATA_HOME="$STATE_DIR/helm/data"
CROSSPLANE_CHART=2.3.3
KYVERNO_CHART=3.9.1
NS=crossplane-system

# Kyverno 1.19 warns on every use of kyverno.io/v1 ClusterPolicy (production uses it too), so the
# warning is filtered out; every other kubectl message and the exit code pass through unchanged.
k() {
  local err rc=0
  err=$(mktemp)
  kubectl --context "kind-$CLUSTER" "$@" 2>"$err" || rc=$?
  grep -v '^Warning: kyverno.io/v1 .* is deprecated' "$err" >&2 || true
  rm -f "$err"
  return "$rc"
}

if [[ -t 1 ]]; then B=$(tput bold); G=$(tput setaf 2); Y=$(tput setaf 3); C=$(tput setaf 6); N=$(tput sgr0)
else B=""; G=""; Y=""; C=""; N=""; fi
step()    { printf '\n%s== %s%s\n' "$B$C" "$*" "$N"; }
explain() { printf '%s   %s%s\n' "$Y" "$*" "$N"; }
# item <icon> <title>: a blank line, then an icon + bold title, to separate blocks of output
item()    { printf '\n%s   %s  %s%s\n' "$B$Y" "$1" "$2" "$N"; }
run()     { printf '%s   $ %s%s\n' "$C" "$*" "$N"; "$@"; }
mr_table() {
  local out
  out=$(k get managed -o custom-columns='KIND:.kind,NAME:.metadata.name,TERMINATING:.metadata.deletionTimestamp,FINALIZERS:.metadata.finalizers' 2>/dev/null)
  printf '%s   $ kubectl get managed%s\n' "$C" "$N"
  # with no resources, custom-columns still prints the header line
  if [[ $(printf '%s\n' "$out" | wc -l) -gt 1 ]]; then printf '%s\n' "$out"; else echo "   (no managed resources in the cluster)"; fi
}

awscli() { k -n aws-mock exec deploy/aws-cli -- aws "$@"; }

# What the fake AWS holds: the IAM role (with its tags) and its inline policy.
# crossplane-orphaned is the tag the orphan-tagger adds, so it is highlighted.
aws_state() {
  local role policies tags line
  printf '%s   $ aws iam get-role / list-role-tags / list-role-policies   (fake AWS)%s\n' "$C" "$N"
  role=$(awscli iam get-role --role-name demo-taggable-role --query Role.RoleName --output text 2>/dev/null || true)
  if [[ -z "$role" ]]; then echo "   (no IAM role in AWS)"; return; fi
  policies=$(awscli iam list-role-policies --role-name demo-taggable-role --query PolicyNames --output text 2>/dev/null || true)
  tags=$(awscli iam list-role-tags --role-name demo-taggable-role --query 'Tags[].[Key,Value]' --output text 2>/dev/null \
    | tr '\t' '=' | sort)
  echo "   TAGGABLE      iam role           $role   (in AWS: yes)"
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    case "$line" in
      crossplane-orphaned=*) printf '%s     tag  %s   <-- added by the orphan-tagger%s\n' "$B$G" "$line" "$N" ;;
      *)                     printf '     tag  %s\n' "$line" ;;
    esac
  done <<< "$tags"
  case "$tags" in *crossplane-orphaned=*) ;; *) echo "     (no crossplane-orphaned tag)" ;; esac
  echo "   NON-TAGGABLE  inline policy      ${policies:-(none)}   (in AWS: $([[ -n "$policies" ]] && echo yes || echo no); no tags field, nothing to tag)"
}
