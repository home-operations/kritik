# Changelog

## 0.1.0 (2026-09-26)


### ⚠ BREAKING CHANGES

* **gateway:** the worker is the model gateway; no provider key enters a runner ([#27](https://github.com/home-operations/kritik/issues/27))
* **egress:** runner pods leave the cluster only through the worker gateway ([#24](https://github.com/home-operations/kritik/issues/24))
* **store:** flatten the migrations into one schema ([#22](https://github.com/home-operations/kritik/issues/22))
* **store:** index embeddings with VectorChord ([#21](https://github.com/home-operations/kritik/issues/21))
* **review:** render comments with text/template and sprout ([#17](https://github.com/home-operations/kritik/issues/17))
* forgejo forge, agentic reviews, in-repo config and templated output ([#15](https://github.com/home-operations/kritik/issues/15))

### Features

* **agent:** run allowlisted commands over a checkout of the head ([#26](https://github.com/home-operations/kritik/issues/26)) ([e5d3935](https://github.com/home-operations/kritik/commit/e5d3935b4d408459f381f14f3d35d176005f0220))
* **egress:** runner pods leave the cluster only through the worker gateway ([#24](https://github.com/home-operations/kritik/issues/24)) ([416e778](https://github.com/home-operations/kritik/commit/416e778cac256893c17ffb85a67fbfe17cb2ec93))
* forgejo forge, agentic reviews, in-repo config and templated output ([#15](https://github.com/home-operations/kritik/issues/15)) ([6f34e8e](https://github.com/home-operations/kritik/commit/6f34e8ed8f9bda52f8aefcb995de3ad6079baecf))
* **gateway:** the worker is the model gateway; no provider key enters a runner ([#27](https://github.com/home-operations/kritik/issues/27)) ([38eaaa8](https://github.com/home-operations/kritik/commit/38eaaa80d0b13e604a6e1c505fadfc4ded8078b3))
* **go:** update module github.com/odvcencio/gotreesitter (v0.54.0 → v0.55.0) ([#11](https://github.com/home-operations/kritik/issues/11)) ([4b797a8](https://github.com/home-operations/kritik/commit/4b797a8cb09e534078ed302b183b78dea6546445))
* **go:** update module github.com/openai/openai-go (v1.12.0 → v3.66.0) ([#5](https://github.com/home-operations/kritik/issues/5)) ([cefd5b2](https://github.com/home-operations/kritik/commit/cefd5b2d36bab6040cf03eff96e0151ae05a9e04))
* **go:** update module golang.org/x/oauth2 (v0.36.0 → v0.37.0) ([#35](https://github.com/home-operations/kritik/issues/35)) ([4cc35e2](https://github.com/home-operations/kritik/commit/4cc35e2fed6af297bd89f9243040b567f63c29ef))
* initial import of the kritik review service ([e27f048](https://github.com/home-operations/kritik/commit/e27f048e560ea3d8235ae816ed350fcb9f87b964))
* **npm:** update dependency oxfmt (0.69.0 → 0.70.0) ([#4](https://github.com/home-operations/kritik/issues/4)) ([f311a0f](https://github.com/home-operations/kritik/commit/f311a0f7851f574c163a333e8512b4884310ff5c))
* **review:** offer a finding's fix as a suggestion and an agent prompt ([#19](https://github.com/home-operations/kritik/issues/19)) ([380bbe0](https://github.com/home-operations/kritik/commit/380bbe0bd06d4fff0b8335b0fd75c69245d83a3d))
* **review:** render comments with text/template and sprout ([#17](https://github.com/home-operations/kritik/issues/17)) ([15525ad](https://github.com/home-operations/kritik/commit/15525ad5a60957a05e92ff0005c92d4f03e8a7a1))
* **review:** sharpen what the reviewer reports ([#18](https://github.com/home-operations/kritik/issues/18)) ([b4d5520](https://github.com/home-operations/kritik/commit/b4d55207ce93f81a5878226bbc261308a184e8d7))
* **runner:** run runner Jobs under a RuntimeClass ([#25](https://github.com/home-operations/kritik/issues/25)) ([8674b2a](https://github.com/home-operations/kritik/commit/8674b2ae64b72e4175e0971af870169f0f4a7a39))
* **store:** index embeddings with VectorChord ([#21](https://github.com/home-operations/kritik/issues/21)) ([23de86e](https://github.com/home-operations/kritik/commit/23de86e1d24721fe676fcfa2941dff3aec589069))
* web dashboard with sign-in, transcripts and dashboard-managed config ([#30](https://github.com/home-operations/kritik/issues/30)) ([9b8d02b](https://github.com/home-operations/kritik/commit/9b8d02bb6810e0a4d72927b8a60c84e22b950554))


### Bug Fixes

* **gateway:** reserve each step against the run's budget ([#29](https://github.com/home-operations/kritik/issues/29)) ([686aeb0](https://github.com/home-operations/kritik/commit/686aeb05aa14882a17cfd18856170431514c1c90))
* **go:** update module github.com/go-git/go-billy/v5 (v5.9.0 → v5.9.1) ([#2](https://github.com/home-operations/kritik/issues/2)) ([1f9a6b8](https://github.com/home-operations/kritik/commit/1f9a6b8c80f8994cf90f6a1980240fec3a47fa91))
* **go:** update module github.com/go-jose/go-jose/v4 (v4.1.4 → v4.1.5) ([#34](https://github.com/home-operations/kritik/issues/34)) ([054d550](https://github.com/home-operations/kritik/commit/054d55041b49364043abaff74e0fb94220e36273))
* keep the git token out of the Job spec, drop dead leader sessions, cap under the lease ([#13](https://github.com/home-operations/kritik/issues/13)) ([9f3ee1c](https://github.com/home-operations/kritik/commit/9f3ee1cd1ba56009a624472fbdf669af1377daac))
* **poller:** record a first poll's older pull requests as a baseline ([#38](https://github.com/home-operations/kritik/issues/38)) ([196bbb0](https://github.com/home-operations/kritik/commit/196bbb024344c0eb3dddcf71c62e57a93cfca66f))
* **review:** no doubled blank line without findings, no "cannot verify" in the take ([#20](https://github.com/home-operations/kritik/issues/20)) ([9cc8709](https://github.com/home-operations/kritik/commit/9cc870929cc9bfcf967a430e5a79047132cb0a28))
* **store:** expire the indexes of repositories disabled past their grace ([#43](https://github.com/home-operations/kritik/issues/43)) ([1c0ce18](https://github.com/home-operations/kritik/commit/1c0ce18a7626562ead43162fc2ec8f2ecefb8c1a))
* **worker:** bound jobs above their runner deadline and delete orphaned Jobs ([#12](https://github.com/home-operations/kritik/issues/12)) ([6a40635](https://github.com/home-operations/kritik/commit/6a406358d353d5604971506c0fbfe11e049c52f7))
* **worker:** clear staged chunks when an index job fails ([#42](https://github.com/home-operations/kritik/issues/42)) ([ab40ef8](https://github.com/home-operations/kritik/commit/ab40ef8abf9442ce37a1ddcd911ed9026721606d))
* **worker:** pace the index queue ([#37](https://github.com/home-operations/kritik/issues/37)) ([bb43a03](https://github.com/home-operations/kritik/commit/bb43a03c5e54cfb644e96fbd5f88047a588a00b0))
* **worker:** snooze a review while every model slot is held ([#33](https://github.com/home-operations/kritik/issues/33)) ([3fe2df7](https://github.com/home-operations/kritik/commit/3fe2df714d64505b3743dc3b5cdd4923b8799ad7))


### Performance Improvements

* **worker:** skip an unchanged bot rebase before starting its runner ([#32](https://github.com/home-operations/kritik/issues/32)) ([a3f5569](https://github.com/home-operations/kritik/commit/a3f55695c8617acca68be09b9b5a886ddb45f417))


### Code Refactoring

* deduplicate helpers and remove quadratic loops ([#41](https://github.com/home-operations/kritik/issues/41)) ([af1ca71](https://github.com/home-operations/kritik/commit/af1ca7170e4847b68296274b314e6bb2c0909fb0))
* modernise for Go 1.27, share the worker plumbing, widen unit tests ([#10](https://github.com/home-operations/kritik/issues/10)) ([0374c74](https://github.com/home-operations/kritik/commit/0374c740004bf85db7c443025107bb62eb1f5fc9))
* **store:** flatten the migrations into one schema ([#22](https://github.com/home-operations/kritik/issues/22)) ([f0ce264](https://github.com/home-operations/kritik/commit/f0ce2640c7ec8d1e36a8b6861f8f288964539b6d))


### Documentation

* **adr:** add ADR-0010 on configuration layers and precedence ([#44](https://github.com/home-operations/kritik/issues/44)) ([e0e038f](https://github.com/home-operations/kritik/commit/e0e038fcf82cb0b357365056db488ea64782af73))
* **adr:** ADR-0003, the review is an agent in the runner, the worker its model gateway ([#14](https://github.com/home-operations/kritik/issues/14)) ([c30b386](https://github.com/home-operations/kritik/commit/c30b386ad3d1e3bba31e82481f9a4ae9c3f1b82f))
* **adr:** ADR-0008, allowlisted commands in the agent and egress through the gateway ([#23](https://github.com/home-operations/kritik/issues/23)) ([501b449](https://github.com/home-operations/kritik/commit/501b4498d3a684c68f2c93867f4095fd96ec2c39))
* **adr:** renumber the gateway ADR as 0004 amending the agentic mode ([#16](https://github.com/home-operations/kritik/issues/16)) ([ad9f727](https://github.com/home-operations/kritik/commit/ad9f72755ff2af2a09f027bc6e639b2fc067ac2f))
* **chart:** state the bound on concurrent runner pods ([#39](https://github.com/home-operations/kritik/issues/39)) ([03c8f65](https://github.com/home-operations/kritik/commit/03c8f65877de35fb7296bb12d57bd7734daf7ef0))
* slim the README and flag kritik as not production ready ([#40](https://github.com/home-operations/kritik/issues/40)) ([9d9581b](https://github.com/home-operations/kritik/commit/9d9581bf640a3ee2b07deeed318d0286af3c5827))


### Tests

* **gateway:** count only the suite's own gateway tokens ([#28](https://github.com/home-operations/kritik/issues/28)) ([2b7cc81](https://github.com/home-operations/kritik/commit/2b7cc8177a9d207329a14b80c88632649ca9f343))


### Build System

* **chart:** keep the generated README and schema out of the formatter ([#9](https://github.com/home-operations/kritik/issues/9)) ([1e3fd18](https://github.com/home-operations/kritik/commit/1e3fd187d4b4ff0443668ee0ce295c79f966d14f))
* **mise:** lock node without a machine-local compile option ([#31](https://github.com/home-operations/kritik/issues/31)) ([8082243](https://github.com/home-operations/kritik/commit/8082243c20c36b242f694f64578bab88e6c80f99))
* **mise:** stop tracking the lock sidecars ([#8](https://github.com/home-operations/kritik/issues/8)) ([b695f42](https://github.com/home-operations/kritik/commit/b695f4264afaedfd1e39e3c1a7709232362add5b))


### Continuous Integration

* **release:** start the version series at 0.1.0 ([#7](https://github.com/home-operations/kritik/issues/7)) ([120b478](https://github.com/home-operations/kritik/commit/120b4782a46ecc81ac28a567a7d5e0c55a38f169))


### Miscellaneous Chores

* **mise:** update tool oxfmt (0.69.0 → 0.70.0) ([#3](https://github.com/home-operations/kritik/issues/3)) ([ec201c5](https://github.com/home-operations/kritik/commit/ec201c50f5dac14efba42d6526a6e776dcec637d))
