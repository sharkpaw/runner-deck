# Runner Deck Desktop

Runs the Manager on your own computer, so runners work as normal processes with no Docker. On a Mac
that means Xcode and iOS builds. The same window can also open the panel of any Runner Deck server.

Data lives in `~/.runner-deck`: `config/`, `runners/`, `manager.log` and `desktop.json`, which holds
the generated password for the local panel and the list of servers.

Closing the window on a Mac leaves the runners working; quitting the app asks first, then stops them.

## Build

Needs Go, Rust and Bun.

```sh
cd desktop
bun install
bun run build
```

The `.dmg` lands in `src-tauri/target/release/bundle/dmg/`. It is signed ad hoc, so on another Mac it
opens only with right click → Open until a Developer ID is set up.
