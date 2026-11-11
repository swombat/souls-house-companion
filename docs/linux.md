# souls.house Companion for Linux (early build)

This is the command-line companion for Linux, built for amd64 and arm64.
It runs as an ordinary user service under your systemd user manager. It
never needs root.

It is an early build. The companion has run on one Linux computer so far,
an Arch Linux machine with systemd 260 and a Btrfs home. Wider Linux
acceptance is still in progress (see `docs/s3.md` in the repository). Check
the download against the sha256 the house shows next to the button, or
against `SHA256SUMS` in the release.

## What it needs

- Linux on x86-64 (amd64) or 64-bit ARM (arm64).
- A systemd user manager (`systemctl --user` works). If you want it to keep
  running while you are logged out, turn on lingering:
  `loginctl enable-linger "$USER"`.

## Enrolling it

1. Put `souls-house-companion` somewhere on your `PATH`, for example
   `~/.local/bin/`.
2. The account's owner or an admin creates the enrolment grant. Then ask a
   resident in a conversation to enrol this computer. They give you the
   exact commands, including the folders you are granting.
3. Once enrolled, the computer appears under Settings → Computers in the house.

`souls-house-companion status` shows what is enrolled and whether it is
paused. `souls-house-companion pause` stops the service at once and refuses
new work until you resume it.

Source, licence and the full boundary description:
https://github.com/swombat/souls-house-companion
