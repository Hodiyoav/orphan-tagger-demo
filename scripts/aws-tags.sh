#!/usr/bin/env bash
# Ask the fake AWS what it holds right now.
#
# To look at the role in moto yourself (not the Crossplane MR), via the aws-cli pod:
#
#   alias awsm='kubectl --kubeconfig /tmp/orphan-tagger-demo/kubeconfig -n aws-mock exec deploy/aws-cli -- aws'
#
#   awsm iam get-role --role-name demo-taggable-role               # the role itself
#   awsm iam list-role-tags --role-name demo-taggable-role         # its tags (look for crossplane-orphaned)
#   awsm iam list-role-policies --role-name demo-taggable-role     # inline policy names
#   awsm iam get-role-policy --role-name demo-taggable-role --policy-name demo-policy   # the policy document
#   awsm iam list-roles --query 'Roles[].RoleName'                 # all roles in moto
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

aws_state
