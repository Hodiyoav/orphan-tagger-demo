#!/usr/bin/env bash
# Command 2: delete both and watch the orphan-tagger do its job.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)

step "BEFORE delete"
mr_table
aws_state

step "Delete both managed resources"
run k delete -f "$ROOT/demo-resources/" --wait=false
explain "Kubernetes can't remove them yet: the orphan-tagger finalizer holds them in Terminating"
mr_table || true

step "Wait for the watcher to tag + release them"
k wait role.iam.aws.upbound.io/demo-taggable-role --for=delete --timeout=3m
k wait rolepolicy.iam.aws.upbound.io/demo-non-taggable-rolepolicy --for=delete --timeout=3m

step "What the watcher did"
k -n "$NS" logs deploy/orphan-tagger-watcher --since-time="$T0" \
  | grep -E "tagging |tagged |not taggable|removing finalizer|done:" || true

step "AFTER delete"
explain "The MRs are gone from Kubernetes"
mr_table
explain "Both AWS resources survive (deletionPolicy: Orphan); only the taggable one carries the orphan tag"
aws_state
