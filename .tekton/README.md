Tekton CI definitions for this repository, rendered by the polling CI
service (gitlab.cee.redhat.com/klape/build). The poller:

* creates a PipelineRun from `build.yaml` for every PR head SHA and every
  push to the default branch
* substitutes the `repo-url`, `revision`, `image-name` and `push-latest`
  params and injects the shared RWX source workspace
* publishes images to the shared quay org as `<repo>:<sha>` (and `:latest`
  on default-branch pushes)
* reports results to the PR as `ci/build` plus per-task contexts
  (`ci/build/lint`, `ci/build/test`, `ci/build/build-and-push`)

See the build repo README for the full design and onboarding docs.
