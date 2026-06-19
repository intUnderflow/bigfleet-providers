# Provider template

A minimal, **compiling** BigFleet capacity provider that passes the bigfleet
conformance suite against an in-memory backend. Copy it to start a real
provider.

```sh
cp -r providers/_template providers/<name>
```

## What's here

- **`backend.go`** — `templateBackend`, a [`providerkit.Backend`](../../providerkit/backend.go)
  with `TODO(provider-author)` stubs. This is the only file you rewrite: replace
  each stub with calls to your substrate's API. It also implements the optional
  `providerkit.Deleter` (cloud-style teardown); delete that method for a
  bare-metal free pool and the kit answers `Delete` with `codes.Unimplemented`.
- **`main.go`** — wires `Backend → providerkit.Server → grpc.Server`. You should
  not need to touch it. Flags:

  | flag | default | meaning |
  |---|---|---|
  | `--addr` | `:9000` | gRPC listen address |
  | `--provider` | `example` | provider/region name stamped on `HostRef`s |
  | `--state` | _(empty)_ | durable state file; empty = in-memory only |
  | `--seed-count` | `32` | Speculative slots seeded on first boot |
  | `--tls-cert` / `--tls-key` | — | enable TLS |
  | `--tls-ca` | — | client CA → mTLS (production) |

- **`backend_test.go`** — validates the seed shape and walks a machine through
  the full lifecycle via the kit.

Everything cross-cutting — fencing, idempotency, async dispatch, transition
timeouts, the `shard_metadata` lifecycle, field-shape — is handled by
`providerkit`. **Do not re-implement any of it here.**

## Run it

```sh
make run-_template PORT=9000          # boot locally, seeded with 32 slots
make conformance-_template            # boot + run the bigfleet conformance suite
make test-_template                   # unit tests
```

## Make it real

See the step-by-step recipe in [CONTRIBUTING.md](../../CONTRIBUTING.md). In
short: implement the `Describe` + actuator methods, declare `capacity_type` /
`price_per_hour` / `interruption_probability` honestly (a real probability for
SPOT), pass `--state` so restarts are lossless, and get
`make conformance-<name>` green.
