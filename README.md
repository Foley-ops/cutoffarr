# cutoffarr

A small, headless Go daemon that unmonitors Radarr movies and Sonarr seasons
once they have genuinely finished upgrading — quality cutoff reached **and**
"upgrade until custom format score" reached — so your indexers stop being
asked to search for something better that no release will ever provide.

**It never deletes, moves, or renames a file, and it never touches anything
but the `monitored` flag.** It does not modify quality profiles, tags,
indexers, download clients, or any other *arr setting. The only thing
cutoffarr ever writes is `monitored: false` on an item that has, by the
profile's own rules, nothing left to download — see
[Safety and dry-run](#safety-and-dry-run) for exactly how that claim is
enforced and checked.

## Contents

- [Quick start](#quick-start)
- [Configuration reference](#configuration-reference)
- [Safety and dry-run](#safety-and-dry-run)
- [Known limitations](#known-limitations)
- [Webhook setup](#webhook-setup)
- [FAQ](#faq)
- [License](#license)

## Quick start

The supported deployment is Docker Compose, alongside your existing Sonarr
and Radarr containers. Two files in this repo are the source of truth for
that setup — copy them, don't retype them:

- [`docker-compose.example.yml`](docker-compose.example.yml)
- [`config.example.yml`](config.example.yml)

1. On the host that runs your `*arr` stack, copy both example files and drop
   the `.example` from their names:

   ```sh
   cp docker-compose.example.yml docker-compose.yml
   cp config.example.yml config.yml
   ```

2. Edit `config.yml`: list each Radarr/Sonarr instance under `instances:`
   with its real `url`, and reference its API key as `${SOME_ENV_VAR}` —
   never write a real key into this file. See
   [Configuration reference](#configuration-reference) for every key.
   `dry_run` defaults to `true`, so a first run is always a rehearsal: it
   logs exactly what it would unmonitor and writes nothing.

3. Create a `.env` file beside `docker-compose.yml` holding the API keys
   `config.yml` references, e.g.:

   ```sh
   RADARR_MAIN_API_KEY=your-real-radarr-api-key
   SONARR_MAIN_API_KEY=your-real-sonarr-api-key
   ```

4. Edit `docker-compose.yml`: point `networks.media-net` at the Docker
   network your `*arr` containers are actually on (`docker network ls` — a
   network Compose created for another stack is named
   `<project>_default`, not the bare name from that stack's file).

5. Confirm the network exists, then bring cutoffarr up:

   ```sh
   docker compose up -d
   docker logs -f cutoffarr
   ```

   The first line printed is the loaded configuration (API keys redacted),
   regardless of `log_level`. With `dry_run: true` (the default), every
   subsequent cycle only logs `would-unmonitor` / `skip` decisions — nothing
   is written to either `*arr` yet.

6. Point each `*arr`'s webhook at cutoffarr so it evaluates items right after
   they import or upgrade instead of waiting for the next full sweep — see
   [Webhook setup](#webhook-setup).

7. Once you've read a few cycles of `would-unmonitor` decisions and agree
   with them, set `dry_run: false` in `config.yml` and restart the
   container. Nothing else about the setup changes.

Prefer a single one-off pass over a long-running daemon (e.g. to sanity-check
a config before committing to it)? The binary also supports `--once`, which
runs one full scan and exits — this is how every phase of this project was
tested. `docker run <image> --once` replaces the image's default `CMD`
entirely, so it runs `/cutoffarr --once` against the same config path the
image always defaults to.

## Configuration reference

Single YAML file, `--config` flag (default `/config/config.yml`, which is
also the path the image expects it mounted at). `${VAR}` references inside
any value are expanded from the environment at load time; a reference to an
unset variable is a fatal startup error naming the variable, not a silent
blank.

| Key | Default | Meaning |
| --- | --- | --- |
| `dry_run` | **`true`** | **Read this one first.** When true, cutoffarr performs zero write requests — not one — and only logs what it would do. Every write code path checks this flag immediately before its HTTP call, not once at startup, so nothing short of setting it to `false` in the loaded config ever causes a write. `--dry-run` on the command line can force it *on* but can never turn it off. |
| `poll_interval` | `24h` | How often the reconciliation full sweep runs, as a Go duration string. Minimum `1h` if nonzero; `0` disables the sweep entirely (webhooks and the startup scan are then the only triggers). This is the safety net for events missed while the container was down. |
| `webhook_port` | `9898` | The port the webhook HTTP listener binds, inside the container. Must be `1`-`65535`. |
| `webhook_debounce` | `45s` | How long to wait after the *last* event for a given movie/series before evaluating it — so a season-pack import (many episode-import events) becomes one evaluation, not one per episode. `0` evaluates immediately, with no wait. |
| `log_level` | `info` | One of `debug`, `info`, `warn`, `error`. Logging is always to stdout only, via `log/slog`'s text handler — never to a file. |
| `exclusion_tag` | `cutoffarr-exclude` | The tag label that opts an item out of everything cutoffarr does, in every mode, including dry-run reporting. Must not be empty or all-whitespace (omit the key entirely to use the default; an explicit empty string is a fatal config error, not a silent "exclude nothing"). |
| `instances` | *(required, may be empty)* | A list of `*arr` instances to reconcile against. An empty list is valid (cutoffarr just warns and does nothing) but almost certainly not what you want. |
| `instances[].name` | — | A unique, human-readable name used in every log line and as the webhook path segment (`/webhook/{name}`). |
| `instances[].type` | — | `radarr` or `sonarr`. |
| `instances[].url` | — | The instance's base URL, e.g. `http://radarr:7878` — must be an absolute `http://` or `https://` URL. |
| `instances[].api_key` | — | The instance's API key. Always reference this as `${ENV_VAR}`; never write a literal key into a committed or shared config file. |

See [`config.example.yml`](config.example.yml) for a complete, commented
file in this exact shape — it is the canonical example, not a summary of
one; if the two ever disagree, the example file is right and this table
should be fixed to match it.

## Safety and dry-run

cutoffarr holds an API key for every `*arr` it's pointed at and is the one
thing in the stack allowed to write through them, so its write surface is
deliberately small and independently checked, not just documented:

- **Dry-run defaults to true.** A freshly deployed cutoffarr — or one whose
  config you haven't touched — writes nothing until you explicitly set
  `dry_run: false`. See the table above for exactly what the flag guards.
- **The exclusion tag is your opt-out.** Tag any movie (Radarr) or series
  (Sonarr) with the `exclusion_tag` label (default `cutoffarr-exclude`) and
  it is skipped entirely, in every mode — including dry-run's own reporting.
  Use it for anything you want to keep monitored regardless of cutoff state.
- **Exactly three places in the entire codebase can send a write.** This
  isn't a claim in a comment — it's enforced by a test,
  `TestTree_HasExactlyThreeWriteVerbCallSites` in `writer_test.go`, which
  recursively walks every non-test `.go` file in the tree and asserts two
  things: (1) the client method that carries a request body (`DoJSON`) is
  called exactly three times total — once in `writer.go` (the Radarr movie
  PUT) and twice in `sonarr_writer.go` (the Sonarr episode-monitor PUT and
  the season PUT) — and (2) no other non-GET HTTP method is named *anywhere*
  else in the tree, whether as an `http.Method*` constant or a bare quoted
  string literal like `"PUT"`. Add a fourth write site anywhere in the
  project and this test fails until someone comes here and updates it
  deliberately.
- **A write changes exactly one JSON field, on an object fetched fresh
  first.** Every write path does GET → flip `monitored` → PUT the same
  object back, never constructing a partial payload from scratch. It never
  calls anything that deletes, imports, renames, or triggers a search —
  `deleteFiles` and `/api/v3/command` are never touched.
- **A Sonarr season write is atomic with respect to shutdown.** Unmonitoring
  a season is two sequential API calls (the episodes, then the season flag),
  and the state in between — episodes unmonitored, season still monitored —
  is exactly the inconsistency this project goes out of its way never to
  leave behind. That pair is deliberately detached from Go's own
  cancellation (`context.WithoutCancel`) so it always completes or never
  starts, and a dedicated recovery pass on the next cycle finishes any pair
  a hard kill did interrupt.
  <br><br>
  That guarantee only holds if the *process* is given time to finish the
  item it's mid-way through before it's killed, which is why
  [`docker-compose.example.yml`](docker-compose.example.yml) sets
  `stop_grace_period: 90s`. On `SIGTERM`, cutoffarr finishes the item
  currently in flight and exits — it does not accept new work — but Docker's
  *default* stop timeout is 10 seconds, after which it sends `SIGKILL`,
  which bypasses all of the above from the outside. A season write is up to
  five sequential API calls (GET series, GET episodes, PUT episode-monitor, a
  confirming re-GET, PUT series), each bounded by a 15-second per-call
  timeout, plus a 5-second drain of the webhook listener: 80 seconds
  worst-case, 90 for headroom. **Don't lower `stop_grace_period` below that
  without understanding why it's there.**

## Known limitations

Honest gaps found during live testing, carried forward rather than hidden:

- **Multi-episode files skip fail-safe, not silently.** When a season's
  `statistics.episodeFileCount` claims more files than Sonarr's
  `/episodefile` endpoint actually returns for it — which happens when one
  physical file covers multiple episodes — cutoffarr cannot trust which
  files it's missing, so it skips the whole season (`episode file count
  mismatch`) rather than guess. Such a season will never be unmonitored
  until this is refined in a future phase; it is not silently ignored — it
  logs a warning naming the season every cycle it's evaluated.
- **An unreachable `cutoffFormatScore` means "done" never arrives, by
  design.** If a quality profile's custom-format cutoff score is set far
  above anything any real release could score (some profiles use a
  sentinel like `10000` to mean "always keep upgrading"), cutoffarr will
  correctly conclude, forever, that the cutoff hasn't been met — because it
  hasn't, by that profile's own definition. This is not a bug to report; it
  means the profile needs to be tuned to a real, reachable "good enough"
  score before cutoffarr has anything to do for it.
- **macOS + Docker Desktop can block LAN access entirely.** If you develop
  or test on macOS, Docker Desktop's containers can be silently prevented
  from reaching other devices on your LAN (including a `*arr` on another
  machine) by macOS's own **Local Network** privacy setting, which is scoped
  to the Docker Desktop *application*, not to individual containers. If
  cutoffarr's connectivity check fails against an instance you can otherwise
  reach fine from a browser on the same Mac, check System Settings → Privacy
  & Security → Local Network → Docker, or test running the binary natively
  instead of inside Docker Desktop.

## Webhook setup

This section is for you, the human operator — cutoffarr does not, and
cannot, configure your `*arr`s for you. Once cutoffarr is running (see
[Quick start](#quick-start)), do the following **in each Radarr/Sonarr
instance's own web UI**:

1. Go to **Settings → Connect**, and add a new connection of type
   **Webhook**.
2. Set the **URL** to `http://cutoffarr:9898/webhook/{instance-name}`,
   where `{instance-name}` is that exact instance's `name:` from your
   `config.yml` (e.g. `http://cutoffarr:9898/webhook/radarr-main`). If both
   containers share the `media-net` Docker network from the compose example,
   this hostname resolves without needing the published `9898:9898` port at
   all — the published port exists only for testing the hook from outside
   Docker.
3. Under **Notification Triggers**, tick **On Import** and **On Upgrade**,
   and leave the rest unticked. Those are the only two events cutoffarr acts
   on; anything else it receives is logged and ignored.
4. Save, then use the instance's own "Test" button to confirm cutoffarr
   receives it — check `docker logs cutoffarr` for the corresponding line.

Repeat once per configured instance — each one has its own name and its own
URL. A webhook fires an evaluation of only the movie or season the event
named, debounced by `webhook_debounce`; everything else eligible still gets
picked up by the next full reconciliation sweep (`poll_interval`, default
`24h`).

## FAQ

**Does it delete anything?**
No. cutoffarr never sends a delete of any kind, to either `*arr`. The only
field it ever writes is `monitored`, and only to `false`. This isn't just a
design intent — see
[Safety and dry-run](#safety-and-dry-run) for the test that makes it
checkable: no delete verb (or any write verb outside the two designated
write-path files) exists anywhere in the tree.

**Is this the same as unmonitorr or unmonitarr?**
No — both are real, unrelated projects that happen to have very similar
names, and neither triggers on the same thing cutoffarr does:

- [**unmonitorr**](https://github.com/Shraymonks/unmonitorr) unmonitors
  media in response to **Plex/Jellyfin playback webhooks** — it acts when
  *you watch something*.
- [**unmonitarr**](https://github.com/unmonitarr/unmonitarr) unmonitors
  media based on **release-group / fake-release filtering** — it acts on
  *what was grabbed*.
- **cutoffarr** (this project) unmonitors based on **quality-profile
  state** — it acts when an item's quality cutoff *and* custom-format
  cutoff score have both genuinely been reached, independent of anything
  ever being played or which release group provided the file.

**Can it re-monitor something it already unmonitored?**
No, not in this version. cutoffarr is one-directional: it only ever moves an
item from monitored to unmonitored. A reverse scan is a possible future
addition, not something v1 does.

**Does it touch quality profiles, tags, indexers, or anything else?**
No. `monitored` is the only field cutoffarr ever writes, on the one object
(a Radarr movie, or a Sonarr season/its episodes) each write targets. It
reads tags (to check the exclusion tag) and quality profiles (to know the
cutoff), but never writes either.

**What if I only want this applied to some of my library?**
Tag anything you want left alone with the `exclusion_tag` label (default
`cutoffarr-exclude`) — see [Safety and dry-run](#safety-and-dry-run).

**Where does it log to?**
Stdout only, structured text via Go's `log/slog`, at the level set by
`log_level`. Nothing is ever written to a file or a database — `docker logs
cutoffarr` is the entire operational surface.

## License

[MIT](LICENSE).
