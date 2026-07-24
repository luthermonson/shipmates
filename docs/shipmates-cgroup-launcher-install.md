# Historical Shipmates cgroup launcher installation

> The pinned launcher is now an embedded, manifest-verified asset of
> `sudo shipmates install`. This page preserves the prior administrator
> provisioning contract and evidence; it is not a replacement for the public
> installer and does not authorize qualification.

The M3 assessment path is disabled unless an administrator provisions the
exact Linux ELF helper described here. Shipmates never installs, replaces, or
executes `sudo` for this step.

Build the pinned target from the repository root:

```text
go run mage.go cgroupLauncherVerify
```

Before installation, verify the generated file is an ELF executable and that
`sha256sum -c tools/shipmates-cgroup-launcher/manifest.sha256` succeeds. An
administrator must install that exact verified file at:

```text
/usr/libexec/shipmates/shipmates-cgroup-launcher
```

The parent directory and helper must be root-owned, regular, and not writable
by group or other users. The helper must remain a regular file, not a
symlink. Provisioning is external to Shipmates and must be repeated through
the administrator's normal package/configuration-management process when the
pinned version changes.

The helper accepts only inherited descriptors for a delegated cgroup
directory, readiness channel, release gate, fixed launch specification, and
already-open target ELF. It joins and verifies the cgroup, emits the fixed
`SHIPMATES_CGROUP_READY_V1 <device> <inode>` message, waits for the release
gate, and executes the already-open target identity. It accepts no generic
command-line target or shell command.

Failure to provision the exact helper, delegated subtree, or required kernel
controls disables M3 assessment only; observation/M7 remains available. This
document does not authorize or perform the unrestricted M3 qualifier.

The protected Fleet binding schema and file-loader contract are documented in
[`m3-qualifier-fleet-config.md`](m3-qualifier-fleet-config.md).

The Captain qualifier accepts these non-secret environment inputs: an
explicit `SHIPMATES_CGROUP_ROOT`, a protected `SHIPMATES_M3_FLEET_CONFIG`, and
an optional `SHIPMATES_M3_EVIDENCE_DIR`. It discovers `/proc` mount and
hierarchy state, validates the root beneath both, probes one disposable leaf,
and stops on any fixed prerequisite diagnostic before invoking production
tests. Credentials are read only from the protected Fleet configuration and
are never command-line arguments.
