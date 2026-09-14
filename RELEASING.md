# Preparing and releasing an experimental alpha

The canonical repository is `lightshipHQ/lightship`. This document is a maintainer checklist, not a
claim that an alpha has passed every production-readiness check. The selected license is
Apache-2.0.

## Start the new repository from a reviewed snapshot

A fresh initial commit can omit old commits, deleted files, branches, PR discussions, and other
history. It does not erase third-party rights, fix vulnerabilities, or prove that current files are
safe to publish. Do not use a mirror push, transfer the old repository, or copy its `.git` directory
if the intention is to exclude history.

1. Review and commit the exact files intended for publication. A source archive includes committed
   files only, not uncommitted readiness changes.
2. Export that commit with GitHub's **Download ZIP**, or from the existing repository:

   ```sh
   git archive --format=tar --output=../lightship-source.tar HEAD
   ```

3. Inspect the archive, then extract into a new, empty directory. Do not copy `.env`, `local/`,
   database volumes, private traces, editor state, or build artifacts alongside it. An ignored file
   is not automatically safe to copy. Check tracked files as well as filenames and symlinks.
4. Scan and manually review the **exact extracted directory**. Scan current source with a redacting
   scanner, for example:

   ```sh
   go run github.com/zricethezav/gitleaks/v8@v8.30.1 dir --redact --no-banner .
   ```

   A clean automated scan does not prove the absence of secrets, customer information, private
   documents, or intellectual-property restrictions. Inspect examples, fixtures, screenshots,
   domains, emails, logs, and binary files. Rotate any real credential found; deleting it is not
   revocation. Keep detailed scan reports private.
5. Confirm the canonical repository name. Update the Go module path and all matching internal
   imports if it changes; rerun tests. Workflows use the current GitHub repository rather than a
   hardcoded organisation where possible.
6. Create a new private repository and initial commit from the reviewed snapshot. Run CI there
   before public launch. Do not carry across the old history just to preserve the review PR.

## Ownership and licensing gate

- [ ] The copyright owner and any employer/client permissions to publish are confirmed by the
      people who hold those rights. Moving organisations does not change ownership.
- [ ] All copied or generated contributions are permitted to be distributed under the selected
      license. Preserve attribution and provenance records even when discarding Git history.
- [ ] `LICENSE` contains the unmodified Apache-2.0 terms. Add an accurate project copyright notice
      after confirming the holder; do not substitute the new organisation merely because it hosts
      the repository.
- [ ] Dependencies, embedded UI code/assets, fixtures, and container base images have been reviewed
      for their separate licenses and required notices. Apache-2.0 on LightShip does not relicense
      dependencies. See [THIRD_PARTY.md](THIRD_PARTY.md).
- [ ] The exact source snapshot has been scanned and manually reviewed; any discovered live
      credentials have been rotated.

These are owner/maintainer attestations. An automated license inventory is not legal clearance.

## Repository setup gate

- [ ] Enable GitHub Actions and run all jobs in `ci.yml` on the destination default branch.
- [ ] Enable private vulnerability reporting and test its availability; subscribe maintainers to
      security notifications. Confirm the fallback contacts in `SECURITY.md` and
      `CODE_OF_CONDUCT.md` remain monitored. Do not open publicly without a working private
      reporting route.
- [ ] Configure a default-branch ruleset requiring a reviewed PR and the CI checks. Review available
      GitHub plan controls; YAML files cannot configure repository protection by themselves.
- [ ] Enable dependency/security alerts and secret scanning where available. Review Dependabot PRs.
- [ ] Designate maintainers who can respond to reports and perform releases. Secure their accounts.
- [ ] Review README, security limitations, changelog, issue forms, and contribution instructions
      from an outsider's perspective; confirm no private repository URLs remain unintentionally.

Private vulnerability reporting setup:
[GitHub documentation](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/configure-vulnerability-reporting/configure-for-a-repository).

## Validation gate

- [ ] Formatting, `go vet`, Go race/unit tests, Node UI tests, and workflow validation pass.
- [ ] Real Postgres and ClickHouse integration tests run, rather than skip, using disposable services.
- [ ] `docker build .` succeeds from a clean checkout. Separately smoke-test the Railway demo's
      login, tenant role preview, trace pagination, and documented MCP connection.
- [ ] Secret and Go vulnerability scans pass on a patched toolchain. A scan of reachable Go symbols
      does not cover all deployment, container, or application-logic vulnerabilities.
- [ ] Review native-array and typed-filter smoke results without claiming comprehensive exporter
      compatibility. Known unsupported cases remain documented.
- [ ] Inspect the release archives and test at least one native-platform binary, migrations, startup,
      and the demo flow. Cross-compilation alone is not runtime verification on every platform.
- [ ] Known security/reliability gaps remain explicit in `SECURITY.md` and release notes. This alpha
      must not be marketed as safe for sensitive production data.

CI uses the latest patched Go 1.25 toolchain. Use a patched local toolchain too; `go 1.25.0` in
`go.mod` is the language/toolchain minimum, not a recommendation to run that unpatched release.

## Create a draft alpha release

Only do this in the destination repository after the gates above have been reviewed.

1. Update `CHANGELOG.md` with the intended version, date, changes, and known limitations; merge the
   reviewed changes to the default branch. Start with `v0.1.0-alpha.1`, then increment alpha versions.
2. In **Actions → Draft experimental release → Run workflow**, select the default branch, supply
   `version`, and set `confirm` only after completing this checklist.
3. The workflow validates the selected commit with the same CI, cross-builds Linux/macOS binaries
   for amd64/arm64, and prepares archives and `SHA256SUMS`. Build jobs have read-only permissions;
   only the final draft-release job can write repository contents.
4. The workflow targets that exact validated commit, refuses an existing tag, and creates a **draft
   prerelease**, not a stable or automatically published release. There is no release-on-push trigger.
5. Inspect the draft and every asset. Include release-specific changes and known limitations from
   the changelog, required license notices, the commit SHA, and an explicit experimental warning.
   Checksums detect corruption; they are not a substitute for signed provenance or a security audit.
6. Manually publish only when ready. Verify the tag, download links, checksums, reporting route,
   and installation instructions afterward. Do not overwrite published tags or release assets;
   publish a new alpha version for corrections.

Publishing the release also runs `publish-container.yml` for the matching GHCR image. Signed
provenance and a stable-version compatibility policy are not supplied for this experimental alpha.
