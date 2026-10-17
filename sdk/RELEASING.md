# Matched alpha releases

Publish the runtime, chart and first-party SDKs from one reviewed source revision
and one accepted candidate. This checkout targets runtime/TypeScript/Go
`0.1.0-alpha.3` and Python `0.1.0a3`. The native distribution name is `mango-sdk`;
Python imports `mango_sdk`, TypeScript imports `mango-sdk`, and the Go module is
`github.com/yanpgwang/mango/sdk/go`.

Follow the [candidate guide](../docs/guides/release-candidates.md) to build and
inspect command archives, wheel/sdist/npm/Go packages, chart and both OCI images.
Acceptance includes generated-source checks, language tests, independent HTTP
conformance, fresh package installations outside the checkout and the supported
Kubernetes lifecycle. Development and ordinary CI require no registry or hosted
agent credentials.

After the exact merged source's required checks pass, run **Release candidate**
on `main`, recording its successful run ID. Then use **Publish developer preview**
with that run, version and full source revision. Start with `publish=false`.
Configure the account-side Trusted Publishers and GHCR package access described
in the [publication design](../docs/design/alpha-publication.md), then explicitly
set `publish=true` to promote those same bytes. Do not rebuild SDK packages or
replace an accepted version as part of publication.

The publisher validates existing versions, bootstraps and anonymously verifies
both OCI indexes, transfers missing native SDK distributions with OIDC, creates
exact source tags (including `sdk/go/v0.1.0-alpha.3`), verifies public Go retrieval,
and checks release assets before making the GitHub prerelease public. The
release record carries checksums, digests, source and workflow evidence, install
instructions and the supported developer-preview boundary. A failed or partial
publication is not an available matched release; retry the same candidate,
reusing only identical bytes.

The historical [alpha 1 record](releases/0.1.0-alpha.1.md) documents the earlier
SDK interface. An alpha publication does not establish a stable Mango API or
change the repository's pre-release policy.
