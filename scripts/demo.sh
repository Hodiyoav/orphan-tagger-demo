#!/usr/bin/env bash
# One command: prepare the lab, then walk through create and delete, asking before each.
# Usage: ./scripts/demo.sh [-y|--yes]      (-y answers every question with yes)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

YES=false
for arg in "$@"; do
  case "$arg" in
    -y|--yes) YES=true ;;
    *) echo "usage: $0 [-y|--yes]" >&2; exit 1 ;;
  esac
done

# Reads the answer from stdin; EOF counts as "no".
confirm() {
  local ans
  if $YES; then printf '\n%s   %s [y/N] y  (--yes)%s\n' "$B$Y" "$1" "$N"; return 0; fi
  printf '\n%s   %s [y/N] %s' "$B$Y" "$1" "$N"
  read -r ans || ans=n
  [[ "$ans" == y || "$ans" == Y || "$ans" == yes ]]
}
stop() {
  echo
  explain "Stopped. The lab is still up: run this again to continue, or 'make down' to remove it."
  exit 0
}

# Highlights the lines that matter in kubectl YAML / aws JSON (marked with ◀) and drops the
# noisy last-applied-configuration annotation (2 lines).
hl() {
  awk -v on="$B$G" -v off="$N" '
    /kubectl.kubernetes.io\/last-applied-configuration/ { skip = 1; next }
    skip == 1 { skip = 0; next }
    /gong\.io\/orphan-tagger|finalizer\.managedresource|deletionPolicy|crossplane-orphaned|^ *owner: |"owner"/ {
      print on $0 "  ◀" off
      mark = ($0 ~ /"Key": "(crossplane-orphaned|owner)"/)
      next
    }
    mark == 1 { print on $0 off; mark = 0; next }
    { print }'
}

# frame <title>: draws stdin as a framed panel
frame() {
  local line
  printf '\n%s   ╭─ %s ──────────────────────────────%s\n' "$B$G" "$1" "$N"
  while IFS= read -r line; do printf '%s   │%s %s\n' "$B$G" "$N" "$line"; done
  printf '%s   ╰───────────────────────────────────────%s\n' "$B$G" "$N"
}

AWSM_ALIAS="alias awsm='kubectl --kubeconfig $KUBECONFIG -n aws-mock exec deploy/aws-cli -- aws'"
# kube <icon> <why> <kubectl args...> / awsm <icon> <why> <aws args...>:
# a titled block with the command and its (highlighted) output
kube() {
  local icon=$1 why=$2; shift 2
  item "$icon" "$why"
  printf '%s   $ kubectl --kubeconfig %s %s%s\n' "$C" "$KUBECONFIG" "$*" "$N"
  { k "$@" 2>&1 || true; } | hl
}
awsm() {
  local icon=$1 why=$2; shift 2
  item "$icon" "$why"
  printf '%s   $ awsm %s%s\n' "$C" "$*" "$N"
  { awscli "$@" 2>&1 || true; } | hl
}

show_mrs() {
  step "☸️  In Kubernetes: the Crossplane managed resources"
  kube "📋" "All managed resources" get managed
  kube "🎭" "Role (finalizers, deletionPolicy and tags are marked ◀)" get role.iam.aws.upbound.io -o yaml
  kube "📜" "RolePolicy (same fields)" get rolepolicy.iam.aws.upbound.io -o yaml
}

show_aws() {
  step "☁️  In the fake AWS (moto): what really exists"
  printf '%s   %s%s\n' "$C" "$AWSM_ALIAS" "$N"
  awsm "🎭" "The role itself" iam get-role --role-name demo-taggable-role
  awsm "🔖" "Its tags (look for crossplane-orphaned)" iam list-role-tags --role-name demo-taggable-role
  awsm "📎" "Inline policy names" iam list-role-policies --role-name demo-taggable-role
  awsm "📄" "The policy document" iam get-role-policy --role-name demo-taggable-role --policy-name demo-policy
  awsm "📚" "All roles in moto" iam list-roles --query 'Roles[].RoleName'
}

# summary created|deleted: the few facts worth looking at, in a frame
summary() {
  local phase=$1 role_fin pol_fin tags pol left
  tags=$(awscli iam list-role-tags --role-name demo-taggable-role --query 'Tags[].[Key,Value]' --output text 2>/dev/null \
    | tr '\t' '=' | sort | tr '\n' ' ' || true)
  pol=$(awscli iam list-role-policies --role-name demo-taggable-role --query PolicyNames --output text 2>/dev/null || true)
  {
    if [[ "$phase" == created ]]; then
      role_fin=$(k get role.iam.aws.upbound.io demo-taggable-role -o jsonpath='{.metadata.finalizers}' 2>/dev/null || true)
      pol_fin=$(k get rolepolicy.iam.aws.upbound.io demo-non-taggable-rolepolicy -o jsonpath='{.metadata.finalizers}' 2>/dev/null || true)
      case "$role_fin$pol_fin" in
        *orphan-tagger*orphan-tagger*) echo "✅ Both resources carry the finalizer gong.io/orphan-tagger (added by Kyverno)" ;;
        *)                             echo "❌ The gong.io/orphan-tagger finalizer is missing" ;;
      esac
      echo "✅ The role exists in moto"
      echo "   tags: $tags"
      case "$tags" in
        *crossplane-orphaned*) echo "⚠️  crossplane-orphaned is already set (left over from an earlier run)" ;;
        *)                     echo "➖ No crossplane-orphaned tag yet: nothing has been deleted" ;;
      esac
      echo "✅ Inline policy in moto: ${pol:-(none)}"
    else
      left=$(k get managed -o name 2>/dev/null || true)
      if [[ -z "$left" ]]; then echo "✅ Both managed resources are gone from Kubernetes"
      else echo "❌ Still in Kubernetes: $left"; fi
      echo "✅ The role is still in moto (deletionPolicy: Orphan)"
      echo "   tags: $tags"
      case "$tags" in
        *crossplane-orphaned=true*) echo "🏷️  crossplane-orphaned=true: added by the orphan-tagger, so this AWS resource is now marked as orphaned" ;;
        *)                          echo "❌ crossplane-orphaned tag is missing" ;;
      esac
      echo "✅ Inline policy ${pol:-(none)} is still in moto; it has no tags, so the tagger only released its finalizer"
    fi
  } | frame "🔎 What to look at"
}

step "Preparing the lab (kind + fake AWS + Crossplane + Kyverno + orphan-tagger)"
QUIET_NEXT=1 "$ROOT/scripts/up.sh"

step "What I am about to create"
item "🏷️ " "demo-resources/taggable-role.yaml   (taggable)"
cat "$ROOT/demo-resources/taggable-role.yaml"
item "🚫" "demo-resources/non-taggable-rolepolicy.yaml   (not taggable)"
cat "$ROOT/demo-resources/non-taggable-rolepolicy.yaml"
item "🏷️ " "Role is taggable (its CRD has spec.forProvider.tags): on delete the orphan-tagger tags it."
item "🚫" "RolePolicy is NOT taggable (no tags field): on delete the orphan-tagger only removes the finalizer."
item "♻️ " "Both use deletionPolicy: Orphan, so deleting the Kubernetes objects keeps them in AWS."

step "State BEFORE create"
kube "📋" "All managed resources" get managed
awsm "📚" "All roles in moto" iam list-roles --query 'Roles[].RoleName'

confirm "Create these 2 resources?" || stop

step "Creating"
run k apply -f "$ROOT/demo-resources/"
explain "waiting for Crossplane to create them in the fake AWS..."
k wait role.iam.aws.upbound.io/demo-taggable-role --for=condition=Ready --timeout=5m
k wait rolepolicy.iam.aws.upbound.io/demo-non-taggable-rolepolicy --for=condition=Ready --timeout=5m

step "AFTER create"
explain "Kyverno added the gong.io/orphan-tagger finalizer to both (marked ◀ below)"
show_mrs
show_aws
summary created

confirm "Delete the 2 managed resources now?" || stop

T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
step "Deleting"
run k delete -f "$ROOT/demo-resources/" --wait=false
explain "The orphan-tagger finalizer holds them in Terminating until the watcher is done"
kube "⏳" "Terminating, held by the finalizer" get managed
k wait role.iam.aws.upbound.io/demo-taggable-role --for=delete --timeout=3m
k wait rolepolicy.iam.aws.upbound.io/demo-non-taggable-rolepolicy --for=delete --timeout=3m

step "What the watcher did"
k -n "$NS" logs deploy/orphan-tagger-watcher --since-time="$T0" \
  | grep -E "tagging |tagged |not taggable|removing finalizer|done:" || true

step "AFTER delete"
explain "The managed resources are gone from Kubernetes; both AWS objects survive (deletionPolicy: Orphan)"
show_mrs
show_aws
summary deleted

printf '\n%s   Done. Run it again with: make demo      Remove the lab with: make down%s\n' "$G" "$N"
