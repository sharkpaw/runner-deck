# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- MCP endpoint at `/mcp`, so Claude and other clients can manage runners.
- OAuth 2.1 sign-in with PKCE and client registration: short JWT access tokens and rotating refresh tokens that keep a session for 180 days after its last use.
- Sessions page: see everything signed in and end any of it. Browser sign-ins now last 180 days too, and logging out ends the session on the server.
- Desktop app for macOS: the Manager runs on your own computer, runners work as normal processes with no Docker, so Xcode and iOS builds work. The same window opens the panels of your servers.
- Without container mode, runner output is kept in `runner.log`, so logs, job results and work cleanup work there too.
- Runners get 4 CPUs and 8 GB by default, editable from the panel, globally or per runner, without a restart.
- Warnings when a runner hits its memory or CPU limit.
- Activity log: starts, stops, job results, memory kills and errors, kept with their times.
- Recreating a runner pulls its image first.
- Job statistics: totals, success rate and average duration, overall and per runner.
- Saved CPU and memory history for up to 30 days, in 5-minute summaries.
- Clean a runner's work directory, old Docker build cache and unused images from the panel.
- Disk guard: past 85% use, untagged images and old build cache are removed automatically (`runners.disk_guard_percent`).
- A runner idle for 3 days gets its work directory cleaned automatically (`runners.auto_clean_days`).

## [2.1.0] - 2026-11-05

- Login page with a browser session and sign out. Scripts can keep using Basic Auth.
- Wrong passwords are limited to 10 per address and 100 overall in 15 minutes.
- A Manager started as root fixes the ownership of its directories, joins the Docker socket's group and switches to the app user. Registering a runner failed before, because `config.sh` refuses to run as root.

## [2.0.0] - 2026-11-04

First release of Runner Deck.

- Images are published to `ghcr.io/sharkpaw/runner-deck`.
- Turkish translation.
- New interface: sidebar, light and dark themes, and a logo.
- System resources page: CPU, memory and disk per runner, session charts and Docker disk usage (`GET /api/system`).
- The root `docker-compose.yml` deploys on Coolify as is.

[Unreleased]: https://github.com/sharkpaw/runner-deck/compare/v2.1.0...HEAD
[2.1.0]: https://github.com/sharkpaw/runner-deck/releases/tag/v2.1.0
[2.0.0]: https://github.com/sharkpaw/runner-deck/releases/tag/v2.0.0
