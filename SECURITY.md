# Security

## Reporting a vulnerability

Report it privately through this repository's **Security** tab, under
"Report a vulnerability", rather than as an issue. One person maintains the
plugin with no support commitment, so there is no promised response time;
reports touching token handling, the pick path or the status page's data come
first.

The plugin runs inside [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
and holds no credential of its own: it reads each seat's OAuth token through
the host. A vulnerability in CLIProxyAPI itself belongs with its maintainers.

## Supported versions

Only the latest release receives fixes.

## Verifying a release

Every release carries a `checksums.txt` and a build provenance attestation for
each zip.

`sha256sum --check --ignore-missing checksums.txt` catches a corrupted
download. It cannot show who built the zip, because the same workflow
publishes the checksums beside the zips.

The attestation can:

```sh
gh attestation verify claude-seat-pacer_<version>_<goos>_<goarch>.zip \
  --repo yuya-iwabuchi/cpa-plugin-claude-seat-pacer
```

It passes only for a zip this repository's release workflow built from the
tagged commit.
