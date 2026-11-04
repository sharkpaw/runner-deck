# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
