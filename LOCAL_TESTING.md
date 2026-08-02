# Running the gdrive-native-v2 build locally

Instructions for evaluating this branch as a real backup tool for a few
weeks. Fork-only document.

## 1. Build and install

```sh
cd ~/sandbox/kopia            # branch gdrive-native-v2
go build -o ~/bin/kopia-gdrive .
kopia-gdrive --version        # note the build; rebuild after any git pull
```

Install under a distinct name (`kopia-gdrive`) so a distro/upstream kopia
can coexist. On additional machines: either copy the binary (same
glibc/arch) or clone the fork and build there. Rebuild + replace is the
whole upgrade story; repositories and config files carry across builds.

## 2. Keep it isolated from any existing kopia use

Always pass a dedicated config file (or export `KOPIA_CONFIG_PATH`):

```sh
alias kg='kopia-gdrive --config-file ~/.config/kopia/gdrive-test.config'
```

Everything below assumes `kg`. Cache lives under the standard kopia cache
dir but is keyed by repository, so no collision with other repos.

## 3. Credentials — one OAuth client, per-user consent

You already have a working OAuth client (GCP project 949080498481, Drive
API enabled) and a refresh-token file (`$KOPIA_GDRIVE_TEST_CREDENTIALS`
in `~/secrets/gdrive.env`).

Two ways to authorize a machine:

- **Copy the token file** (simplest for your own machines, same Google
  account): copy it to the machine, `chmod 600`, and pass
  `--credentials-file /path/to/token.json` at create/connect. The loader
  accepts Google's `authorized_user` format and the type-less variant
  yours is in.
- **Fresh consent per machine** (what an org fleet would do): download
  the OAuth client JSON (Desktop type) from the GCP console, then pass
  `--client-credentials-file client.json` — a browser consent opens via
  the loopback flow, and the resulting token is cached at
  `~/.config/kopia/gdrive-token-<hash>.json` (0600). Headless box:
  `--device-flow` (drive.file scope only), or run the consent on a
  browser machine and copy the token cache file.

## 4. Create the repository

Under the default `drive.file` scope the backend can only see folders
**created by the app** — a folder made in the Drive web UI is invisible
(this is the scope's security property, not a bug). Create the repo
folder with the app's own identity. The rclone remote configured on this
machine shares the same OAuth client, so it counts as the same app:

```sh
rclone mkdir gdrive-bench:kopia-main-repo
rclone lsjson --dirs-only gdrive-bench: | python3 -c \
  "import json,sys; print({d['Name']:d['ID'] for d in json.load(sys.stdin)}['kopia-main-repo'])"
```

(Alternative: pass `--scope=drive` at create/connect and any folder —
including hand-created ones — works, at the cost of the token being able
to read your whole Drive.)

Then:

```sh
kg repository create gdrive \
  --folder-id <ID-from-above> \
  --credentials-file ~/secrets/<token>.json
kg repository validate-provider    # one-time sanity; ~1 min
```

Additional machines, same account: `kg repository connect gdrive
--folder-id <same-ID> --credentials-file <token>` — app-created files are
visible to the same (user × client) grant on any machine. A *different*
Google user connecting to a shared folder under `drive.file` is the one
documented-unverified case; use `--scope=drive` for that scenario for now.

## 5. Snapshots and scheduling

```sh
kg policy set --global --compression=zstd --keep-latest 30 --keep-daily 30
kg snapshot create ~/documents ~/projects        # first one is the slow one
```

Systemd user timer (per machine):

```ini
# ~/.config/systemd/user/kopia-gdrive.service
[Unit]
Description=kopia gdrive-native snapshot
[Service]
Type=oneshot
ExecStart=%h/bin/kopia-gdrive --config-file %h/.config/kopia/gdrive-test.config snapshot create ~/documents ~/projects

# ~/.config/systemd/user/kopia-gdrive.timer
[Unit]
Description=hourly kopia gdrive snapshot
[Timer]
OnCalendar=hourly
RandomizedDelaySec=10m
Persistent=true
[Install]
WantedBy=timers.target
```

```sh
systemctl --user enable --now kopia-gdrive.timer
```

Maintenance runs automatically from whichever machine owns it; nothing to
schedule.

## 6. What to watch during the trial

- **Weekly:** `kg snapshot verify --verify-files-percent=10` (spot-check
  restore path); occasionally `kg restore <snapshot-id> /tmp/restore-test`
  and diff a directory you know.
- **Monthly (or before relying on it):** `--verify-files-percent=100`.
  Note the 1 TB/day/project download quota if the repo grows large.
- **Errors:** the point of this build is that Drive failures surface as
  one clear line with the Google reason and remediation, not a 10-retry
  hang. If you ever see a stall or an unhelpful error, capture
  `~/.cache/kopia/<repo-id>/cli-logs/` from that window — that plus the
  reason string is a complete bug report.
- **Seeding:** initial upload of a large corpus hits Drive's 750 GB/day
  per-user cap; the backend backs off cleanly, but plan multi-day seeding
  for multi-TB sources.
- **Known rough edge:** pointing at a hand-created folder under the
  default scope fails with an actionable error (see §4). The planned fix
  is Google's Picker flow; workaround is app-created folders or
  `--scope=drive`.

## 7. Optional: automated regression watch

The benchmark harness can watch for performance regressions unattended:

```sh
cd ~/sandbox/kopia && go build -o ~/bin/gdrive-bench ./tools/gdrive-bench
source ~/secrets/gdrive.env
gdrive-bench run --smoke --label nightly --targets <targets.json>
gdrive-bench baseline check <newest-result.json>   # exit 1 on REAL regression
```

Wire those two lines into a nightly cron/timer if you want continuous
signal; the quota ledger caps its own spend.

## 8. Reverting

The repository format is standard kopia — nothing in this build changes
the on-Drive data layout contract beyond what upstream kopia writes. If
you stop testing, the blobs remain a valid kopia repository; upstream
kopia (once this backend merges) or this binary can both read it. Your
existing backups and any distro kopia install are untouched throughout.
