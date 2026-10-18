#!/usr/bin/env bash
# What `make up` installed, and with which command.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

step "Installed in the lab"

explain "Helm releases (Crossplane and Kyverno)"
run helm list -A --kube-context "kind-$CLUSTER"

explain "Crossplane AWS provider (the only one) and the CRDs it brought"
run k get providers.pkg.crossplane.io
printf '   AWS CRDs installed: %s\n' "$(k get crd -o name | grep -c '\.aws\.upbound\.io')"

explain "Kyverno policy that adds the finalizer"
run k get clusterpolicy

explain "Fake AWS (moto) and the orphan-tagger watcher"
run k get pods -n aws-mock
run k get pods -n "$NS" -l app=orphan-tagger-watcher

step "Right now"
explain "Managed resources in the cluster"
mr_table
explain "What is actually in the fake AWS (moto)"
aws_state
