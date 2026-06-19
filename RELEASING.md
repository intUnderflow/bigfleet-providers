# Releasing

`bigfleet-providers` is a **multi-module** mono-repo, so each Go module is
versioned and tagged independently using Go's module-path-prefixed tag
convention. A release tags every module at the **same** version.

## Modules and their tags

| Module | Import path | Tag for version `vX.Y.Z` |
|---|---|---|
| providerkit (root) | `github.com/intUnderflow/bigfleet-providers` | `vX.Y.Z` |
| AWS provider | `github.com/intUnderflow/bigfleet-providers/providers/aws` | `providers/aws/vX.Y.Z` |
| Conformance | `github.com/intUnderflow/bigfleet-providers/conformance` | `conformance/vX.Y.Z` |

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

1. Make sure `main` is green (including the `certify-full` job — all 92
   conformance behaviors).
2. Update `CHANGELOG.md` with the new version.
3. Bump `providers/aws/deploy/helm/Chart.yaml` `version` + `appVersion` if the
   chart or image changed.
4. Confirm the bigfleet proto pin is identical across modules:
   `make check-bigfleet-pin`.
5. Tag every module at the same version and push the tags together:

   ```sh
   v=v0.1.0
   git tag "$v" "providers/aws/$v" "conformance/$v"
   git push origin "$v" "providers/aws/$v" "conformance/$v"
   ```

Pushing the **root** tag (`vX.Y.Z`) triggers `.github/workflows/release.yml`,
which:

- runs the **full conformance certification** (`make report-aws` over all
  profiles) and fails the release unless the verdict is `CERTIFIED`;
- builds and pushes the AWS provider image to
  `ghcr.io/intunderflow/bigfleet-aws:vX.Y.Z` (+ `:latest`);
- packages the Helm chart and pushes it to `oci://ghcr.io/intunderflow/charts`;
- creates the GitHub Release, attaching the conformance `report.json`,
  `junit.xml`, and the conformance `badge.json`.

## Versioning policy

Pre-1.0 (`v0.x`), minor versions may contain breaking changes to providerkit's
Go API; patch versions are bug fixes only. The conformance **behavior registry**
is append-only at every version — a behavior id is never reused, only
deprecated — so a provider certified at `vX` stays meaningful at `vX+1`.
