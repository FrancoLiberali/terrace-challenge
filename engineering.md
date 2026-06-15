# Engineering Practices

This document covers the practices applied while building the detector — testing discipline, code review, CI, quality gates, security, reproducibility — that produced everything described in the other docs. The brief asked for a working detector; these practices are how I'd build any system I plan to keep alive, not specific to this challenge.

If the rest of the documentation answers *what* was built and *why*, this one answers *how* it was built.

---

## Verification: defence in depth

Multiple layers, each catching a different class of failure. No single layer carries the weight on its own:

- **Unit tests** at 87.5% statement coverage on `internal/` (the `cmd/` packages are wiring + diagnostics, exercised by smoke tests rather than unit tests, and are deliberately excluded from the coverage metric). The race detector is on in both local runs (`make test`) and CI.
- **Live smoke tests against real venues** at the close of every plan-step. Either a probe binary against the integration in question (`probe-binance`, `probe-uniswap`, `probe-chain`) or the full pipeline in `arbd`, with the result captured in the PR description. Catches integration drift the unit suite can't see.
- **CI** runs `gofmt`, `go vet`, `golangci-lint`, `go build`, `go test -race`, coverage, and SonarCloud scan on every PR. Failure blocks merge.
- **SonarCloud quality gate** + coverage tracking layered on top — second opinion from a tool that catches duplication, complexity, and security smells the Go ecosystem isn't strong on.

Each layer catches a different shape of defect. Race detector catches concurrency bugs unit tests miss; smoke tests catch integration drift the unit suite can't see; SonarCloud catches code smells the Go-native linting doesn't.

## Code review through PRs

Every plan-md step landed as a separate PR. Each PR description follows the same shape:

- **Summary** — what changed and why.
- **Design rationale** — including alternatives considered, not just the choice taken.
- **Test plan** — what was verified, how.
- **Smoke-test evidence** when the change touches the runtime path (timestamps, candidate counts, breaker-trip transitions, etc).

Refactors that emerged from review — e.g. moving the resilience layer from per-Snapshotter to per-HTTP-host after a design discussion — landed as their own follow-up PRs rather than being smuggled into unrelated work. Keeps diff scope small and reviewable.

Commit messages explain the *why*, not the *what*. The diff already shows what changed; the message exists to tell future-me (or future-someone) why that change was the right one at the time.

## Quality gates that fail builds

The strict bias: build fails noisily, not silently:

- **`golangci-lint`** runs with `default: all`. Most linters are enabled by default. The few exceptions are documented inline with a one-line justification each (`exhaustruct`: tedious without payoff; `paralleltest`: too many false positives; etc). Suppressions inside the code are inline `//nolint` directives with a required explanation — `nolintlint.require-explanation: true` means a suppression without rationale fails the build.
- **`gofmt`** enforced via a CI step that fails if any file is unformatted.
- **`go test -race`** is mandatory in CI; race-detected failures are treated as build failures, not flakes.
- **SonarCloud quality gate** must be green to merge.

The result is that "lint is green" carries real meaning. When a teammate sees the green checkmark on a PR, they know the code passes every standard the repo cares about.

## Security discipline

Small repo, light surface, but the right defaults baked in:

- **Third-party GitHub Actions pinned to commit SHAs** (Sonar hotspot rule `githubactions:S7637`). The version tag stays as a trailing comment for human readability, but the action reference is the immutable SHA. Prevents tag hijacking — a hostile or compromised maintainer can't repoint `v8` at a malicious commit.
- **Dependabot** watches versions of both GitHub Actions and Go modules; minor / patch bumps land routinely, major bumps reviewed by hand.
- **Secrets** live in `.env` (gitignored) or come in at runtime via `docker run --env-file .env`. Never baked into images, never logged.
- **Config that's not secret** (URLs, fee tiers, contract addresses) lives in committed `.env.example` and `config.yaml` so a reviewer can see exactly what knobs exist.

## Reproducibility

A reviewer should be able to clone, configure, and run in minutes — and get the same result anyone else does:

- **`Dockerfile`** produces an 18MB distroless image with the static binary. Same behaviour on any host with Docker.
- **`Makefile`** abstracts toolchain. `make test`, `make lint`, `make docker-build`, `make docker-run` work the same locally and in CI.
- **`config.yaml`** committed with sensible defaults. Out-of-the-box config detects ETH-USDC arbitrage on mainnet.
- **`.env.example`** documents every required env var with a comment explaining its purpose.

The README quickstart targets five minutes from `git clone` to first block evaluated.

## Documentation as a deliverable

Docs are treated on equal footing with code:

- The doc chain (`business.md` / `limitations.md` / `architecture.md` / `plan.md` / `implementation.md`) covers problem framing → simplifying assumptions → design rationale → sequencing → Go-level structure. Each is current with what was actually built, not aspirational.
- **`architecture.md`** decisions use a numbered format with the chosen approach AND the alternatives considered. A reader can see what was weighed against what; nothing reads as arbitrary.
- **`limitations.md`** doubles as a production backlog — each entry is framed as "what changes if we shipped this for trading," so the doc is useful as more than just a list of caveats.
- The split of *where each kind of documentation lives* is explicit: PR descriptions for transient context, godoc for symbol-level API contracts, `.md` docs in the repo root for architectural lessons that outlive any specific change.

## Incremental delivery

`plan.md` divides the challenge into seven phases. Each phase:

- Is **independently runnable end-to-end** (probe-binance after phase 1, full arbd after phase 5, etc).
- Closes with a **live smoke test against real venues**.
- Lands as a **PR with smoke-test evidence** in the description.

The **integration-first ordering** (real venues before composition) bounds risk early: by the time the Pathfinder and Evaluator are built, the integrations they depend on are already proven against mainnet. The riskiest unknowns (does QuoterV2 return what we think? Does the WS reconnect cleanly?) are settled in steps 1–3 against real services, not deferred.

**Types emerge on demand.** The unified `Quote` / `Quotes` shape doesn't exist in step 1; it crystallises in step 4 once two adapters are pushing into a shared shape. Defining it in step 1 would have been guessing.

---

## Why this matters

The brief was "produce a working detector." The practices above are how I'd build any system I expect to outlive the first commit. Whatever's shipped from here can be picked up by someone else and modified safely, because:

- The tests catch regressions before merge.
- The CI tells you the truth about every commit.
- The lint config carries the team's standards.
- The docs explain *why* the code looks the way it does.
- The PR history shows the order decisions were taken in and what alternatives were considered.

Treating these as part of the delivery, not overhead on top of it, is the shape of work I'm trying to demonstrate.
