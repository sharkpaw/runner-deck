# Runner Deck Desktop

Runs the Manager on your own computer, so runners work as normal processes with no Docker. On a Mac
that means Xcode and iOS builds. The same window can also open the panel of any Runner Deck server.

Data lives in `~/.runner-deck`: `config/`, `runners/`, `manager.log` and `desktop.json`, which holds
the list of servers and the local panel's generated password. Server sessions are in the Keychain,
which is only read when a server's window has lost its own sign-in.

Closing the window leaves the app in the menu bar with the runners working. The icon there shows CPU
and memory, counted over the Manager, its runners and their jobs, plus the disk `~/.runner-deck` takes.
Its menu stops and starts the runners, opens the app at login, and quits, which asks first.

Opening a server signs in with OAuth in your browser the first time. The session then stays for 180
days after its last use, shows up on the server's Sessions page, and ends when you remove the server.

## Install

Download for [Apple Silicon](https://github.com/sharkpaw/runner-deck/releases/download/desktop-latest/RunnerDeck-macos-arm64.dmg) or [Intel](https://github.com/sharkpaw/runner-deck/releases/download/desktop-latest/RunnerDeck-macos-x64.dmg), and drag the app to Applications.

The app is not notarized by Apple yet, so macOS refuses to open it the first time. Allow it once:

```sh
xattr -dr com.apple.quarantine "/Applications/Runner Deck.app"
```

Or open it, then choose Open Anyway under System Settings → Privacy & Security.

The app updates itself from GitHub Releases, checking at launch and every 6 hours. It installs when no
job is running, because an update restarts the runners.

## Release

Raise `version` in `src-tauri/tauri.conf.json` and merge to `main`. The workflow builds both Macs and
publishes the release, and the links above then give the new version. Installed apps pick it up from
its `latest.json`. Publishing needs the `TAURI_SIGNING_PRIVATE_KEY` repository secret; losing that key
means installed apps can no longer update.

With these secrets as well, the build is signed with a Developer ID and notarized: `APPLE_CERTIFICATE`
(the `.p12` in base64), `APPLE_CERTIFICATE_PASSWORD`, `APPLE_SIGNING_IDENTITY`, `APPLE_ID`,
`APPLE_PASSWORD` (an app-specific password) and `APPLE_TEAM_ID`.

## Build

Needs Go, Rust and Bun.

```sh
cd desktop
bun install
bun run build
```

The `.dmg` lands in `src-tauri/target/release/bundle/dmg/`.
