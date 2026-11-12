# grid-environment-operator

Stamps complete, multi-site AI Grid environments on demand.

An `AIGrid` custom resource produces a working grid: HyperShift LWE sites
(hub, inference, ingress), the cross-site networking they need on a shared
host cluster (Submariner cable policies, the DNS workaround for nested
HyperShift topologies), a GitOps-driven grid installation, and a
ready-to-use inference endpoint in `status.ingressEndpointURL`.

Design document: `scratchpad/proposals/aigrid-operator-design.md` in the
workspace scratchpad.

## Status

First draft — implementation complete enough to review end to end, not yet
run against a live cluster. Known review points:

* The import flow applies the hub-generated `crds.yaml`/`import.yaml`
  documents verbatim; the stale-ManagedCluster detection (guest missing its
  klusterlet namespace) is implemented but only exercised by hand during
  the live validation of the underlying workaround.
* The root Argo Application assumes the GitOps repo layout deploys the
  full hub + spoke fan-out from a single root path, matching the demo
  repository. `spec.destination.namespace` handling may need adjusting per
  repo.
* `SiteStatus.KubeconfigSecret` is declared but not populated; CI reads
  kubeconfigs from the LWEs' `status.kubeconfig` directly today.
* The hub is deliberately **not** a Submariner participant (no gateway
  node label, no `local-cluster` submariner labels): the grid's data plane
  is spoke-to-spoke and hub communication rides ACM channels. The demo
  had the hub labeled for submariner and observability, so the first live
  run should watch for membership or metrics issues and add the labeling
  if the simpler hypothesis is wrong.

## Layout

* `api/v1alpha1` — the AIGrid CRD types, conditions, generated deepcopy
* `cmd/manager` — manager entrypoint
* `internal/controller` — the phase machine:
  * `sites.go` — Provisioning: LWE creation, drift, readiness, expiration
  * `import.go` — Importing: ManagedClusters, stale-registration cleanup,
    import document application
  * `guestdns.go` + `zone.go` + `policies.go` — Wiring: the per-grid
    CoreDNS zone server, host-side NetworkPolicies, guest
    `dns.operator` forward zones (read-modify-write, preserving foreign
    entries such as Lighthouse's)
  * `gitops.go` — GitOpsSynced + GridReady: role labels, root
    Application, OperatorGroup annotations, GridSite conditions, the
    consumer gateway's ingress Route
* `config/` — generated CRD and RBAC, a sample, and the Tekton pipeline

## Build

```sh
make build     # compile the manager
make test      # unit tests (zone records, DNS merge, policy shapes)
make generate  # regenerate deepcopy + CRD + RBAC after API changes
```

## Running without cluster-admin

The operator's service account is a hard prerequisite — it never runs with
admin credentials:

```sh
# once, with an admin kubeconfig: namespace, service account, generated
# ClusterRole, and binding
kubectl apply -f config/rbac/ --kubeconfig <admin-kubeconfig>

# mint a scoped kubeconfig for devcontainer runs (admin is used only to
# mint the token, never to run the operator)
tools/dev-kubeconfig.sh <admin-kubeconfig> > /tmp/aigrid-dev.kubeconfig
KUBECONFIG=/tmp/aigrid-dev.kubeconfig go run ./cmd/manager
```

Note on RBAC generation: the `+kubebuilder:rbac` markers live in
`internal/controller/doc.go` at the package level, because controller-gen
collects package-target markers only from package-clause doc comments in
this toolchain — struct- or function-attached markers are silently
ignored. `make generate` regenerates `config/rbac/role.yaml` from them.

## Tekton

`config/tekton` mirrors the llm-workload-environment-operator pipeline:
clone, test, vet, buildah image build, optional push. Apply the pipeline
and create a PipelineRun (requires a
`grid-environment-operator-registry-auth` secret and a builder
service account when pushing).

## Hard-won invariants encoded here

These came from live debugging on the pirate cluster; do not remove them
without understanding why they exist:

* NetworkPolicy egress must match **post-DNAT** destinations — allow the
  pod port (5353), not just the service port (53).
* ConfigMap changes do not restart pods; the operator owns restarts and
  verifies config changes with live DNS queries.
* The guest `dns.operator` servers list is merged, never replaced —
  Lighthouse's `clusterset.local` entry lives there.
* Site recreation under the same name requires deleting the hub-side
  ManagedCluster first.
* The grid's `GridSite Connected` condition is the end-to-end probe; the
  operator never patches grid objects (the grid-operator reverts them) —
  names are repointed via DNS instead.
