# cutoffarr

Unmonitors the movies and seasons your Radarr/Sonarr have finished upgrading — and nothing else.

[![tests](https://github.com/Foley-ops/cutoffarr/actions/workflows/test.yml/badge.svg)](https://github.com/Foley-ops/cutoffarr/actions/workflows/test.yml)

> Built with Claude Code. The spec, the safety constraints (never delete,
> three write sites, dry-run by default), and the acceptance decisions are
> mine; the agent wrote to them under review.

## What it does

Radarr and Sonarr never stop looking for upgrades. Even when a file has hit your
quality cutoff *and* your "Upgrade Until Custom Format Score", the item stays
monitored and your indexers keep getting asked about it, forever.

cutoffarr runs alongside your *arrs, asks them which items have fully met their
own quality profile requirements, and sets those items to **unmonitored** so the
searching stops. It changes exactly one field (`monitored`) on movies, seasons,
and episodes. It never deletes, moves, renames, or imports anything on its own,
and it never touches profiles, tags, or files.

It also watches the other direction: a reverse scan flags items that are
*unmonitored but shouldn't be* (the accidental unmonitor you did six months
ago), and an optional file report finds video files on disk that your *arrs
aren't tracking — duplicates, orphans, and folder names that differ only by
letter case.

## Why I built it

My Unraid box runs the usual stack — Radarr, Sonarr, Prowlarr, a pile of
profiles managed by Profilarr. One day I looked at how often my indexers were
being queried for upgrades that could not exist: over a thousand finished items,
each still being hunted on every RSS cycle because nothing ever tells the *arrs
"this one is done." Your quality profile already defines *done* — the cutoff
plus the upgrade-until score. cutoffarr just makes that finish line real.

## Quick start

1. Create a config folder and drop in a `config.yml`
   (full annotated version: [config.example.yml](config.example.yml)):

   ```yaml
   instances:
     - name: radarr-main
       type: radarr
       url: http://radarr:7878
       api_key: ${RADARR_API}
     - name: sonarr-main
       type: sonarr
       url: http://sonarr:8989
       api_key: ${SONARR_API}
   ```

2. Add the service to your compose file
   (full version with media mounts and Unraid labels:
   [docker-compose.example.yml](docker-compose.example.yml)):

   ```yaml
   cutoffarr:
     image: ghcr.io/foley-ops/cutoffarr:latest
     volumes:
       - /path/to/appdata/cutoffarr:/config
     environment:
       - RADARR_API=${RADARR_API}
       - SONARR_API=${SONARR_API}
     ports:
       - "9898:9898"
     networks:
       - your-arr-network
     stop_grace_period: 90s
     restart: unless-stopped
   ```

3. `docker compose up -d cutoffarr`
4. Open `http://your-server:9898/` — the dashboard shows what it *would* do.
   **Dry-run is on by default**; nothing is written until you decide.
5. Read the report. When it matches your expectations, set `dry_run: false`.
6. Optional: point a webhook at it from each *arr (Settings → Connect →
   Webhook → `http://cutoffarr:9898/webhook/<instance-name>`, On Import +
   On Upgrade) so imports get evaluated as they land instead of waiting for
   the daily sweep.

## The safety model

This tool writes to the software that manages my media library, so it is built
paranoid. The guarantees are structural — enforced by tests that fail if the
code ever drifts:

- **Three write call sites, one field.** Every HTTP write in the codebase sets
  `monitored` and nothing else. A test counts the call sites and fails on a
  fourth: [`TestTree_HasExactlyThreeWriteVerbCallSites`](writer_test.go).
- **Dry-run by default, provably inert.** A full end-to-end cycle with
  decisions firing is asserted to make zero write requests of any kind:
  [`TestRun_DryRun_MakesZeroWriteRequestsAcrossTheEntireRun`](writer_test.go).
- **Deletion cannot be expressed.** `os.Remove` and friends are banned from
  the entire tree by a structural audit; the only filesystem mutations allowed
  anywhere are human-clicked moves into a `.cutoffarr-trash/` folder:
  [`TestTree_BansFilesystemMutationAPIsEverywhere`](filereport_test.go).
- **An airing season can never be unmonitored.** The single most important
  Sonarr rule, pinned end-to-end:
  [`TestRun_Sonarr_AiringSeason_NeverUnmonitored`](sonarr_test.go).

Beyond those: every decision is logged with its reason; anything the API
returns that looks partial, malformed, or ambiguous makes cutoffarr skip that
item or instance with a warning rather than guess; and a mount that looks
wrong aborts the file report instead of reporting your library as orphans.

## The dashboard

A single embedded page (no framework, no external assets) showing each
instance's progress toward "everything at rest", plus the reverse-scan and
file-clutter findings. Findings carry action buttons — re-monitor, move to
trash, merge case-twins — that are **off by default** (`gui_actions: false`)
and rehearse instead of acting while dry-run is on. Every button states the
exact operation it will perform, re-verifies against live data when clicked,
and refuses if reality changed since the sweep. There is no authentication:
treat it as a LAN tool.

**It comes up warm, and it shows you the scan.** A restart no longer blanks
the page: cutoffarr saves the dashboard's numbers to `state-cache.json` beside
your config at the end of every full sweep, and restores them at startup behind
an amber "showing last sweep from … — refreshing now" banner until the running
scan replaces each shelf with fresh numbers. That file is display only — never
an input to any decision, write, or action, all of which re-read live data
every time — and it is safe to delete whenever you like; you lose one warm
start. While any scan is running, a progress strip above the shelves shows what
each instance is doing (connectivity, library, evaluating, cross-check,
reverse-scan, file-walk…) and the page polls every 2s instead of 30s.

## FAQ

**Does it delete anything?** No. No delete call exists in the codebase — a
test enforces that. File actions (which you click, per item, with the switches
on) move files into `.cutoffarr-trash/`, which is never auto-pruned.

**Is this unmonitorr / unmonitarr?** No — unmonitorr unmonitors after Plex
playback, and unmonitarr unmonitors based on release-group filtering. cutoffarr
unmonitors when your quality profiles' own criteria say an item is finished.

**Why does it say nothing is done?** Your profile's "Upgrade Until Custom
Format Score" is the finish line. If it's set to an unreachable number
(a common default is 10000), then by your own profile's definition nothing is
ever finished. Set it to the score you actually consider done.

## Everything else

Config reference, decision rules, the reverse scan, the file report, action
semantics, trash restore, and operational details:
**[docs/REFERENCE.md](docs/REFERENCE.md)**.

## License

[MIT](LICENSE)
