# Historical UbuntuRojo localhost M3 prerequisite provisioner

> This document preserves the completed manual provisioning evidence. It is
> not the current operator ceremony. Released operators install packaged
> Shipmates-owned runtime assets with `sudo shipmates install`; the profile,
> credentials, host qualification, and Captain approval remain separate.

The managed-safe M3 implementation remains GO, while production M3 remains
NO-GO until the separate unrestricted qualification is run by the Captain.
This provisioner only creates later prerequisites; it never runs the
qualifier, invokes `sudo`, contacts Fleet, or starts systemd.

Historical build/provisioning details follow for audit and migration context:

```text
make shipmates-m3-provision
```

An administrator must install the verified provisioner and fixed runner as:

```text
/usr/libexec/shipmates/shipmates-m3-provision
/usr/libexec/shipmates/shipmates-m3-qualifier-run
```

The only accepted invocation is:

```text
/usr/libexec/shipmates/shipmates-m3-provision --profile ubuntu-rojo-localhost
```

It requires root, rejects extra arguments and secret-bearing environment
names, refuses existing targets, and emits only redacted IDs, hashes, paths,
and the later Captain command. It writes through a staging directory and one
final rename. Existing profiles and service units are never replaced.

The profile creates the administrator-owned authority store, one enrolled
ship, one active one-ship `fleet.commander.delegate.v1` credential, and a
separate disposable Commander credential. The disposable credential is
rotated, its old generation is checked inactive, the replacement is checked
active, and it is revoked and cleared before provisioning completes. No
disposable secret is persisted.

Protected output is rooted at `/etc/shipmates/m3-qualifier/` and includes the
read-only public `fleet.json` and trust plus root-only
`credentials/ship.json` and `credentials/commander.json`, and the ship-proof credential,
localhost CA/leaf/key files, trust pin, helper manifest, authority state, and
ship identity state. Secret and state files are create-new and mode `0600`;
public config/trust files are non-writable (`0644`); protected directories are
`0700`. The exact verified helper bytes are copied under `helper/` alongside
their pinned manifest; no PATH or override is installed. The leaf certificate
is valid for `localhost` only, and `fleet.json`
binds the exact WSS tunnel path, service, Fleet/ship IDs, CA digest, and SPKI
pin. The installed cgroup launcher is checked as a protected ELF and pinned
by SHA-256; it is not installed or replaced here.

The `shipmates-m3-qualifier.service` unit is written but not enabled or started. It runs as the
pre-existing `shipmates` account with `Delegate=yes`, fixed read-only and
read-write paths, bounded memory/tasks/timeouts, and no environment-based
secrets. It has exactly two `LoadCredential` mappings from the root-only
credential directory; the runner reads only `CREDENTIALS_DIRECTORY` and fixed
basenames while retaining descriptor identity. A staged delegated-probe plan records the required disposable
`cgroup.procs`, `cgroup.kill`, `cgroup.events`, pidfd, cleanup, and leaf-removal
checks; it performs no fork, cgroup mutation, network access, or qualification
during provisioning. Later, after the administrator has separately validated
the delegated cgroup root and helper, the Captain may run exactly the one
emitted command:

```text
systemctl start shipmates-m3-qualifier.service
```

That later command is outside this bounded job and remains subject to the
authoritative production NO-GO gate.
