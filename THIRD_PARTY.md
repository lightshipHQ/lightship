# Third-party software and attribution

LightShip's own source is distributed under [Apache-2.0](LICENSE). Its dependencies retain their
own licenses. The Go dependency graph is recorded in `go.mod` and `go.sum`; the application also
embeds the UI files under `internal/httpapi/ui`.

Before distributing a release, inventory the dependencies actually compiled into the executable
and preserve their license/copyright notices. From the checkout root with a patched Go toolchain:

```sh
go run github.com/google/go-licenses/v2@v2.0.1 report ./cmd/lightship
bash scripts/third-party-licenses.sh dist/third-party-licenses
```

Review errors and unknown licenses rather than suppressing them. Re-run the inventory after
dependency changes. License discovery can miss manually copied code, JavaScript assets, templates,
or data; inspect those separately. Keep any existing third-party `NOTICE` text as well as licenses.
The release workflow includes dependency notices in the binary archives.

The pinned scanner classifies `github.com/segmentio/asm` v1.2.1 as unknown because it does not
recognize MIT-0. The helper checks that exact dependency version and license-file checksum, then
copies its unmodified license explicitly alongside the automatically collected notices and Go's
runtime license. This is a narrow scanner workaround, not permission to ignore unknown licenses.
A dependency update that changes the version or license text requires reviewing and updating the
helper before release.

The embedded web UI vendors `@primer/css` 22.3.0 and `@primer/primitives` 11.10.0. Their compiled
stylesheets retain their upstream notices, and their MIT license text is embedded alongside them in
`internal/httpapi/ui/primer-LICENSES.txt`.

The embedded web UI also vendors Latin WOFF2 subsets of Instrument Sans and Geist Mono. Both fonts
are licensed under the SIL Open Font License 1.1; their full license texts and copyright
notices are kept in `internal/httpapi/ui/instrument-sans-OFL.txt` and
`internal/httpapi/ui/geist-mono-OFL.txt`.

Container images have additional dependencies and notices beyond the Go binary. Review the exact
base images and their distribution obligations separately before publishing images.

The retail-demo scenarios are a clean-room adaptation of retail support concepts reviewed in
Sierra Research's MIT-licensed τ²-bench. No upstream code, data records, or policy prose are copied.
The exact revision, license link, reviewed subset, and modifications are recorded in
`examples/retail-demo/SOURCE_MANIFEST.md`. The Langfuse agent workshop named there has no repository
license and is reference-only; no part of it is redistributed.

This inventory process is not an ownership attestation or legal opinion. Publication approval and
the source-snapshot review remain the maintainer's responsibility in [RELEASING.md](RELEASING.md).
