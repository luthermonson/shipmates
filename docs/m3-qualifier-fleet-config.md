# Protected M3 qualifier Fleet configuration

The future unrestricted qualifier accepts exactly one administrator-provisioned
public binding at:

```text
/etc/shipmates/m3-qualifier/fleet.json
```

The closed schema is `shipmates.m3.qualifier-fleet-config.v1`:

```json
{
  "schema": "shipmates.m3.qualifier-fleet-config.v1",
  "fleet_id": "flt_example_0123456789",
  "ship_id": "shp_example_0123456789",
  "credential_id": "cred_example_0123456789",
  "fleet_url": "wss://fleet.example.test:8443/api/fleet/v1/tunnel",
  "fleet_dns": "fleet.example.test",
  "tls_server_name": "fleet.example.test",
  "trust_file": "fleet-ca.pem",
  "trust_sha256": "<64 lowercase hex characters>",
  "tls_spki_sha256": "<64 lowercase hex characters>",
  "credential_file": "commander.json",
  "service_identity": "shipmates-fleet-commander.v1",
  "m7_identity": "fleet-observation.v1",
  "m3_identity": "fleet.commander.mailbox.v1"
}
```

`trust_file` is a basename under `/etc/shipmates/m3-qualifier/trust/`.
`credential_file` is a basename under
`/etc/shipmates/m3-qualifier/secrets/`. The secret record is never committed
or printed and has the separate closed schema
`shipmates.m3.qualifier-credential.v1`:

```json
{
  "schema": "shipmates.m3.qualifier-credential.v1",
  "fleet_id": "flt_example_0123456789",
  "ship_id": "shp_example_0123456789",
  "credential_id": "cred_example_0123456789",
  "secret": "<administrator-provisioned credential>"
}
```

The loader rejects duplicate, unknown, trailing, invalid UTF-8, oversized, or
noncanonical JSON; non-WSS endpoints, non-production paths, redirects, and
DNS/SPKI/ID grammar violations; and identity mismatches. The TLS dialer uses
the opened trust bytes, exact DNS/server identity, current certificate
validity, and the configured leaf SPKI hash. The production client then uses
the same opened credential identity for the authenticated M7 handshake and
requires the negotiated `fleet.commander.mailbox.v1` capability before any
M3 lifecycle gate. Every parent and child is opened descriptor-relatively
with no-follow semantics. Production directories and files must be regular,
single-link where applicable, protected from group/other writes, and
root-owned. The executing-account exception exists only in the internal
deterministic `LoadAt` test fixture and is unreachable from the qualifier.
Trust bytes must match the configured SHA-256.

Provisioning is administrator-owned. Shipmates does not create these roots,
install credentials, invoke sudo, contact Fleet, or place secret bytes in
argv, environment, URLs, logs, evidence, or reports. The only intended secret
handoff is the internal ship-proof callback after all public bindings have
validated. Missing or unsafe configuration disables the future qualifier while
leaving M7 healthy; it does not alter M3 authority or the existing NO-GO.
