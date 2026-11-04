package handler

import (
	"github.com/labstack/echo/v4"
	version "github.com/soulteary/version-kit/v4"
)

// All three are injected at build time via -ldflags "-X main.Xxx=..." and then assigned by main.
// Commit and BuildDate are newly added: before this, given a runner-manager binary
// there was no way to tell which commit it came from, so production debugging could only guess from the version number.
//
// We inject main's variables, not version-kit's package-level ones, so when the kit bumps a major version
// (the module path changes with it) you need not touch ldflags. That is exactly the step the kit's own upgrade notes keep warning about;
// if it is missed, nothing errors and the binary silently reports dev.
var (
	// Version semantic version number; /version and -version both report dev when not injected
	Version string
	// Commit git commit used for the build
	Commit string
	// BuildDate build time (RFC3339)
	BuildDate string
	// AssetVersion content fingerprint of the embedded static assets, appended as ?v= to /static/*.css|js in the page.
	// computed by main at startup and assigned in.
	AssetVersion string
)

// BuildInfo assembles version info from the current Version/Commit/BuildDate.
// GoVersion, Platform, and Compiler are filled in by version-kit from the runtime.
//
// Assembled on every call rather than once at package init: version-kit's HandlerConfig
// freezes the response body at construction time, but Version here may change at runtime (main sets it in
// after flag.Parse, and test cases also modify it). /version is not a hot path, so recomputing is fine.
func BuildInfo() *version.Info {
	v := Version
	if v == "" {
		v = "dev"
	}
	return version.New(v, Commit, BuildDate)
}

// VersionInfo returns the version info (returns dev when not injected).
//
// Uses version-kit's default HandlerConfig, that is, the trimmed-down values from Public(): only
// version (we do not inject Branch). Deliberately **not** enabling IncludeBuildDetails: with Basic Auth
// unconfigured, this endpoint is fully public, and go_version would let anyone match a published Go
// runtime CVE to the exact runtime version. The kit's own comment states this same reasoning.
//
// So the response body stays byte-for-byte identical to before the change: {"version":"..."}. Commit and build time go only through
// the -version command line, which runs locally and is not exposed.
//
// Write the response ourselves using ResolveConfig(...).JSONResponse(), rather than the kit's HTTP handler:
// v4 moved the net/http pieces into the httpadapter subpackage, leaving the root package with only the config for deciding what to serve
// API, the half frameworks like Echo, Gin, and chi use. So this file's code
// did not change a single line when moving to v4; only the import changed.
func VersionInfo(c echo.Context) error {
	body, status := version.ResolveConfig(version.HandlerConfig{Info: BuildInfo()}).JSONResponse()
	return c.JSONBlob(status, body)
}
