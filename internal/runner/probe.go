package runner

import (
	"errors"
	"strings"
)

// ProbeErrorType identifies the type of container status probe failure, for API/UI classification.
type ProbeErrorType string

const (
	ProbeErrorTypeUnknown      ProbeErrorType = "unknown"
	ProbeErrorTypeDockerAccess ProbeErrorType = "docker-access"
	ProbeErrorTypeAgentHTTP    ProbeErrorType = "agent-http"
	ProbeErrorTypeAgentConnect ProbeErrorType = "agent-connect"
)

// ProbeError wraps the underlying error and carries a machine-readable failure type.
type ProbeError struct {
	Type ProbeErrorType
	Err  error
}

func (e *ProbeError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ProbeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newProbeError(t ProbeErrorType, err error) error {
	if err == nil {
		return nil
	}
	return &ProbeError{Type: t, Err: err}
}

// DetectProbeErrorType extracts the probe error type for unified use by handler/API/UI.
func DetectProbeErrorType(err error) ProbeErrorType {
	if err == nil {
		return ProbeErrorTypeUnknown
	}
	var pe *ProbeError
	if errors.As(err, &pe) && pe != nil && pe.Type != "" {
		return pe.Type
	}
	// Compatible with legacy error strings (avoids losing classification when old paths are not wrapped)
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "docker"), strings.Contains(msg, "daemon"), strings.Contains(msg, "socket"):
		return ProbeErrorTypeDockerAccess
	case strings.Contains(msg, "agent returned"):
		return ProbeErrorTypeAgentHTTP
	case strings.Contains(msg, "connect"), strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"):
		return ProbeErrorTypeAgentConnect
	default:
		return ProbeErrorTypeUnknown
	}
}

// ProbeSuggestion returns a short troubleshooting suggestion to the caller.
func ProbeSuggestion(t ProbeErrorType) string {
	switch t {
	case ProbeErrorTypeDockerAccess:
		return "check the docker.sock mount and its permissions (DOCKER_GID/group_add/user), and that the Docker daemon is reachable"
	case ProbeErrorTypeAgentConnect:
		return "check the runner container network, DNS, and that the Agent port is reachable"
	case ProbeErrorTypeAgentHTTP:
		return "read the runner container logs and check the Agent and the scripts under /runner"
	default:
		return "try a stop/start to self-heal first, then read the manager and runner container logs"
	}
}

// ProbeCheckCommand returns a read-only check command (no side effects).
func ProbeCheckCommand(t ProbeErrorType) string {
	switch t {
	case ProbeErrorTypeDockerAccess:
		return "ls -l /var/run/docker.sock && id && docker info"
	case ProbeErrorTypeAgentConnect:
		return "docker network inspect runner-net && docker ps --format \"table {{.Names}}\\t{{.Status}}\\t{{.Networks}}\""
	case ProbeErrorTypeAgentHTTP:
		return "docker ps -a | rg \"github-runner-\" && docker logs --tail=200 <runner_container_name>"
	default:
		return "docker compose ps && docker logs --tail=200 runner-manager"
	}
}

// ProbeFixCommand returns a fix command that may have side effects.
func ProbeFixCommand(t ProbeErrorType) string {
	switch t {
	case ProbeErrorTypeDockerAccess:
		return "DOCKER_GID=$(stat -c \"%g\" /var/run/docker.sock 2>/dev/null || stat -f \"%g\" /var/run/docker.sock) && echo \"DOCKER_GID=$DOCKER_GID\" > .env && docker compose up -d"
	case ProbeErrorTypeAgentConnect:
		return "docker compose up -d && docker restart runner-manager"
	case ProbeErrorTypeAgentHTTP:
		return "docker restart <runner_container_name>"
	default:
		return "docker compose up -d --force-recreate"
	}
}
