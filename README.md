# Orphan Tagger Demo

> Delete a Crossplane managed resource, keep the AWS resource, and still know it was orphaned.

A local, zero-cost lab (no AWS account) that shows how Gong's **orphan-tagger** works:

1. **Kyverno** adds a `gong.io/orphan-tagger` finalizer to every AWS managed resource (MR) at creation.
2. When an MR is deleted, the finalizer keeps it in `Terminating`.
3. The **orphan-tagger watcher** sees it, tags the real AWS resource `crossplane-orphaned=true`
   (taggable MRs only), then removes the finalizer so Kubernetes can finish the delete.

The AWS resource survives (`deletionPolicy: Orphan`) and is now discoverable by that tag.

## Prerequisites

`docker`, `kind`, `kubectl`, `helm`, and internet access (to pull images and charts).
`make up` took ~3 minutes in my test run; `create` ~3 s; `delete` ~5 s.

## Run it

```bash
make demo      # the whole thing, interactively
```

`make demo` does everything in one go and asks you three questions along the way:

1. **Prepares the lab:** kind + fake AWS + Crossplane + 1 AWS provider + Kyverno + orphan-tagger,
   and lists what it installed. No question asked.
2. **Shows the two manifests it is about to create, then asks `Create these 2 resources? [y/N]`.**
   It creates them and shows the result.
3. **Asks `Delete the 2 managed resources now? [y/N]`.** It deletes them, shows what the
   orphan-tagger did, and shows the result again.
4. **Asks `Remove the lab now? [y/N]`.** Yes removes the kind cluster; no keeps it for you to poke at
   (`make down` removes it later).

Answering anything but `y` stops safely and leaves the lab up. `make demo YES=1` answers yes to the
first two questions and never removes the lab on its own (handy for unattended runs).

Each result view (after create, after delete) shows, in separate icon-titled blocks:

- `kubectl get managed`, and the `role` and `rolepolicy` as YAML. The lines that matter are marked
  `◀`: the finalizers, `deletionPolicy`, the tags.
- the same objects as moto sees them, through the `awsm` commands listed below.
- a framed **🔎 What to look at** summary of the few facts worth checking.

Prefer step by step? The same flow is available as separate targets:

```bash
make up        # once: build the lab
make create    # create a taggable and a non-taggable AWS resource
make delete    # delete them, watch the tagger work, see the tag in the fake AWS
make down      # remove the lab
```

- `make up` ends by listing what it installed (also: `make status`).
- `make create` and `make delete` each print a **BEFORE** and **AFTER** view: the managed resources with
  their finalizers, and the IAM role's tags in the fake AWS. After `delete`, the
  `crossplane-orphaned=true` tag is highlighted.
- They can be repeated. On a re-run, BEFORE create shows the previous run's orphaned role and policy
  (still tagged); Crossplane then adopts it and resets its tags.

## What you will see

| Resource | Taggable? | What the orphan-tagger does on delete |
|----------|-----------|---------------------------------------|
| `Role` (IAM role) | yes, its CRD has `spec.forProvider.tags` | tags it `crossplane-orphaned=true`, then removes the finalizer |
| `RolePolicy` (inline policy) | no, no `tags` field | removes the finalizer only |

After the delete, the Kubernetes objects are gone but both AWS objects survive, and moto shows the
role carrying the new tag:

```
"Key": "crossplane-orphaned",  ◀
"Value": "true"
```

The policy is still in AWS too; it has no tags, so the orphan-tagger only released its finalizer.

## What is in the box

```
kind/                cluster config (own kubeconfig in /tmp, ~/.kube/config is never touched)
mock-aws/            moto (fake AWS) + a tagging shim + an aws-cli pod to inspect it
crossplane/          ONE provider (provider-aws-iam) + ProviderConfig pointing at moto
kyverno/             the orphan-add-finalizer ClusterPolicy (same as production)
orphan-tagger/       RBAC + Deployment, and watcher/ = the watcher's Go source + Dockerfile
demo-resources/      the two managed resources you create and delete
scripts/             demo (the interactive flow) + up / status / create / delete / aws-tags / down
```

Versions mirror Gong's: Crossplane 2.3.3, provider-aws v2.5.3.

## Fake AWS notes

- **moto** answers the AWS APIs over plain HTTP. The provider reaches it through the
  ProviderConfig `endpoint`; the watcher through the standard `AWS_ENDPOINT_URL` env var.
  The watcher's code is unchanged.
- moto's Resource Groups Tagging API (the call the watcher makes to tag) does not support IAM,
  S3 or EC2. A ~50-line proxy, `mock-aws/tagging-shim.py`, sits in front of moto and answers
  that one call for IAM roles by calling IAM `TagRole`; everything else passes through to moto.
  Without it the Role would stay stuck in Terminating.
- The cluster name can be overridden (`CLUSTER=my-lab make demo`) to run a second lab or to avoid
  clashing with someone else's on a shared Docker daemon. The kubeconfig then lives at
  `/tmp/my-lab/kubeconfig`.
- Credentials are the dummy `test`/`test`. Nothing leaves your machine.

## Useful extras

```bash
make logs        # follow the watcher
make aws-tags    # what the fake AWS holds right now
kubectl --kubeconfig /tmp/orphan-tagger-demo/kubeconfig get managed
```

Look at the role in moto itself (not the Crossplane MR):

```bash
alias awsm='kubectl --kubeconfig /tmp/orphan-tagger-demo/kubeconfig -n aws-mock exec deploy/aws-cli -- aws'

awsm iam get-role --role-name demo-taggable-role               # the role itself
awsm iam list-role-tags --role-name demo-taggable-role         # its tags (look for crossplane-orphaned)
awsm iam list-role-policies --role-name demo-taggable-role     # inline policy names
awsm iam get-role-policy --role-name demo-taggable-role --policy-name demo-policy   # the policy document
awsm iam list-roles --query 'Roles[].RoleName'                 # all roles in moto
```

The lab never touches `~/.kube/config`, so `kubectx` won't list it. To use it from your shell:

```bash
export KUBECONFIG=/tmp/orphan-tagger-demo/kubeconfig   # context: kind-orphan-tagger-demo
kubectx                                                # now lists it
```

Kyverno 3.9 warns that `ClusterPolicy` is deprecated. The policy is kept identical to production on
purpose, and the scripts hide that one warning from their `kubectl` output (the Helm install notes
still print a similar line).
