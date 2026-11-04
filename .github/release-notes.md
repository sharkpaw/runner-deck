## Runner Deck v2.1.0

- **Login page:** sign in from a page in the same design, with sign out. Scripts can keep using Basic Auth.
- **Wrong passwords** are limited to 10 per address and 100 overall in 15 minutes.
- **Fix:** adding a runner failed when the Manager ran as root. It now switches to the app user at startup.

**Images:** `ghcr.io/sharkpaw/runner-deck:v2.1.0` and `ghcr.io/sharkpaw/runner-deck:v2.1.0-runner`.
