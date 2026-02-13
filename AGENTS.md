# AGENTS

## Build Rule
- Every code change must keep the repository buildable with `make build`.
- Do not finish a change set unless `make build` succeeds.

## Test Rule
- For changes touching API or UI flow, run `make test` before finalizing.
