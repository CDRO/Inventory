# Vendored icon data

`noto.json` is the `@iconify-json/noto` npm package's `icons.json`, in
[IconifyJSON format](https://iconify.design/docs/types/iconify-json.html):
Google's **Noto Emoji** set, converted to a single machine-readable file by
the Iconify project.

- **Source:** `https://registry.npmjs.org/@iconify-json/noto` — the
  `icons.json` file inside the package tarball, version `1.2.9`, unpacked at
  `package/icons.json`.
- **License:** Apache License 2.0
  (`https://github.com/googlefonts/noto-emoji/blob/main/svg/LICENSE`),
  Google Inc. The license applies to the whole file; see
  [`../../../docs/specs/42-local-icon-library.md`](../../../docs/specs/42-local-icon-library.md)
  for why it is recorded once here rather than per row.
- **Never fetched at build or run time.** This file is read from disk (via
  `//go:embed`) by `internal/iconlib` and by the `inventory icons import`
  subcommand (`docs/specs/01-architecture-and-deployment.md`). Downloading it
  is a one-time, by-hand maintainer action — not a step any Docker build or
  `docker compose` invocation performs.

## Refreshing

To pick up a newer Noto release:

```console
$ curl -s https://registry.npmjs.org/@iconify-json/noto/latest | grep -o '"tarball":"[^"]*"'
$ curl -sL <tarball URL> -o noto.tgz
$ tar -xzf noto.tgz package/icons.json
$ cp package/icons.json internal/iconlib/data/noto.json
```

Then re-run `inventory icons import` (or `migrate up`, which runs it) against
a running instance — existing rows are untouched (`ON CONFLICT (name) DO
NOTHING`), and any newly-added icon keys are inserted. Removing an icon key
upstream does not remove its row here; nothing in this system deletes an
`icons` row that a product may already reference by name.

Before vendoring a different or additional collection, confirm its license is
permissive (MIT, Apache-2.0, CC0) — see this spec's "Adding more icons later".
