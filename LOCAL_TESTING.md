# Running the gdrive-native-v2 build locally

Instructions for evaluating this branch as a real backup tool across
several machines. Everything here uses only the kopia binary built from
this branch — no rclone, no helper scripts. Fork-only document.

## 1. Build and install

```sh
cd ~/sandbox/kopia            # branch gdrive-native-v2
go build -o ~/bin/kopia-gdrive .
kopia-gdrive --version
```

Install under a distinct name (`kopia-gdrive`) so a distro/upstream kopia
can coexist. For other machines: copy the binary (same arch/libc) or
build there. Upgrading = rebuild + replace the binary; repositories and
configs carry across builds unchanged.

## 2. Keep it isolated from any existing kopia use

Always pass a dedicated config file (or export `KOPIA_CONFIG_PATH`):

```sh
alias kg='kopia-gdrive --config-file ~/.config/kopia/gdrive-test.config'
```

## 3. Credentials — one OAuth client, per-machine authorization

You have one OAuth client (GCP project 949080498481, Drive API enabled).
Each machine needs a user authorization against it, one of:

- **Copy a token file** (same Google account on every machine): copy the
  `authorized_user` JSON to the machine, `chmod 600`, pass
  `--credentials-file /path/to/token.json`. The loader also accepts the
  type-less variant your current file is in.
- **Fresh consent on the machine** (its own Google account, or cleaner
  hygiene): download the Desktop-type client JSON from the GCP console
  once, ship it with the binary, and pass
  `--client-credentials-file client.json` — a browser consent runs via
  the loopback flow and the token is cached at
  `~/.config/kopia/gdrive-token-<hash>.json` (0600). Headless machine:
  add `--device-flow` (works with the default drive.file scope), or do
  the consent on a browser machine and copy the token cache file over.

The default `drive.file` scope means the token can only ever see files
this app created — a stolen laptop token cannot read the rest of the
Drive account.

## 4. Choose a repository layout for client machines

- **Per-machine repositories (recommended for client machines):** each
  machine creates its own repo folder under its own authorization. Best
  isolation, per-account quota, and no dependence on cross-user folder
  visibility.
- **One shared repository (fine when every machine authorizes as the
  same Google account):** create once, connect everywhere with the
  folder ID. Kopia handles many sources in one repo natively.
- **Avoid for now:** one shared repo where machines authorize as
  *different* Google users under `drive.file` — cross-user visibility of
  app-created files is the one documented-but-unverified Drive behavior;
  use `--scope=drive` there or wait until we've live-tested it.

## 5. Create / connect

First machine (or every machine, in the per-machine layout):

```sh
kg repository create gdrive \
  --create-folder-name kopia-backup-$(hostname) \
  --credentials-file ~/secrets/<token>.json
```

The backend creates (or idempotently reuses) the app-owned folder in the
account's My Drive root, prints the resolved folder ID plus a
ready-to-paste connect command, and stores the ID in the config. There is
no need to touch the Drive web UI at all. (A folder made by hand in the
web UI is invisible to the app under `drive.file` — if you must use one,
add `--scope=drive`.)

Additional machines joining a shared repo (same Google account):

```sh
kg repository connect gdrive \
  --folder-id <ID printed at create> \
  --credentials-file ~/secrets/<token>.json
```

One-time sanity check on any machine: `kg repository validate-provider`
(~1 min, exercises the provider contract against the live folder).

`kg repository status` shows the folder ID and name any time.

## 6. Snapshots and scheduling

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
loginctl enable-linger $USER    # timers fire without an active session
```

Maintenance runs automatically from whichever machine owns it.

## 7. What to watch during the trial

- **Weekly:** `kg snapshot verify --verify-files-percent=10`; now and
  then a real `kg restore <snapshot-id> /tmp/restore-test` + diff.
- **Monthly, or before relying on it:** `--verify-files-percent=100`
  (mind the 1 TB/day/project download quota on big repos).
- **Errors:** Drive failures should surface as one clear line carrying
  the Google reason and a remediation, never a silent 10-retry hang. If
  anything stalls or an error is unhelpful, capture
  `~/.cache/kopia/<repo-id>/cli-logs/` from that window — that plus the
  reason string is a complete bug report.
- **Seeding:** initial upload of a large corpus hits Drive's 750 GB/day
  per-user cap; the backend fails fast with a clear message rather than
  spinning, but plan multi-day seeding for multi-TB sources.
- **Client-machine hygiene:** this is a pre-upstream build — keep these
  repos separate from any production backup the machines already have,
  and record each repo's password somewhere recoverable (there is no
  reset).

## 8. Optional: automated regression watch (dev box only)

```sh
cd ~/sandbox/kopia && go build -o ~/bin/gdrive-bench ./tools/gdrive-bench
source ~/secrets/gdrive.env
gdrive-bench run --smoke --label nightly --targets <targets.json>   # native-only targets
gdrive-bench baseline check <newest-result.json>                    # exit 1 on REAL regression
```

The harness needs only the native lane for regression watching; the
quota ledger caps its own spend.

## 9. Reverting

The on-Drive format is standard kopia — nothing in this build changes
the repository data contract. If you stop testing, the blobs remain a
valid kopia repository readable by this binary or by upstream kopia once
the backend merges. Existing backups and any distro kopia install are
untouched throughout.
