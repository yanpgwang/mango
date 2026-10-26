# Changelog

## [0.1.0-alpha.5](https://github.com/yanpgwang/mango/compare/v0.1.0-alpha.4...v0.1.0-alpha.5) (2026-10-26)


### Features

* **workflows:** add opt-in Run coordinator recovery gate ([#278](https://github.com/yanpgwang/mango/issues/278)) ([0020331](https://github.com/yanpgwang/mango/commit/0020331d5acd836f2756da0c5a336b8385716938))
* **workflows:** close successful Runs through durable primary synthesis ([#283](https://github.com/yanpgwang/mango/issues/283)) ([57406c0](https://github.com/yanpgwang/mango/commit/57406c00198b49462421e798faf61553976613db))
* **workflows:** fence internal frontier decisions and grading ([#276](https://github.com/yanpgwang/mango/issues/276)) ([d5ba11f](https://github.com/yanpgwang/mango/commit/d5ba11fb273b7b5f71c1f87db4c51d59e6c95291))
* **workflows:** persist internal owned invocation receipts ([#274](https://github.com/yanpgwang/mango/issues/274)) ([b57f458](https://github.com/yanpgwang/mango/commit/b57f458d2d2adf84b73b8de0f3382e147b6bdd9b))


### Bug Fixes

* **workflows:** fence coordinator chains and make wake retries fair ([#279](https://github.com/yanpgwang/mango/issues/279)) ([4e90b6d](https://github.com/yanpgwang/mango/commit/4e90b6d0a207144891b274e137b62be4b297f91c))

## [0.1.0-alpha.4](https://github.com/yanpgwang/mango/compare/v0.1.0-alpha.3...v0.1.0-alpha.4) (2026-10-17)


### Features

* filter session threads by status ([#265](https://github.com/yanpgwang/mango/issues/265)) ([0831386](https://github.com/yanpgwang/mango/commit/0831386962c4ee4f1ec90af7c4dd9fd0647c3bf5))
* **release:** propose matched alpha versions with release-please ([#261](https://github.com/yanpgwang/mango/issues/261)) ([404aae7](https://github.com/yanpgwang/mango/commit/404aae7313d5c1d54ce99463347b23a83082e9a4))


### Bug Fixes

* **worker:** condition automatic idle Stop on the current durable end-turn so newer processed input and pending tools remain reclaimable

* **sdk:** recover persisted approvals from silent tool-runner event connections without repeating tools or extending idle deadlines
* **release:** preserve archive names across version PRs ([#264](https://github.com/yanpgwang/mango/issues/264)) ([d8cdf0c](https://github.com/yanpgwang/mango/commit/d8cdf0cd520d2318b26f90c4954f2313c4005f59))
* **sdk:** reconcile ambiguous Memory upload acknowledgements ([#263](https://github.com/yanpgwang/mango/issues/263)) ([c370105](https://github.com/yanpgwang/mango/commit/c370105810018aed0ce5fad2b53383506787a959))
