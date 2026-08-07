# Architecture diagrams

The Mermaid sources in [`docs/diagrams/`](diagrams/) are canonical. Keeping a
second copy of each diagram in this file caused the rendered architecture and
route names to drift.

- [`topology.mmd`](diagrams/topology.mmd) — local captain, crew, server, and memory.
- [`lifecycle.mmd`](diagrams/lifecycle.mmd) — one delegation and its hook flow.
- [`fleet-topology.mmd`](diagrams/fleet-topology.mmd) — outbound ship tunnels into Fleet Command.
- [`install.mmd`](diagrams/install.mmd) — embedded catalog installation.
- [`update.mmd`](diagrams/update.mmd) — manifest-aware updates.

Render a source with Mermaid CLI, for example:

```sh
mmdc -i docs/diagrams/topology.mmd -o topology.svg
```
