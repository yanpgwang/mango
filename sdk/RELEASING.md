# SDK alpha releases

## Scope and acceptance criteria

Mango users should be able to install a real, working client from PyPI or npm
under the same distribution name, `mango-sdk`, without cloning the runtime.
The first candidates are Python `0.1.0a1` and TypeScript `0.1.0-alpha.1`.
Python's import remains `mango_sdk`; TypeScript's import is `mango-sdk`.

Acceptance requires generated-source checks, language tests, HTTP conformance,
and installation from the actual wheel/sdist/npm tarball outside the checkout.
Inspect those archives for the public entrypoints, generated bindings, type
information and license, and exclude credentials, local configuration, caches
and development dependencies. After upload, verify registry metadata and repeat
the installation/import smoke test using the exact published versions.

These are usable alpha clients, not empty packages that reserve a name. An alpha
publication does not establish a stable Mango API or change the pre-release
policy. Server routes, persistence, retries, authentication and Go module paths
do not change. A hosted runtime, automatic deployment, new SDK languages and a
Go tagged release are outside this slice.

## Build and verify

From the repository root:

```sh
make sdk-install
make sdk-test
make sdk-conformance
uv build --no-sources --out-dir sdk/python/dist sdk/python
mkdir -p sdk/typescript/artifacts
(cd sdk/typescript && npm pack --pack-destination artifacts)
```

The Python build produces both a source distribution and a wheel in
`sdk/python/dist/`. The npm package's `prepack` script compiles the checked-in
bindings and transport; its allowlist distributes `dist/`, README and LICENSE
alongside package metadata. npm archives go under `sdk/typescript/artifacts/`,
outside the shipped `dist/` tree so repacking cannot accidentally bundle an older
tarball. Build output and package archives are not committed.

Use fresh environments outside this checkout to install and smoke-test all
three artifacts. A successful import must resolve from the installed package,
not a local source directory or an editable install. Verify TypeScript
declarations using the public `mango-sdk` import as well as running JavaScript.

## Publish only after an explicit release request

Confirm that the package name is available or owned by the releasing account.
Authenticate to the official registries on the maintainer's machine. Never put
passwords or tokens into this repository, terminal command arguments, logs or
chat messages. PyPI uses an API token or a configured Trusted Publisher, not an
account password. npm may require browser login and interactive two-factor
authentication. Missing credentials block publication, not local validation.

Publish the inspected artifacts, rather than rebuilding during upload:

```sh
uv publish --publish-url https://upload.pypi.org/legacy/ \
  sdk/python/dist/mango_sdk-0.1.0a1-py3-none-any.whl \
  sdk/python/dist/mango_sdk-0.1.0a1.tar.gz
npm publish sdk/typescript/artifacts/mango-sdk-0.1.0-alpha.1.tgz \
  --registry=https://registry.npmjs.org/ --access public --tag alpha
```

Provide the PyPI token through the publishing tool's secure local credential
mechanism (for example, a process-local `UV_PUBLISH_TOKEN` supplied by the user).
Do not change npm's `latest` tag to advertise an alpha release as stable.
When an upload has an uncertain result, query registry metadata before retrying;
never overwrite or reuse a released version with different contents. If only
one registry succeeds, report the partial release explicitly.

Record the source commit, package versions, artifact SHA-256 hashes and registry
URLs with the release result. Candidate configuration and passing tests do not
mean publication occurred: only verified registry uploads establish that.
