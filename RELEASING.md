# Releasing

`bigfleet-providers` is a **multi-module** mono-repo, so each Go module is
versioned and tagged independently using Go's module-path-prefixed tag
convention. A release tags every module at the **same** version.

## Modules and their tags

| Module | Import path | Tag for version `vX.Y.Z` |
|---|---|---|
| providerkit (root) | `github.com/intUnderflow/bigfleet-providers` | `vX.Y.Z` |
| Each provider | `github.com/intUnderflow/bigfleet-providers/providers/<name>` | `providers/<name>/vX.Y.Z` |
| Conformance | `github.com/intUnderflow/bigfleet-providers/conformance` | `conformance/vX.Y.Z` |

Providers (`<name>`): `aws`, `azure`, `digitalocean`, `gcp`, `hetzner`,
`libvirt`, `oracle-cloud`, `ovhcloud`, `scaleway`.

The root module is **providerkit** — the shared correctness library every
provider builds on. It carries no `replace` directives, so external providers
consume it directly:

```sh
go get github.com/intUnderflow/bigfleet-providers@vX.Y.Z
```

The in-repo provider + conformance modules use a local `replace` to build
against the working tree; that `replace` is for in-repo development only and is
ignored by external consumers.

## Cutting a release

1. Make sure `main` is green (the `ci-ok` gate — every provider's `certify` lane
   passes all 93 conformance behaviors credential-free).
2. Update `CHANGELOG.md` with the new version (its top `## vX.Y.Z` section
   becomes the GitHub Release notes).
3. Confirm the bigfleet proto pin is identical across modules:
   `make check-bigfleet-pin`.
4. Tag every module at the same version and push the tags together. Charts and
   images are versioned by the tag (no `Chart.yaml` bump needed):

   ```sh
   v=v0.2.0
   # root (providerkit) + conformance + every provider module
   tags=("$v" "conformance/$v")
   for d in providers/*/; do
     n=$(basename "$d"); [ "$n" = "_template" ] && continue
     tags+=("providers/$n/$v")
   done
   git tag "${tags[@]}"
   git push origin "${tags[@]}"
   ```

Pushing the **root** tag (`vX.Y.Z`) triggers `.github/workflows/release.yml`,
which builds and publishes **every** provider (auto-discovered) and:

- runs a **full conformance certification** of each provider over all profiles
  and fails the release unless every verdict is `CERTIFIED`;
- builds and pushes each provider image to
  `ghcr.io/intunderflow/bigfleet-<name>:vX.Y.Z` (+ `:latest`);
- packages each Helm chart at the tag version and pushes it to
  `oci://ghcr.io/intunderflow/charts` (e.g. `bigfleet-aws`);
- creates the GitHub Release, attaching every provider's packaged chart and
  using the latest `CHANGELOG.md` section as the notes.

> Note: every green check certifies against the **credential-free** conformance
> suite (the fake backend); a provider's real-cloud path is operator-verified,
> not exercised in CI. Release notes should say so.

## Versioning policy

Pre-1.0 (`v0.x`), minor versions may contain breaking changes to providerkit's
Go API; patch versions are bug fixes only. The conformance **behavior registry**
is append-only at every version — a behavior id is never reused, only
deprecated — so a provider certified at `vX` stays meaningful at `vX+1`.
