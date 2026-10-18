#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" || true
rm -rf "$STATE_DIR"
