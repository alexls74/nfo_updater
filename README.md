# NFO Updater

Keeps the ratings in your Kodi-style `.nfo` files up to date.

## Features

- **Six rating sources** — IMDb, TMDb, Rotten Tomatoes (critics and audience), Trakt and Metacritic. Each one can be switched on or off separately.
- **Movies and TV shows** — processing separately: movies, series and individual episodes.
- **Kodi-standard output** — named entries inside `<ratings>`. No flat tags, no proprietary extensions.
- **Careful with data** — if there are ratings in the original file that are not mentioned above, they are saved unchanged.
- **Backups** — the originals of every changed file can be archived per run.
- **Quota handling** — several keys per service are used one after another,
  with a daily counter kept per key.
- **Metadata repairs** — adds modern `<uniqueid>` entries for titles that only
  carry legacy ids, and fills in a missing `<premiered>` date.
- **Crew order fix** — puts `<credits>` and `<director>` below the cast for Emby server.
- **Library refresh** — asks Emby, Jellyfin or Plex to rescan once a pass has
  actually changed something.
- **Scheduled operation** — a resident daemon with a cron-style schedule.
- **Setup wizard** — asks where the library is and which keys to use, verifying every key and every server address as you enter it.
- **Per-run logs** — one file per pass with a summary at the end.

## Installation and configuration

### Debian / Ubuntu

You will need a 64-bit x86 Linux machine, `curl` or `wget`, `tar`, `sha256sum`.<br>
`Systemd` is needed only if you want the library updated on a schedule.

To install:

```sh
wget -qO- https://raw.githubusercontent.com/alexls74/nfo_updater/main/nfo_updater.sh | sh -s -- install
```

or:

```sh
curl -fsSL https://raw.githubusercontent.com/alexls74/nfo_updater/main/nfo_updater.sh | sh -s -- install
```

> You should run this as user, **not** under `sudo`. The configuration belongs to current user. Under `sudo` it would be written to root's home directory instead. The installer asks for a password itself, and only for the steps that need it.

The installer downloads the release, verifies its checksum and hands over to the setup wizard, which asks for:

- whether the library should be updated on a schedule, and how often;
- where to store user data;
- the directories your movies and TV shows live in;
- the API keys (see [Configuration](#configuration) — have them ready before you start);
- optional media servers configuration.

Answer *yes* to the schedule and the installer registers a systemd service; answer *no* and you get a program you start yourself. Nothing is written to disk until the last question has been answered, so leaving halfway changes nothing at all.

#### Installer commands

The same script handles the whole lifecycle. Run it the same way, replacing `install` with any of these:

| Command | What it does |
| --- | --- |
| *(none)* | Show a menu. |
| `install` | Download, set up and install. |
| `update` | Update to the latest release. `update x.x.x` goes to a specific one. |
| `configure` | Go through the setup wizard again. |
| `service on` | Register the service for an already installed program. |
| `service off` | Remove the service. The program and its settings stay. |
| `status` | Show what is installed, where, and what the service is doing. |
| `remove` | Remove the program and the service. |
| `help` | Show the list of commands. |

`update` replaces the binary in place, whichever directory it was installed into.<br>
`remove` leaves your configuration, database, logs and backups untouched — see [Where things live](#where-things-live) if you want them gone too.

#### Configuration

The installation wizard provides mandatory settings. All settings are described below.

##### API keys

All services have to be configured. Every key is verified at the start of each pass, so one that has expired or been revoked is reported in the log right away.

| Service | Where to get a key | Free tier | Setting |
| --- | --- | --- | --- |
| OMDb | [omdbapi.com/apikey.aspx](https://www.omdbapi.com/apikey.aspx) | 1000 requests/day per key | `OMDB_API_KEYS` |
| MDBList | [mdblist.com/preferences](https://mdblist.com/preferences/) | 1000 requests/day per key | `MDBLIST_API_KEYS` |
| TMDb | [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api) | no daily limit | `TMDB_API_KEY` |

> **TMDb.** Use the value labelled **API Key** — 32 characters, digits and lowercase `a`–`f`. The **API Read Access Token** shown on the same page belongs to a different authentication scheme and will be rejected.

OMDb and MDBList accept several keys, comma-separated without spaces. They are used one after another as each daily quota runs out.

##### Settings

| Setting | What it is for |
| --- | --- |
| `MOVIES_PATH`, `TVSHOWS_PATH` | Where your library lives. Comma-separated for several directories. |
| `IMDB_RATING`, `TMDB_RATING`, `POPCORN_RATING`, `TRAKT_RATING`, `TOMATOES_RATING`, `METACRITIC_RATING` | Which ratings to write. `yes` or `no` each. Switching a rating to `no` removes it from your files on the next pass. A rating left at `yes` keeps its previous value when it cannot be fetched. |
| `DEFAULT_RATING` | Which of the enabled ratings is the main one for an item, marked `default="true"` in the file. Default: `imdb`. |
| `CREW_ORDER_FIX` | Move `<credits>` and `<director>` below the cast. This is about display order, and nothing is added or removed either way. Emby follows the order of the file exactly,  so a file that lists the crew above the cast is displayed crew first. |
| `SCHEDULE` | Cron expression for scheduled operation. Empty means every Monday at 03:00. |
| `BACKUP_ENABLED`, `BACKUP_DIR`, `BACKUP_LIMIT` | Backups of the original files, and how many archives to keep per category. |
| `LOG_ENABLED`, `LOG_VERBOSE`, `LOG_DIR`, `LOG_LIMIT` | Log files, and how many to keep. |
| `DATABASE_PATH` | Where the rating cache is kept. |
| `EMBY_*`, `JELLYFIN_*`, `PLEX_*` | Optional: ask a media server to rescan after a pass. |

Rules worth knowing before you edit:

- **Every path must be absolute and written out in full.** A leading `~` is expanded by your shell, not by NFO Updater, so `~/some_path/logs` stays a relative path here and is rejected. Write `/home/yourname/some_path/logs` instead.
- Movie paths must not overlap TV show paths, and no path may sit inside another. Symbolic links are not followed.
- Leave `DATABASE_PATH`, `LOG_DIR` and `BACKUP_DIR` empty to keep everything in one directory under your home folder.
- Switching a rating to `no` removes it from your files on the next pass. A rating left at `yes` keeps its previous value when it cannot be fetched. Ratings written by other tools are never touched either way.
- Enabling a media server means filling in both its address and its key, otherwise the configuration is rejected. The address has to be reachable from the machine NFO Updater runs on, which is not necessarily the machine your browser runs on.

Backups are zip archives, one per pass, kept separately for movies and TV shows:

***backups/Movies/2026-08-10_03-00-11.zip<br>
backups/TVShows/2026-08-10_03-00-11.zip***

Each archive holds only the files that pass actually changed, every one with its full path. The oldest archives beyond `BACKUP_LIMIT` are deleted as new ones appear, counted per category.

##### Applying changes

If the service is running, tell it to re-read the file:

```sh
sudo systemctl reload nfo_updater
```

> Use `reload`, not `restart`.

If a pass is running at that moment, the new configuration is applied as soon as it ends. A configuration that fails to parse is reported in the log and the old one stays in force.

#### Usage

##### As a service

Installed with a schedule, NFO Updater runs as a systemd service and needs no attention.
The commands you may still want:

```sh
systemctl status nfo_updater            # is it running, when did it last work
journalctl -u nfo_updater -f            # follow along
systemctl kill -s USR1 nfo_updater      # start a pass right now, off schedule
sudo systemctl reload nfo_updater       # re-read the configuration file
sudo systemctl stop nfo_updater         # stop, waiting for a running pass to finish
```

A pass in progress is never cut short: on stop the daemon finishes the file it is on, packs its backups and only then exits. systemd waits up to five minutes for this.

##### One-shot

Without a schedule — or whenever you want a pass right now — run it yourself:

```sh
nfo_updater
```

It performs a single pass over the library and exits. Only one instance works at a time: started while another pass is running, it says so and exits without touching anything. The following flags can be used:

| Flag | What it does |
| --- | --- |
| *(none)* | A single pass over the library. |
| `--setup` | The setup wizard. Safe to run again at any time. |
| `-d` | Run as a daemon: stay resident, start a pass on the schedule. |
| `-v` | Show the version and the paths this instance would use. |
| `--config PATH` | Use a different configuration file. Default: `~/.config/nfo_updater/config.conf` |
| `-h`, `--help` | Full help, including the exit codes. |

#### Where things live

| What | Path |
| --- | --- |
| Configuration | `~/.config/nfo_updater/config.conf` |
| Rating cache | `~/.local/share/nfo_updater/database.db` |
| Logs | `~/.local/share/nfo_updater/logs/` |
| Backups | `~/.local/share/nfo_updater/backups/` |
| Program | `/usr/local/bin/nfo_updater`, or `~/.local/bin/nfo_updater` with `--user` |
| Service unit | `/etc/systemd/system/nfo_updater.service` |

The **Configuration** can be redirected using the `--config` flag.<br>
**Rating cache**, **Logs**, **Backups** can be replaced with custom paths in the configuration file.

## Roadmap

- [x] Ratings: IMDb, TMDb, Rotten Tomatoes, Trakt, Metacritic.
- [x] Fix legacy tags.
- [x] Crew, cast reorder.
- [ ] Docker support.
- [ ] i18n support.

## License

MIT — see [LICENSE](LICENSE). The binary is statically linked; notices for the
bundled dependencies are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
