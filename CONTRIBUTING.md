# Contributing

Thanks for your interest in contributing to Gubernator! Please take a moment to review this document **before submitting a pull request**.

## Pull requests

**Please ask first before starting work on any significant new features.**

We love to code, and as such, we tend to code first and talk later. (wut? I haz to talk with people?) However, it's 
never a fun experience to have your pull request declined after investing joy, time and effort into a new feature. 

To avoid this from happening to you, we request that contributors create a GitHub issue and tag a maintainer, so we
can first discuss any new feature ideas. Your ideas and suggestions are welcome!

Bug requests are always welcome, but if the bug request requires a large refactor to fix, you might want to create
and issue and discuss.

### Fork and create a branch
If this is something you think you can fix, then fork https://github.com/gubernator-io/gubernator and create
a branch with a descriptive name.

### Run tests
You can run the test suite with all the bells and whistles by running
```bash
$ make test
```
### Run linter
We use a linter to ensure things stay pretty, ensure your code passes the linter by running
```bash
$ make lint
```

### Run Tiger
We run [Tiger](https://github.com/kapetan-io/tiger), a static analyzer for Go, over every package. Install it with
```bash
$ go install github.com/kapetan-io/tiger/cmd/tiger@latest
```
and run it with
```bash
$ make tiger
```
`scripts/tiger.sh` pins `GOTOOLCHAIN` to the `go` version in `go.mod`. Tiger uses whichever `go` is on your PATH when
it runs, and a newer one makes it fail on every package with `export data version N is greater than maximum supported
version`. The pin makes Go use the `go.mod` version, downloading that toolchain the first time if you don't have it.

`make tiger` fails when Tiger reports anything. Run `scripts/tiger.sh` directly to get Tiger's own exit code, which is
0 when clean, 1 when Tiger has findings and 2 when it couldn't check the code.

CI does not run Tiger. To see its findings before each push, turn on the committed hook once per clone (optional):
```bash
$ git config core.hooksPath .githooks
```
The hook prints Tiger's findings and never blocks the push. If `tiger` isn't installed it skips the step.

### Commits & Messages
We don't have a specific structure for commit messages, but please be as descriptive as possible and avoid
lots of tiny commits. If this is your style, please squash before making a PR, or we may squash on merge.

### Create a Pull Request
At this point, your changes look good, and tests are passing, you are ready to create a pull request. Please
explain WHY the PR exists in the description, and link to any related PR's or Issues.

## Merging a PR (maintainers only) 
A PR can only be merged into master by a maintainer if: CI is passing, approved by another maintainer and is
up-to-date with the default branch. Any maintainer is allowed to merge a PR if all of these conditions ae met.

## Shipping a release (maintainers only)
See [RELEASE.md](docs/RELEASE.md) for detailed procedures



