---
title: Alpha distribution acceptance
description: Install one reviewed runtime, chart and SDK candidate before publication.
---

# Matched alpha distribution acceptance

Delivery 4 fulfills the Kubernetes developer-preview alpha scope confirmed on
2026-10-17: users install runtime, chart and first-party SDK artifacts from one
reviewed source revision without building Mango. Required cluster acceptance
exercises installation, Mango process restart and Work recovery using the actual
packaged chart, native supervisor archive and exported OCI images, with retained
external state services.

Extend the existing clean-source candidate builder with standard Helm package output; reject Chart version/appVersion mismatches, include the chart in the common manifest/checksums, inspect the packaged chart with Helm. Record each validated OCI top-level index digest alongside platforms so exact identity can be selected for importing and publication. Keep tooling restricted to the existing known Buildx layout; do not build a general importer.

The opt-in Kubernetes harness accepts a candidate directory. Validate identity, names and streaming artifact hashes before consuming artifacts. Load native-platform OCI exports through the Docker containerd image store, tag only unique fixture aliases, inspect OS/architecture/numeric user/version/revision, extract the native supervisor from its verified archive, and install the packaged chart. Only the explicitly simulated model fixture is built from source. Default cluster CI retains its existing source-build path.

The manual read-only Release candidate workflow adds verified Helm/kind/kubectl
tooling, a containerd Docker image store and artifact-based supported lifecycle
acceptance after finalizing checksums. It uploads only after that acceptance
passes; no automatic publication or registry credential is introduced. Linux
AMD64 CI and local macOS ARM64 exercise both supported image platforms. Existing
independent HTTP/SDK/service tests retain their own roles, including active Work
across Mango runtime restart.

Independent external Temporal/database cold backup and restore is outside this
alpha promise. Its complete test remains explicitly runnable with
`make test-kubernetes-restore` and the manual cold-restore workflow; failures
remain failures. The [known main-run exit](https://github.com/yanpgwang/mango/actions/runs/37417003487)
is unresolved. Successful unchanged diagnostic runs did not establish a fix.
This classification changes release acceptance, not runtime behavior, fixture
resources or the original restore assertions.

After review and exact-head CI, build one candidate at the merged reviewed source. Publish those inspected bytes and matching version tags; verify registry identity and public retrieval before claiming availability. Release notes record actual revision, payload hashes, OCI index digests, workflow/acceptance evidence, fresh-install commands, external sandbox/state responsibilities and limitations. Credentials are needed only for publication, never development or CI.

Non-goals: new API/SDK/runtime behavior, cloud sandbox, bundled dependencies,
external-state cold restore support, upgrades/rollback, live backup or HA
promises, hosted-agent runtime dependencies, general OCI import/registry framework.
