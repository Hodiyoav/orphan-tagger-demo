#!/usr/bin/env bash
# What `make up` installed, and with which command.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

step "Installed in the lab"

item "📦" "Helm releases: Crossplane and Kyverno"
run helm list -A --kube-context "kind-$CLUSTER"

item "🔌" "Crossplane AWS provider (the only one) and the CRDs it brought"
run k get providers.pkg.crossplane.io
printf '   AWS CRDs installed: %s\n' "$(k get crd -o name | grep -c '\.aws\.upbound\.io')"

item "🛡️ " "Kyverno policy that adds the finalizer"
run k get clusterpolicy

item "☁️ " "Fake AWS (moto + tagging shim)"
run k get pods -n aws-mock

item "🏷️ " "orphan-tagger watcher"
run k get pods -n "$NS" -l app=orphan-tagger-watcher

step "Right now"
explain "Managed resources in the cluster"
mr_table
explain "What is actually in the fake AWS (moto)"
aws_state
