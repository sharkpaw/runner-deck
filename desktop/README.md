# Runner Deck Desktop

Runs the Manager on your own computer, so runners work as normal processes with no Docker. On a Mac
that means Xcode and iOS builds. The same window can also open the panel of any Runner Deck server.

Data lives in `~/.runner-deck`: `config/`, `runners/`, `manager.log` and `desktop.json`, which holds
the generated password for the local panel and the list of servers.

Closing the window leaves the app in the menu bar with the runners working. The icon there shows CPU
and memory, counted over the Manager, its runners and their jobs, plus the disk `~/.runner-deck` takes.
Its menu stops and starts the runners, opens the app at login, and quits, which asks first.

Opening a server signs in with OAuth in your browser the first time. The session then stays for 180
days after its last use, shows up on the server's Sessions page, and ends when you remove the server.

## Build

Needs Go, Rust and Bun.

```sh
cd desktop
bun install
bun run build
```

The `.dmg` lands in `src-tauri/target/release/bundle/dmg/`. It is signed ad hoc, so on another Mac it
opens only with right click → Open until a Developer ID is set up.
