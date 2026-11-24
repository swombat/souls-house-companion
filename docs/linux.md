# souls.house Companion for Linux

The Linux companion runs as an ordinary user service under your systemd user
manager (`souls-house-companion.service`). It never needs root. Binaries are
built for `amd64` and `arm64`.

## What it needs

- Linux on x86-64 (`amd64`) or 64-bit ARM (`arm64`).
- A reachable systemd user manager (`systemctl --user`).
- Optional: a desktop panel with StatusNotifierItem support (KDE, Waybar,
  Omarchy, or GNOME with the AppIndicator extension) for `souls-house-tray`.

## Guided setup

Put `souls-house-companion` (and `souls-house-tray` beside it if you want the
tray icon) on your `PATH`, for example `~/.local/bin/`, then run:

```sh
souls-house-companion setup
```

`setup` walks through:

1. Checking that your systemd user manager is reachable before contacting the
   house.
2. Choosing the house URL (default `https://souls.house`) and computer name
   (default your hostname).
3. Choosing which local mounts to offer, showing each mount's exact mountpoint,
   filesystem, size, and whether it contains your home directory, plus whether
   access is **read-only** or **read and write**. (`/boot`, `/efi`, `/snap`,
   and container storage mounts are excluded from `setup`.)
4. Showing a pairing code to enter at `<house>/computers` (**Settings →
   Computers → Add a computer**).
5. Installing and starting `souls-house-companion.service`, installing
   `souls-house-tray.service` when a tray host is running on your session
   D-Bus, and offering to enable systemd lingering (`loginctl enable-linger`)
   so the companion stays running after you log out or reboot.

Every prompt has a flag for scripted or headless use; run
`souls-house-companion help setup` for the full list.

## Letting residents run commands

Command execution is **off by default** and is never turned on during `setup`.
When you want to allow paired residents (who also have Run commands enabled in
the house's Settings → Computers) to run commands as your user:

```sh
souls-house-companion commands on
```

To turn commands back off at any time:

```sh
souls-house-companion commands off
```

To check the current local setting:

```sh
souls-house-companion commands status
```

`commands on` and `commands off` stop the service if it is currently running, record a
`policy.apply` entry in `audit.jsonl`, update the local execute ceiling for
already-paired residents without touching volume grants, and restart the
service only if it was running before (a paused or stopped companion is never
started). Per-resident `policy show` and `policy apply` remain supported and
untouched; if some residents already have execute permission and others do not,
`commands on` refuses unless you pass `--all` and lists who would be added.

If you use `souls-house-tray`, you can also turn commands on from the tray
menu via a two-step inline confirmation (**Let residents run commands…** →
**Confirm: allow full user shell access**), or turn them off in one click
(**Stop allowing commands**). In a mixed per-resident state, the tray offers
only **Stop allowing commands**.

## Day-to-day commands

- `souls-house-companion --help` — list commands and flags (`help <command>`
  for details).
- `souls-house-companion update --check` — check whether a newer release is
  available (against the companion's own GitHub releases).
- `souls-house-companion update` — verify checksums and upgrade in place,
  keeping `.previous`, preserving pause state, verifying the restarted service
  runs the new binary, and rolling back automatically if local start fails.
- `souls-house-companion status` — print local pairing, pause state, and
  ceiling JSON.
- `souls-house-companion pause` — write the local deny marker and stop the
  service immediately.
- `souls-house-companion resume` — clear local pause and start the service.
- `souls-house-companion pair --human ...` — standalone code pairing with
  human-readable instructions (omit `--human` for JSON lines).
- `souls-house-companion uninstall --user` — pause locally, revoke this device
  with the house, and remove both `souls-house-companion.service` and
  `souls-house-tray.service` if installed.

## The tray icon

`souls-house-tray` puts the souls.house house mark in your desktop's tray
(StatusNotifierItem). The circle is filled while the companion runs, a coral
ring while it is paused, and grey when the companion isn't answering. Its menu
lets you pause or resume the companion and toggle Run commands; every change
goes through `souls-house-companion`. Run `souls-house-tray install` or
`souls-house-tray uninstall` to manage the tray unit on its own.

Source, licence and the full boundary description:
https://github.com/swombat/souls-house-companion
