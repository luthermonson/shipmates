# Known GitHub repos

Visibility cache. Populated as the persona learns the project's GitHub neighborhood.
Read this file before any `gh` call to avoid burning the team `GITHUB_TOKEN` bucket on
visibility checks. For repos marked `public`, prefer `GH_TOKEN= gh ...` for reads.

| Repo | Visibility | First seen | Notes |
|---|---|---|---|

<!--
Append rows as you learn:

| anthropics/claude-code | public | 2026-06-18 | docs/CHANGELOG only; no CLI source |
| our-team/private-monorepo | private | 2026-06-20 | always use auth |

If a repo's visibility changes (rare but possible — open-sourcing, going private),
update the row and add a note. Don't delete the old finding silently.
-->
