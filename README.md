# cutoffarr

A small, headless Go daemon that unmonitors Radarr movies and Sonarr seasons
once they have genuinely finished upgrading — quality cutoff reached **and**
"upgrade until custom format score" reached — so your indexers stop being
asked to search for something better that no release will ever provide.

**It never deletes, moves, or renames a file, and it never touches anything
but the `monitored` flag.** It does not modify quality profiles, tags,
indexers, download clients, or any other *arr setting. `monitored` is the only
field it ever writes: `false` on an item that has, by the profile's own rules,
nothing left to download — and `true` only when you switch on
`reverse_scan_remonitor`, which is off by default and is the sole way anything
here is ever re-monitored (see [The reverse scan](#the-reverse-scan)). See
[Safety and dry-run](#safety-and-dry-run) for exactly how that claim is
enforced and checked.

## Contents

- [Quick start](#quick-start)
- [Configuration reference](#configuration-reference)
- [Safety and dry-run](#safety-and-dry-run)
- [The reverse scan](#the-reverse-scan)
- [The file report](#the-file-report)
- [Known limitations](#known-limitations)
- [Webhook setup](#webhook-setup)
- [The web dashboard](#the-web-dashboard)
- [FAQ](#faq)
- [License](#license)

## Quick start

The supported deployment is Docker Compose, alongside your existing Sonarr
and Radarr containers. Two files in this repo are the source of truth for
that setup — copy them, don't retype them:

- [`docker-compose.example.yml`](docker-compose.example.yml)
- [`config.example.yml`](config.example.yml)

1. On the host that runs your `*arr` stack, copy `docker-compose.example.yml`
   to `docker-compose.yml` wherever you keep your compose files, and copy
   `config.example.yml` to `config.yml` **inside the directory
   `docker-compose.example.yml` mounts as `/config`** — by default
   `/mnt/user/appdata/cutoffarr` (Unraid's usual appdata layout):

   ```sh
   cp docker-compose.example.yml docker-compose.yml
   mkdir -p /mnt/user/appdata/cutoffarr
   cp config.example.yml /mnt/user/appdata/cutoffarr/config.yml
   ```

   If you keep configs somewhere else, edit the host side of the `volumes:`
   line in `docker-compose.yml` to point there instead — whatever that path
   is, it's what the `/config` path in the
   [Configuration reference](#configuration-reference) below refers to
   inside the container. The volume is mounted read-only, so an empty or
   missing host directory produces an empty, unwritable `/config` and the
   container exits at startup rather than silently running with no config.

   `docker-compose.example.yml`'s `image:` already names cutoffarr's
   published GHCR image, so no build step is required; uncomment its
   `build: .` line instead if you'd rather build from source.

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
tested. `docker run ghcr.io/foley-ops/cutoffarr:latest --once` replaces the
image's default `CMD` entirely, so it runs `/cutoffarr --once` against the
same config path the image always defaults to (mount the same `/config`
volume `docker-compose.yml` does, or it has nothing to read).

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
| `webhook_port` | `9898` | The port the webhook HTTP listener binds, inside the container. Also serves [the web dashboard](#the-web-dashboard) (`GET /`, `GET /api/stats`, `POST /api/scan`) on the same port. Must be `1`-`65535`. |
| `webhook_debounce` | `45s` | How long to wait after the *last* event for a given movie/series before evaluating it — so a season-pack import (many episode-import events) becomes one evaluation, not one per episode. `0` evaluates immediately, with no wait. |
| `log_level` | `info` | One of `debug`, `info`, `warn`, `error`. Logging is always to stdout only, via `log/slog`'s text handler — never to a file. |
| `exclusion_tag` | `cutoffarr-exclude` | The tag label that opts an item out of everything cutoffarr does, in every mode, including dry-run reporting. Must not be empty or all-whitespace (omit the key entirely to use the default; an explicit empty string is a fatal config error, not a silent "exclude nothing"). |
| `reverse_scan_remonitor` | **`false`** | Whether the reverse scan may WRITE. The reverse scan itself always runs on full cycles and reports what it finds; this flag alone decides whether it re-monitors it. With `false` no write of any kind is composed by that pass — not gated, not attempted. With `true`, re-monitoring obeys `dry_run` and the exclusion tag exactly like the forward path, and `--once --only-id N` becomes a scoped both-directions run against that single item ([Trying it on one item first](#trying-it-on-one-item-first)). See [The reverse scan](#the-reverse-scan). |
| `instances` | *(required, may be empty)* | A list of `*arr` instances to reconcile against. An empty list is valid (cutoffarr just warns and does nothing) but almost certainly not what you want. |
| `instances[].name` | — | A unique, human-readable name used in every log line and as the webhook path segment (`/webhook/{instance-name}`). |
| `instances[].type` | — | `radarr` or `sonarr`. |
| `instances[].url` | — | The instance's base URL, e.g. `http://radarr:7878` — must be an absolute `http://` or `https://` URL. |
| `instances[].api_key` | — | The instance's API key. Always reference this as `${ENV_VAR}`; never write a literal key into a committed or shared config file. |
| `instances[].media_root_map` | *(absent = off)* | Opt-in switch for [the file report](#the-file-report): a map of the `*arr`'s root folder path to the same root as cutoffarr's own filesystem sees it, e.g. `/movies: /data/media/Movies`. Absent entirely means the file report never runs for that instance — no disk access of any kind. If present, every key and value must be a non-empty absolute path, or the config is a fatal error at startup; an explicitly empty map (`{}`) is accepted and behaves exactly like absent. |

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
- **Two of the three write sites change exactly one field on an object
  fetched fresh first; the third uses Sonarr's own bulk endpoint, which
  has no "object" to fetch.** The Radarr movie write and the Sonarr season
  write both do GET → flip `monitored` → PUT the same object back, never
  constructing a partial payload from scratch. The Sonarr episode write is
  different by necessity, not by exception: Sonarr has no per-episode PUT,
  only a bulk `PUT /api/v3/episode/monitor` whose body can only ever be a
  list of episode ids plus a single `monitored` bool — there is no fuller
  object to round-trip. cutoffarr still holds that write to the same
  standard by checking its result instead of its input: it reads the
  server's own echoed response to confirm every requested episode id came
  back with the value the write asked for (`false` on the forward path,
  `true` on a reverse re-monitor), and if the response can't settle that, it
  falls back to a read-only re-`GET` of those episodes before trusting the
  write at all — never a second guess in cutoffarr's own favor. Either way,
  no write anywhere calls anything that deletes, imports, renames, or
  triggers a search — `deleteFiles` and `/api/v3/command` are never
  touched.
- **`monitored: true` is written by one pass only, and only if you switch it
  on.** Everything above describes both directions, because both go through
  the same three sites; this is what is additionally true of the reverse one.
  Re-monitoring exists solely behind `reverse_scan_remonitor` (default
  `false`), and with that switch off the reverse pass composes no write at all
  — not a gated one, not a rehearsed one. With it on, three further conditions
  hold, each of them a refusal that is logged and counted rather than a silent
  skip: the cycle's cross-check must have **passed and actually verified
  something**; the decision is **re-run against a fresh fetch** and the write
  refused unless the item still fails its own profile's criteria; and a Sonarr
  season is re-monitored only when its **series is monitored** and **every
  episode under it is unmonitored** — the clean shape an accidental unmonitor
  leaves. A series-level `monitored` flag is never written in either
  direction, ever. See [The reverse scan](#the-reverse-scan) for what it finds
  and [Trying it on one item first](#trying-it-on-one-item-first) for how to
  test it against a single item before trusting it with a library.
- **A Sonarr season write is atomic with respect to shutdown.** Unmonitoring
  a season is two sequential API calls (the episodes, then the season flag),
  and the state in between — episodes unmonitored, season still monitored —
  is exactly the inconsistency this project goes out of its way never to
  leave behind. That pair is deliberately detached from Go's own
  cancellation (`context.WithoutCancel`) so it always completes or never
  starts, and a dedicated recovery pass on the next cycle finishes any pair
  a hard kill did interrupt. Re-monitoring a season is the same two calls and
  the same detachment, but there is no recovery pass in that direction: an
  interrupted re-monitor is *reported* every cycle from then on and never
  finished automatically, because the state it leaves is indistinguishable
  from one you made by hand ([The reverse scan](#the-reverse-scan)).
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
- **The file report never writes, deletes, moves, or renames a file, and it
  never gains an option to.** It is off by default (opt-in per instance via
  `media_root_map`) and, unlike the reverse scan, has no write switch at all
  to turn on: it only reads `/movie`, `/series`, and `/episodefile`, and
  walks the mapped media directories with `fs.WalkDir` and `os.Stat` —
  nothing that creates, removes, or renames anything. This is enforced by a
  test the same way the API write surface is:
  `TestTree_BansFilesystemMutationAPIsEverywhere` in `filereport_test.go`
  greps every non-test file in the tree for `os.Remove`, `os.Rename`,
  `os.Create`, `os.WriteFile`, `os.OpenFile`, `os.Mkdir`/`os.MkdirAll`, and a
  handful of other filesystem-mutation calls, and fails unless the file is on
  an allowlist that is empty by design. See
  [The file report](#the-file-report).

## The reverse scan

cutoffarr's main job is to stop monitoring things that are finished. The
reverse scan asks the opposite question, on every full cycle (the startup
scan, each reconciliation sweep, and `--once`): **what is unmonitored that
should not be?**

It runs the *same* decision function over the unmonitored half of your
library — Radarr movies that have a file, Sonarr seasons that are complete on
disk and fully aired — against Radarr/Sonarr's own
`/wanted/cutoff?monitored=false`. Anything that is unmonitored while still
failing the criteria is reported:

```
level=INFO msg="reverse-scan finding" id=707 title="Some Film" reason="quality cutoff not met" profile=HD-1080p instance=radarr-main
level=INFO msg="radarr decision summary" ... reverseFindings=1 ...
```

In practice a finding is almost always an accidental unmonitor — a stray
click in the UI, an import that flipped something, a list sync — and nothing
else in the stack would ever tell you about it.

Sonarr has one extra finding with no Radarr equivalent, because a season has
episodes underneath its own flag:

```
level=INFO msg="reverse-scan finding" seriesId=42 series="Some Show" season=2 reason="unmonitored season with monitored episodes" ...
```

The season says unmonitored while episodes inside it say monitored. Sonarr
goes on searching and upgrading those episodes, and *nothing else in
cutoffarr can see them* — the forward scan skips the whole season on its
flag. It is the state a season write interrupted halfway leaves behind, and
it is also what you get by monitoring one episode of an unmonitored season by
hand; either way the season is reported until the two agree again — and only
reported, never re-monitored, because nothing in the API says which of the two
you are looking at. See [Letting it fix them](#letting-it-fix-them).

What is **not** a finding, deliberately:

- An unmonitored item that *meets* the criteria. That is cutoffarr's own
  output, and every item it has ever unmonitored looks like that.
- An unmonitored movie with **no file**: leaving a film unmonitored and
  undownloaded is a deliberate choice, not a mistake. (Counted at
  `log_level: debug` so the numbers still add up.)
- Anything carrying the `exclusion_tag`. The tag means *leave this alone*,
  which includes not being told about it.
- Anything cutoffarr could not read with confidence. "We could not check
  this" is never reported as "this is below cutoff".

Findings repeat every cycle, because they stay true until you act on them.
They are printed in full on the startup scan and on `--once`, and demoted to
`debug` on the daemon's repeating sweeps — where the summary's
`reverseFindings=N` is what stays visible.

### Letting it fix them

`reverse_scan_remonitor: true` lets the reverse scan re-monitor what it
finds. It is **off by default**, and off means off: the pass composes no
write at all, rather than composing one and gating it.

With it on, re-monitoring goes through the *same* three write call sites the
rest of the project uses, with every one of the same guards — `dry_run`
checked immediately before each HTTP write, the exclusion tag re-checked
against a fresh fetch, the object's identity confirmed, the server's own echo
required before anything counts as done — plus one more that only this
direction needs: the decision is **re-run against fresh data**, and the write
is refused if the item no longer fails the criteria. (Without that, an item
upgraded since the scan would be re-monitored, unmonitored again by the next
forward pass, and found again by the next reverse one.)

Re-monitoring a Sonarr season is two writes as well (the episodes, then the
flag). If the second one fails, the season is left unmonitored with monitored
episodes inside it. That is never silent — the failure is logged as a warning
naming exactly that state, and every full cycle from then on reports the season
as `unmonitored season with monitored episodes` — but cutoffarr will not finish
the flag for you, because that state is indistinguishable from one you created
yourself (see the third rule below). It is one click in Sonarr, and the finding
keeps pointing at it until you make it.

Three things it will never do:

- Re-monitor anything unless the cycle's cross-check explicitly passed *and*
  actually verified something. If an instance's data disagreed with itself,
  nothing derived from it is written in either direction — and a cycle that
  compared nothing (no monitored item was eligible for the sample) has no
  health signal to offer, so it authorizes nothing either.
- Re-monitor a season whose **series** is unmonitored. Unmonitoring a whole
  series is a human retiring a show; such seasons are reported (with
  `seriesMonitored=false`) and left alone. cutoffarr never writes a
  series-level monitored flag in either direction.
- Re-monitor a season that **already has monitored episodes inside it**.
  Auto-remonitoring is for the clean case: the season flag off and every episode
  under it off too, which is what an accidental unmonitor leaves. A season with
  some episodes already monitored is a mixed state, and cutoffarr cannot tell
  the two things that produce it apart — its own interrupted write, or you
  monitoring an episode by hand — so it does neither thing to it. Writing the
  whole season would drag along the episodes you left alone, and the next
  forward cycle would then unmonitor the lot, including the one you chose;
  writing just the flag would guess that the half-done write was cutoffarr's.
  The season is reported every cycle instead, and the refusal is logged and
  counted as `remonitorsRefused`.

### Trying it on one item first

Switching a write flag on and letting a whole-library pass make the first
re-monitors, unattended, is not a way to try a feature. So the scoped one-shot
run doubles as the instrument for it:

```sh
cutoffarr --once --only-id 707 --instance radarr-main
```

With `reverse_scan_remonitor: true`, that runs **both** directions against
exactly that one id — the ordinary forward evaluation of it, and a reverse pass
that evaluates *only* it. Nothing else in the library is evaluated, reported,
or written, in either direction. If the item turns out not to be a finding on
fresh data (you named something that is unmonitored and finished, say), the
pass reports `reverseFindings=0` and writes nothing: naming an item asks a
question, it does not issue an instruction. Every gate applies unchanged — the
cross-check must have passed *and* verified something, the exclusion tag is
re-checked, the decision is re-run against a fresh fetch, and `dry_run: true`
still withholds the write immediately before the PUT.

`--instance` is required when more than one instance could hold that id, since
each `*arr` numbers its own library from 1.

With the switch **off**, a scoped run stays forward-only, exactly as before:
there is nothing the reverse pass could do about the item, and a second pass
over the unmonitored half of your library is not what `--only-id` means.

The summary line tells you which mode you are in: with the switch off it
carries `reverseFindings=N` and nothing else; with it on, `remonitored`,
`remonitorsRefused` and `reverseWithheld` are always present, including as 0
— and including on a cycle whose reverse pass could not be trusted, which
reads `reverseScan=skipped` in place of the finding count (a number that
cycle is in no position to state) with those three counters still beside it.

## The file report

The *arrs only know about files they imported. A stray extra copy sitting
next to a tracked episode, or a whole file dropped straight into the media
folder from outside the *arr, is invisible to both of them forever — nothing
in Radarr or Sonarr's own UI will ever mention it. The file report finds
these by walking your media directories and comparing what's actually there
against exactly what each `*arr` says it tracks.

**It is read-only, permanently, with no flag to change that.** See the
[Safety and dry-run](#safety-and-dry-run) bullet above for how that is
enforced by a test rather than merely documented. Findings are reported;
nothing is ever deleted, moved, or renamed. Acting on a finding — deleting
the extra file, investigating the orphan — is always a decision you make by
hand, outside cutoffarr, after reading the report.

### Turning it on

It's off by default, per instance. To enable it for an instance, add
`media_root_map` mapping the `*arr`'s own root folder path to the same path
as cutoffarr's own filesystem sees it, and mount that path into the
container **read-only**:

```yaml
instances:
  - name: radarr-main
    type: radarr
    url: http://radarr:7878
    api_key: ${RADARR_MAIN_API_KEY}
    media_root_map:
      /movies: /data/media/Movies
```

```yaml
# docker-compose.yml
volumes:
  - /mnt/user/data/media/Movies:/data/media/Movies:ro
```

If cutoffarr's container mounts media at the *exact same path* the `*arr`
itself uses (the TRaSH-guides convention — every container in the stack
sharing `/data/media/...`), the map is trivial (`/data/media/Movies:
/data/media/Movies`) but still required: an absent `media_root_map` means
off, on purpose, so a config written before this phase existed keeps
touching zero bytes of your disk.

An instance with no `media_root_map` never calls `os.Stat`, never opens a
directory, never reads a single byte from disk — the feature does not exist
for it as far as your filesystem is concerned.

### Method

For every configured root, cutoffarr builds the set of files the `*arr`
actually tracks (Radarr: each movie's own folder plus its `movieFile.path`;
Sonarr: each series' own folder plus every `episodeFile.path`, fetched per
series), maps that set onto disk through `media_root_map`, then walks the
disk root and classifies every file it finds by **location alone** — never
by filename:

- The exact tracked path → not reported (it's supposed to be there).
- Anywhere else inside a tracked movie/series folder → **duplicate**, grouped
  and counted (Sonarr additionally tries to label the group from an
  `SxxEyy`-shaped filename or a `Season NN` folder, for display only — an
  unparseable name groups under its containing folder instead of guessing,
  and never changes whether something is a duplicate in the first place).
- Anywhere else under the mapped root → **orphan**.

Non-video extensions, `-trailer`/`-sample`-suffixed files, and Plex-style
extras subfolders (`Featurettes`, `Behind The Scenes`, `Trailers`, `Extras`,
`Other`, `Specials Extras`, `Deleted Scenes`, `Interviews`, `Scenes`,
`Shorts`, `Featurette`) are excluded from candidacy entirely — never reported
as either kind of finding.

Anything carrying the [`exclusion_tag`](#configuration-reference) is excluded
too, exactly like every other pass: the item's own tracked file is still
protected (it is never reported as an orphan of itself), but any extra file
sitting in its folder is withheld rather than printed as a duplicate finding
naming it.

Symlinks are never resolved — comparison is purely lexical on the mapped disk
paths, matching the rest of cutoffarr's path handling. A symlink pointing at
a tracked file is a *different* path, so it is reported as a duplicate under
its own (unresolved) name; a symlinked subdirectory is never descended into,
because a symlink's own directory entry never reports itself as a directory
to the walk, regardless of what it points to.

```
level=INFO msg="file-report finding" kind=duplicate instance=radarr-main root=/data/media/Movies path="/data/media/Movies/Some Film (2020)/Some Film (2020) (2).mkv" title="Some Film (2020)" groupCount=1
level=INFO msg="file-report finding" kind=orphan instance=radarr-main root=/data/media/Movies path="/data/media/Movies/Stray Folder/something.mkv"
level=INFO msg="file report" instance=radarr-main type=radarr fileReport=ran duplicates=1 orphans=1 fileSkipReasons="none"
```

Exactly like reverse-scan findings, individual `file-report finding` lines
print in full on the startup scan and on `--once`, and demote to `debug` on
the daemon's repeating reconciliation sweeps — where the summary's
`duplicates=N orphans=N` is what stays visible.

`fileReport` on that summary line is always one of three values, and the
three are deliberately never confusable with each other:

- **`off`** — `media_root_map` isn't configured for this instance. Logged
  once per cycle at `debug`, so a user who never opted in sees nothing about
  this feature at the default log level.
- **`skipped`** — it's configured, but at least one mapped root could not be
  trusted this cycle (see below) and was aborted with its own `WARN` naming
  it. `duplicates`/`orphans` still reflect whatever roots *did* complete —
  one root's problem never hides another root's real findings.
- **`ran`** — every configured root completed. `duplicates=0 orphans=0` is a
  real, positive statement that the report ran and found nothing, not
  silence.

### Multi-episode files

A Sonarr season already excluded from the forward scan for an
[episode-file-count mismatch](#known-limitations) has its folder excluded
from duplicate candidacy too, for the same reason: cutoffarr cannot trust
which files in that season are "extra" when it already couldn't trust the
count. Such files are counted under `fileSkipReasons`, never guessed at.

That exclusion is decided by **where a file physically sits** — its
containing `Season NN` folder — never by parsing a season number out of its
filename; a stray file's `SxxEyy`-shaped name is used only to LABEL a
duplicate finding for display, never to decide whether it's a duplicate at
all. Two consequences follow from that, both intentional: a file inside a
distrusted season's folder is withheld even if its filename claims to belong
to a different, trusted season (the folder wins); and if season folders are
disabled for a series entirely (every episode file sits flat in the series
folder) and *any* one of that series' seasons is currently distrusted, every
extra file in that series is withheld rather than guessed at, because there
is no folder boundary left to tell them apart by.

### Case-twin names

A **case-twin** is two entries in the same directory — two movie/series
folders, or two files — whose names differ *only* by letter case: `My Name
Is Earl` next to `My Name is Earl`. This is a real library defect: a `*arr`
tracks at most one of the two spellings, and the other is shadow content
nothing manages or ever will. On a case-*insensitive* view of the same
disk (a Samba/SMB share re-exported to a case-insensitive client, for
example), the two names can also make directory addressing itself
ambiguous — before this check existed, that ambiguity made the whole
root's report abort as "could not be checked" the moment the walk reached
it, rather than telling you what was actually wrong.

Case-twins are caught from the directory **listing** itself — which works
identically on every platform, case-sensitive or not — before anything
else happens with that directory: every level of the walk, both the
subdirectory names and the file names in it, are checked against each
other for a case-only difference *before* any of them is descended into or
classified as tracked/duplicate/orphan. A colliding pair (or larger group)
is reported as its own finding, `kind=case-collision`, naming the
containing directory and every colliding name — and, wherever cutoffarr
can tell, whether each name IS or CONTAINS something the `*arr` actually
tracks, so you know which spelling to keep. The colliding entries
themselves are excluded from that cycle's duplicate/orphan accounting (and,
for a folder, nothing inside it is walked or counted this cycle either) —
counted instead under `fileSkipReasons["case-twin names excluded"]`. A root
whose only irregularity is a detected case-twin still completes with
`fileReport=ran`: a collision is a **finding**, not a reason to abort.

```
level=INFO msg="file-report finding" kind=case-collision instance=radarr-main root=/data/media/Movies path="/data/media/Movies/My Name Is Earl" entryType=dir names="My Name Is Earl, My Name is Earl"
level=INFO msg="file report" instance=radarr-main type=radarr fileReport=ran duplicates=0 orphans=0 caseCollisions=1 fileSkipReasons="case-twin names excluded=2"
```

As with every other finding here, resolving it — merging the two folders,
renaming the stray one — is always a decision you make by hand, outside
cutoffarr; nothing in this pass ever touches a file.

### The mount-problem safeguard

The single most dangerous failure mode here isn't a bug in the matching
logic — it's a **half-mounted or unmounted media share** silently turning a
perfectly healthy library into what looks like thousands of missing files
(mass false "duplicates" from a stale bind mount) or an empty directory that
reads as every tracked file having vanished. Before trusting anything a walk
finds, cutoffarr checks, per root:

1. The mapped path exists and is a readable directory.
2. Of a sample of up to 100 of that root's own tracked files, at least 90%
   actually exist on disk.
3. If the root tracks any movie/series folders at all, it also tracks at
   least one FILE under it. This is the mirror image of check 2 — an
   unreadable or misnamed `movieFile.path`/`episodeFile.path` across an
   entire root would otherwise empty the tracked-file set while the tracked
   folders stayed populated, and every real file in those folders would then
   match its folder but no tracked file, misreporting the whole library as
   duplicates. A root nothing here manages at all (zero tracked folders too)
   is unaffected — that's a legitimate empty or unmanaged root, not a mount
   problem.
4. If the root tracks any files at all, the walk finds *at least one* video
   file somewhere under it.

Any one of these failing aborts **that root's** report with a `WARN` naming
it — never a flood of false findings. One root failing never affects any
other root or any other instance. A walk error partway through a root (e.g.
a permission problem on a subdirectory) aborts that root's report the same
way, for the same reason: a partial walk is not a report, it's a guess.

One more check runs before any of the above, for the whole instance rather
than one root: if the library is non-empty but **not a single tracked path
mapped to any configured root** — a `media_root_map` key typo (matching is
case-sensitive) is the most common cause — the per-root checks above never
even get a chance to run, because there is nothing tracked to sample. The
whole instance is aborted with a `WARN` naming `media_root_map` rather than
walking a root with an empty tracked set and reporting every real file as an
orphan. A `media_root_map` typo, or a legitimately unmapped tracked path,
that affects only *some* of the library (not all of it) is counted under
`fileSkipReasons` and also gets one `WARN` per instance — never a flood, one
line, however many paths it affects.

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
- **The reverse scan costs a second pass over the unmonitored half.** It
  re-uses the real decision rules rather than a cheap approximation of them,
  which means one `/moviefile` call per unmonitored movie whose *quality*
  cutoff is already met (the ones below it are decided from the paged wanted
  set alone), and — for a Sonarr series that has both monitored and
  unmonitored seasons — one extra `/episode` read. On a large library with
  many unmonitored items this is the most expensive part of a full cycle. It
  runs only on full cycles, never per webhook.
- **The file report's Sonarr side costs one `/episodefile` fetch per series.**
  Radarr's tracked-file paths are already embedded on the `/movie` response
  cutoffarr fetches anyway, so enabling the file report there costs nothing
  extra. Sonarr never exposes a file's path anywhere but `/episodefile`, and
  a complete, accurate tracked-file set needs every series' files regardless
  of monitored state — not just the ones the forward scan happened to touch
  — so this is a genuine per-series API cost on top of everything else a full
  cycle already does. Like the reverse scan, it runs only on full cycles
  (never per webhook, never on a `--only-id` scoped run) and is entirely
  opt-in per instance via `media_root_map`.
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
   on; anything else it receives is answered `200 OK` and ignored — but that
   is logged only at `log_level: debug`, so at the documented default
   (`info`) a wrongly-ticked trigger produces **no line at all** in
   `docker logs cutoffarr`. If you ticked something else by mistake, the
   *arr side shows a green checkmark either way (cutoffarr never answers
   4xx/5xx for an event type it just isn't interested in) — set
   `log_level: debug` temporarily if you need to confirm what actually
   arrived.
4. Save, then use the instance's own "Test" button to confirm cutoffarr
   receives it — check `docker logs cutoffarr` for the corresponding line.
   Unlike step 3's unhandled triggers, **Test** is the one event type that
   always logs at `info` (naming the instance), so this step works at the
   default log level with no setup.

Repeat once per configured instance — each one has its own name and its own
URL. A webhook fires an evaluation of only the movie or season the event
named, debounced by `webhook_debounce`; everything else eligible still gets
picked up by the next full reconciliation sweep (`poll_interval`, default
`24h`).

## The web dashboard

`GET http://cutoffarr:9898/` (or whatever host/port you've mapped
`webhook_port` to) is a small, self-contained page — no build step, no
JavaScript framework, nothing loaded from anywhere but this one response —
that shows, per configured instance, how much of the library is at rest
(unmonitored, nothing left to do) versus still hunting (monitored, still
being upgraded), plus anything that needs a human's attention: reverse-scan
findings and file-report duplicates/orphans/case-twins. There's a **Scan now** button
that queues one full sweep on demand, for whenever you don't want to wait for
the next `poll_interval` tick or restart the container.

It reads the SAME in-memory state the log lines already come from — nothing
it shows is computed specially for the page, and nothing about what it shows
changes what any cycle decides or writes. Before the first cycle has
completed (a container that only just started), the page and its JSON both
show an empty instance list rather than an error; give it a few seconds and
reload, or watch `docker logs cutoffarr` for "startup scan complete".

**No authentication.** This is a LAN homelab tool, the same posture the
webhook endpoint already has: nothing here exposes an `*arr` API key or any
control an operator on the LAN doesn't already have some other way to reach
(the dashboard can trigger a sweep; it cannot change what that sweep does,
and dry-run/write mode is still entirely a `config.yml` decision). Do not
publish this port to the open internet, same as you would not publish the
webhook one.

Two endpoints back the page, and either can be used on its own (a shell
script, a status-page widget, whatever) without ever loading the HTML:

- **`GET /api/stats`** — JSON, always `200`, never an error (a store with
  nothing in it yet just means an empty `instances` array):

  ```json
  {
    "instances": [
      {
        "name": "radarr-main",
        "type": "radarr",
        "total": 996,
        "monitored": 540,
        "unmonitored": 456,
        "wouldUnmonitor": 3,
        "lastRun": "2026-03-01T12:00:00Z",
        "lastCycleKind": "sweep",
        "reverseStatus": "ran",
        "reverseAsOf": "2026-03-01T12:00:00Z",
        "reverseFindings": [],
        "fileReport": { "status": "ran", "duplicates": 0, "orphans": 1, "caseCollisions": 0, "findings": [] },
        "lastActions": [],
        "lastCycleStatus": { "status": "ok" }
      }
    ],
    "dryRun": true,
    "version": "dev"
  }
  ```

  `lastCycleKind` is `startup`, `sweep`, `webhook`, or `once`. `lastRun`/
  `lastCycleKind` are `null` when this instance has never once completed a
  full evaluation — either because its only cycle(s) so far each aborted
  before finishing one (a profile-fetch failure, say — a `total`/`monitored`/
  `unmonitored` shown alongside a `null` `lastRun` is a real library read
  from a cycle that didn't get further), or because the daemon has never
  once been able to reach it at all (see `lastCycleStatus` below, where
  `total` and friends stay at `0`). An instance is absent from `instances`
  entirely only before its very first cycle has been ATTEMPTED — the brief
  window before the startup scan reaches it. A manual scan (below) reports
  itself as `sweep`, since it's mechanically the same full-library pass,
  just run on demand instead of on the timer.

  `fileReport.status` and `reverseStatus` are each the same three-way
  `ran`/`skipped`/`off` vocabulary the log's own `msg="file report"` line
  and `reverseScan` attr use: `off` means that pass has never run a
  complete, trustworthy cycle for this instance yet (`media_root_map` never
  set, for the file report; the reverse scan globally disabled, or no full
  sweep has reached it yet); `skipped` means it ran this cycle but could not
  be trusted (a tracked root read failed; an incomplete unmonitored
  wanted/cutoff set); `ran` means a clean, trustworthy pass. `duplicates`/
  `orphans`/`caseCollisions`/`reverseFindings` being empty/zero means "clean"
  ONLY when the matching status is `ran` — never conflate `off` or `skipped`
  with "clean". `caseCollisions` is always present (including `0`) whenever
  `fileReport.status` is `ran` or `skipped`, the same as `duplicates`/
  `orphans`.

  Each `fileReport.findings` item is `{"kind": "duplicate"|"orphan"|
  "case-collision", "group": "...", "path": "...", "display": "...", "count":
  N, "entryType": "dir"|"file", "names": [...]}`. `group`/`count` are present
  only on a `duplicate`; `entryType`/`names` are present only on a
  `case-collision` — `names` is `[{"name": "...", "tracked": bool}, ...]`,
  every colliding name found in that directory plus, wherever cutoffarr
  could tell, whether that exact name is or contains something the `*arr`
  actually tracks (see [Case-twin names](#case-twin-names)). `path` is the
  full cutoffarr-side (disk) path — the same one the `msg="file-report
  finding"` log line carries, and for a `case-collision` the CONTAINING
  DIRECTORY the collision was found in, never one of the colliding names
  itself. `display` is that same path relative to its mapped
  `media_root_map` root: the root's own last path segment, then the
  remainder underneath it (`Movies/Some Title/file.mkv`, never the full,
  potentially much longer host-specific mount path `path` carries). The
  dashboard renders `display` in the row itself and puts `path` only in that
  row's hover title.

  `reverseAsOf` is the timestamp of the reverse pass `reverseFindings` is
  CURRENTLY holding — the last cycle whose pass actually completed
  trustworthily (`reverseStatus: "ran"`), not the most recent cycle. On a
  `skipped` cycle the findings are last-known-good (preserved, not cleared)
  and `reverseAsOf` stays exactly where it was, so the dashboard can say
  "showing last complete sweep from &lt;time&gt;" instead of silently
  presenting stale findings as fresh. It is `null` until the first cycle
  that ever completes a trustworthy pass.

  `lastCycleStatus` is this instance's outcome on the single MOST RECENT
  cycle that named it, independent of everything else in this object:
  `{"status": "ok"}` once that cycle's decision engine actually completed an
  evaluation (`wouldUnmonitor` is a real, freshly computed number), or
  `{"status": "skipped", "reason": "..."}` for any of three warn-and-skip
  paths — the connectivity check failed, the library read failed, or (a
  round-4 review fix) the cycle reached the engine but aborted INSIDE it
  before finishing an evaluation (a quality-profile fetch failure, an
  exclusion-tag resolution failure). Unlike every other field here, it is
  never carried forward from an earlier cycle: an instance that cannot
  complete an evaluation for a week reports `skipped` on every poll, not a
  stale `ok` from the last time it could. The dashboard shows this as a clay
  "last sweep incomplete — &lt;reason&gt;" badge on that instance's shelf
  card; every other number on the card is left exactly as it last was.

  `lastActions` holds up to the last 50 confirmed `unmonitor`/`remonitor`
  writes across both directions; it's always empty in dry-run, because a
  rehearsal is never reported as an action taken.

- **`POST /api/scan`** — queues one full-library sweep (the same
  `fullLibraryScope`/reverse/file-report shape the reconciliation sweep and
  the startup scan use — never webhook-scoped, never `--only-id`-narrowed).
  Always `202`, with a body naming what actually happened:
  `{"status":"queued"}` the first time, or `{"status":"already-pending"}` if
  a cycle is already running or one is already queued — idempotent by
  design, so mashing the button never stacks up extra sweeps.

Both endpoints, and the page itself, live on the same listener and port as
the webhook endpoint (`webhook_port`); nothing about `POST
/webhook/{instance-name}`'s own routing, method handling, or response
changed to make room for them.

## FAQ

**Does it delete anything?**
No. cutoffarr never sends a delete of any kind, to either `*arr`. The only
field it ever writes is `monitored`: `false` on the forward path, and `true`
only when `reverse_scan_remonitor` is enabled (off by default — see
[Letting it fix them](#letting-it-fix-them)). This isn't just a
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
Only if you ask it to, and by default it can't. The reverse scan reports what
is unmonitored while still failing the criteria; **writing** those back to
monitored is a separate switch, `reverse_scan_remonitor`, which is **off by
default** — with it off that pass composes no write at all, in any mode. See
[The reverse scan](#the-reverse-scan) for what it reports, and
[Letting it fix them](#letting-it-fix-them) for exactly what the switch
permits and the three things it still refuses to do.

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

**I ticked a trigger other than On Import/On Upgrade — why do I see nothing in the logs?**
Because it's genuinely quiet, not broken: cutoffarr answers every webhook
event `200 OK` (the `*arr` side always shows success), but an event type it
doesn't act on is logged only at `log_level: debug` — the default (`info`)
prints nothing at all for it. That is different from the "Test" button
specifically, which always logs at `info` — visible at the default log level
and at `debug`, but like any other `info` line it is suppressed if you've set
`log_level: warn` or `error` (see [Webhook setup](#webhook-setup)). Set
`log_level: debug` temporarily if you need to see exactly which event type an
`*arr` sent.

## License

[MIT](LICENSE).
