#!/usr/bin/env bash
# Command 1: create one taggable + one non-taggable AWS managed resource.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

step "BEFORE create"
mr_table
aws_state

step "Create a taggable (Role) and a non-taggable (RolePolicy) AWS resource"
run k apply -f "$ROOT/demo-resources/"
explain "waiting for Crossplane to create them in the fake AWS..."
k wait role.iam.aws.upbound.io/demo-taggable-role --for=condition=Ready --timeout=5m
k wait rolepolicy.iam.aws.upbound.io/demo-non-taggable-rolepolicy --for=condition=Ready --timeout=5m

step "AFTER create"
explain "Kyverno added the gong.io/orphan-tagger finalizer to both MRs"
mr_table
explain "AWS has the role with its own tags only: no crossplane-orphaned yet"
aws_state
printf '\n%s   Next:  make delete%s\n' "$G" "$N"
