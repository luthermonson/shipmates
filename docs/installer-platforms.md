# Shipmates installer and runtime packaging

The distributable entry point for Shipmates-owned runtime assets is:

```text
sudo shipmates install
```

The command is offline and manifest-verified. It installs only the fixed
release assets embedded in the Linux binary; it does not download packages,
contact Fleet, read credentials, invoke an inner `sudo`, enable or start a
service, or run M3 qualification.

## Public command contract

`shipmates install` accepts no positional arguments and no destination, source,
service-manager, credential, endpoint, or arbitrary-command flags.

- `--dry-run` validates the embedded manifest, detects the platform, selects a
  composition, and reports without changing files.
- `--json` emits one bounded `shipmates.install.report.v1` report containing
  action, release, changed state, composition, and retained-state references;
  it never contains secrets or external Fleet values.
- `--profile ubuntu-rojo-localhost` selects the fixed optional profile plan
  only when hardened capability is available. It does not create credentials,
  authority, a Fleet connection, or a qualification.
- `--uninstall` first requires a proven inactive service state; unknown or
  active state refuses without stopping anything. It removes only matching
  Shipmates-owned release/current assets and retains the install and uninstall
  journals, state, credentials, authority, and operator data needed for
  recovery and audit. Drifted objects are reported as incomplete.
  `--dry-run --uninstall` reports the retained state without removing anything.

The installer requires effective UID 0. It refuses active-unit drift, symlinked
or unsafe parents, modified managed assets, unknown manifest entries, and
partial activation. Repeating an install against the verified same release is
idempotent; a changed fixed asset or destination is a hard error. Installation
stages a release, fsyncs files and directories, commits atomically, then
updates fixed destinations only after digest and identity checks. A failed
commit removes only its incomplete staging and newly created assets.

The immutable layout is rooted at `/usr/libexec/shipmates`, with releases under
`releases/<release>` and a non-symlink `current` release marker. The retained
install journal is `/var/lib/shipmates/install.json` and the uninstall recovery
journal is `/var/lib/shipmates/uninstall.json`; uninstall deliberately does not
delete either journal or `/var/lib/shipmates` credentials/state. The shipped
manifest is `shipmates.install.manifest.v1`, and each regular asset is pinned
by source, fixed destination, byte size, SHA-256, mode, and `root:root` owner.

## Platform composition

Capability detection is fail-closed for hardened composition but conservative
for ordinary operation:

| Environment | Hardened result | Fallback |
| --- | --- | --- |
| Bare Linux | systemd layout only with systemd, cgroup v2 delegation, pidfd, trusted launcher, no user namespace, and writable root | ordinary Shipmates operation remains available |
| Capable WSL | same protected layout when all capabilities are present | limited WSL guidance; no Windows mutation |
| Limited WSL | no hardened service composition | use ordinary Shipmates operation or administrator-provisioned Linux assets |
| systemd container | protected layout only when delegation and all containment capabilities are visible inside the container | ordinary operation if capability is unavailable |
| Non-systemd container | no init/service-manager assumption | fixed container runtime layout and ordinary operation |
| Read-only root or user namespace | no hardened install claim | ordinary operation; do not weaken the manifest or filesystem boundary |

The installer never assumes that a container can control the host cgroup tree.
The optional profile is refused unless hardened composition is selected. Missing
capability does not disable ordinary project/persona workflows.

## M3 assets and security boundary

The embedded release contains the fixed runner, the pinned cgroup launcher, and
the disabled systemd unit. The installer does not create Fleet credentials or
the protected profile. When an administrator separately provisions the
profile, systemd delivers exactly the fixed `ship.json` and `commander.json`
records through `LoadCredential=`; an all-encrypted
`LoadCredentialEncrypted=` variant is mutually exclusive and cannot silently
downgrade. The runner accepts only the manager-created credential directory
and fixed basenames.

The service remains `User=shipmates`, `Delegate=yes`, `NoNewPrivileges=yes`,
and bounded by the existing filesystem, resource, and address-family policy.
The service is disabled/inactive after installation. Production M3 remains
NO-GO until the separately authorized unrestricted qualifier proves real host
provisioning, TLS/M7/M3 binding, delegated containment, lifecycle, and cleanup.
Historical manual provisioner/helper/runner reports remain evidence; they are
not the current operator ceremony. See [M3 provisioning history](m3-ubuntu-rojo-localhost-provisioning.md)
and [qualifier helper history](shipmates-cgroup-launcher-install.md).

## Packaging and reproducibility

The release binary embeds the same assets used to generate the installer
manifest. `go run mage.go installerManifest` emits the closed JSON manifest
for offline review. `go run mage.go release` requires `SOURCE_DATE_EPOCH`,
builds a trimmed Linux binary, emits the manifest alongside the binary and
operator docs, writes a sorted SHA-256 checksum file, and creates a normalized
owner/group/timestamp archive. GoReleaser archives the operator docs and
produces its standard checksums. Packaging checks must verify archive member
allowlists, manifest digests, no credential-shaped data, stable ordering, and
identical bytes for repeated builds with the same source date.

No release or publication is implied by these targets. The future Captain
qualification remains a separate host action and is not part of installation,
packaging, or archive verification.
