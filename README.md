# LumiPanel

A subscription and traffic management panel for proxy deployments. Go, single
binary, PostgreSQL.

**Status: early M0.** The pure domain layer, the schema and the project
scaffolding exist. Nothing serves traffic yet — `lumipanel serve` returns
"not implemented". See [Current state](#current-state).

This repository is software, not a service. It ships with no nodes configured
and no default endpoints.

---

## What it is

A control plane for proxy subscriptions: users, plans, orders, traffic
accounting, node configuration delivery, and subscription rendering for the
client applications people actually use.

Three principles run through the whole design, and most of the code makes more
sense read against them:

1. **Any invariant expressible as a database constraint becomes one.**
   Application logic has bugs; constraints do not.
2. **Authoritative decisions are never left to a scheduled job.** Expiry and
   quota are computed at the point of use. When the cron dies, authorisation
   stays correct.
3. **Side effects leave through the event bus.** A handler validates, writes
   its primary state, and publishes. Otherwise every new admin action is
   another chance to forget one of its consequences.

The full design document is [`docs/design/v1.0.md`](docs/design/v1.0.md). It is
the reference for every section number referenced in the code.

## Current state

Implemented and tested:

| Area | What is there |
|---|---|
| `internal/domain` | Traffic billing arithmetic, quota attribution, reset and period boundaries, order state machine, plan-change pricing |
| `internal/subplane` | User-Agent detection, protocol capability matrix, subscription intermediate representation |
| `internal/metering` | The Flusher's in-memory quota ladder |
| `internal/events` | Event bus and subscriber registry |
| `internal/platform/secret` | The credential type that refuses to serialize |
| `migrations/` | Full schema for the core and business tables |

Not started: HTTP layer, store layer, node plane, renderers, frontend,
importers. Anything touching the database or Redis needs integration
infrastructure that has not been set up yet.

`go test ./...` is green, including `-race`.

## Building

```sh
make            # fmt, vet, test, build
make race       # the race detector
make cross      # prove the single-binary claim across four platforms
```

Go 1.24+. There are no third-party dependencies yet, and `CGO_ENABLED=0` is
enforced in the Makefile and in CI — linking CGO would end cross-compilation
and static linking, and with them the single-binary deployment story.

## A few decisions worth knowing before reading the code

**Money is `int64` in the smallest currency unit.** No floats, no decimals.
`domain.Money`.

**Billing multipliers are integer basis points**, where `100` means `1.00x`.
Floating point makes the same total traffic bill differently depending on how
it was batched, and the resulting discrepancy cannot be reconciled because no
individual row is wrong — only the sum. `domain.RateBP`.

**Traffic attribution has a fixed order**: data packs by soonest expiry, then
the primary subscription. Anything left over after every subscription is
exhausted becomes a row against subscription 0, which is a sentinel rather than
NULL — PostgreSQL treats NULLs in a unique index as distinct, which would make
the ledger's uniqueness constraint unenforceable. `domain.Attribute`.

**User-Agent matching is an ordered slice, not a map.** Go randomises map
iteration, and `Clash Verge Rev` matches both `clash` and `clash-verge`, so a
map hands the same user a different format on each refresh.
`subplane.DetectUA`, with 68 fixtures in `testdata/ua/`.

**The render entry point cannot see a user ID.** Node visibility is decided
entirely by permission group. A user-level filter would make every group-keyed
cache either useless or capable of serving one user another user's nodes.
`subplane.Selector`, with a test that parses the source to keep it true.

**Credentials cannot be serialized into a response.** `secret.Secret` fails to
marshal rather than redacting, so a leak is a failed request instead of a quiet
disclosure. A test walks the response-building packages to make sure nothing
unwraps one.

## Open decisions

[`docs/decisions/C-1-relay-chain.md`](docs/decisions/C-1-relay-chain.md) —
relay chain model and billing basis. **Awaiting sign-off.** The schema already
follows the proposal; reversing it is cheap now and expensive after the
metering core lands.

Appendix C of the design document lists five more (C-2 through C-6) that do not
block the schema.

## Layout

```
cmd/lumipanel/      the single binary
internal/
  domain/           pure logic, no IO
  metering/         traffic accounting
  events/           domain event bus
  subplane/         northbound: subscription rendering
  nodeplane/        southbound: node protocol
  platform/         secret, httpx, and the rest of the plumbing
migrations/         schema
docs/design/        the design document
docs/decisions/     decisions taken against it
testdata/           golden fixtures
```

The dependency rule is `api → service → domain ← store`. `domain` imports no IO
package. `metering` is the one non-service package allowed to touch the store
directly, and only where a comment says why.
