package runner

import (
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// hostSocketSpec is a typical host-socket container form
func hostSocketSpec() containerSpec {
	return containerSpec{
		ContainerName: "github-runner-a",
		Image:         "img:tag",
		Network:       "runner-net",
		MountSrc:      "/host/runners/a",
		JobBackend:    "host-socket",
		DindHost:      "runner-dind",
		DockerGID:     999,
	}
}

// factsFor builds an inspect result that matches the spec (with creation labels, i.e. a container made by the new version)
func factsFor(s containerSpec) *containerFacts {
	f := legacyFactsFor(s)
	f.Labels = map[string]string{
		labelJobBackend: s.JobBackend,
		labelNetwork:    s.Network,
	}
	return f
}

// legacyFactsFor has no labels, simulating a container created by an old version
func legacyFactsFor(s containerSpec) *containerFacts {
	f := &containerFacts{
		Running:     true,
		Status:      "running",
		ImageRef:    s.Image,
		ImageID:     "sha256:aaa",
		Binds:       []string{s.runnerBind()},
		Networks:    []string{s.Network},
		NetworkMode: s.Network,
	}
	if host := s.dockerHostEnv(); host != "" {
		f.Env = append(f.Env, "DOCKER_HOST="+host)
	}
	if s.JobBackend == "host-socket" {
		f.Binds = append(f.Binds, HostDockerSocket+":"+HostDockerSocket)
		if s.DockerGID >= 0 {
			f.GroupAdd = []string{"999"}
		}
	}
	return f
}

func TestDriftReason_NoDriftWhenMatching(t *testing.T) {
	spec := hostSocketSpec()
	if got := spec.driftReason(factsFor(spec), "sha256:aaa"); got != "" {
		t.Fatalf("a consistent container should not report drift: %s", got)
	}
	// Should not report drift either when the container does not exist
	if got := spec.driftReason(nil, "sha256:aaa"); got != "" {
		t.Fatalf("nil facts should not report drift: %s", got)
	}
}

func TestDriftReason_BackendSwitch(t *testing.T) {
	// Real scenario: the container was created with host-socket, and the config was changed to dind.
	// Without a rebuild it still mounts the host socket, so the switch has no effect.
	old := hostSocketSpec()
	facts := factsFor(old)

	want := old
	want.JobBackend = "dind"
	got := want.driftReason(facts, "sha256:aaa")
	if !strings.Contains(got, "dind") {
		t.Fatalf("host-socket → dind not detected: %q", got)
	}

	// Reverse: container created with dind, config changed back to host-socket
	dindSpec := old
	dindSpec.JobBackend = "dind"
	dindFacts := factsFor(dindSpec)
	if got := old.driftReason(dindFacts, "sha256:aaa"); !strings.Contains(got, "host-socket") {
		t.Fatalf("dind → host-socket not detected: %q", got)
	}

	// Switch to none: there should be neither DOCKER_HOST nor a socket mount
	noneSpec := old
	noneSpec.JobBackend = "none"
	if got := noneSpec.driftReason(facts, "sha256:aaa"); !strings.Contains(got, "none") {
		t.Fatalf("host-socket → none not detected: %q", got)
	}
	if got := noneSpec.driftReason(factsFor(noneSpec), "sha256:aaa"); got != "" {
		t.Fatalf("none container with none config should not report drift: %q", got)
	}
}

func TestDriftReason_DindHostChanged(t *testing.T) {
	spec := hostSocketSpec()
	spec.JobBackend = "dind"
	facts := factsFor(spec)

	moved := spec
	moved.DindHost = "other-dind"
	got := moved.driftReason(facts, "sha256:aaa")
	if !strings.Contains(got, "other-dind") {
		t.Fatalf("DinD address change not detected: %q", got)
	}
}

func TestDriftReason_ImageRefAndRebuild(t *testing.T) {
	spec := hostSocketSpec()
	facts := factsFor(spec)

	// Image reference changed
	newImage := spec
	newImage.Image = "img:v2"
	if got := newImage.driftReason(facts, "sha256:aaa"); !strings.Contains(got, "img:v2") {
		t.Fatalf("image reference change not detected: %q", got)
	}

	// Same tag rebuilt: the reference is unchanged but the image ID changed, so it still requires a rebuild
	if got := spec.driftReason(facts, "sha256:bbb"); !strings.Contains(got, "rebuilt") {
		t.Fatalf("same-tag rebuild not detected: %q", got)
	}

	// Must not false-positive when the image is not local (no ID available)
	if got := spec.driftReason(facts, ""); got != "" {
		t.Fatalf("should not report drift when image ID is unavailable: %q", got)
	}
}

func TestDriftReason_NetworkAndMount(t *testing.T) {
	spec := hostSocketSpec()
	facts := factsFor(spec)

	otherNet := spec
	otherNet.Network = "other-net"
	if got := otherNet.driftReason(facts, "sha256:aaa"); !strings.Contains(got, "other-net") {
		t.Fatalf("network change not detected: %q", got)
	}

	// volume_host_path changed, so the mount directory follows
	moved := spec
	moved.MountSrc = "/data/runners/a"
	if got := moved.driftReason(facts, "sha256:aaa"); !strings.Contains(got, "/data/runners/a") {
		t.Fatalf("mount directory change not detected: %q", got)
	}
}

// TestDriftReason_NetworkUsesCreationNotAttachment
// docker create --network sets only one network, but a container can be attached later with docker network connect
// to other networks. So 'is it connected to the target network?' cannot answer 'which network was it created with?':
// When the config changes from runner-net to net-b and the container is connected to both, the check 'connected means it counts' would let it pass,
// so it stays connected to runner-net, the very link this change was meant to sever.
func TestDriftReason_NetworkUsesCreationNotAttachment(t *testing.T) {
	spec := hostSocketSpec() // Network = runner-net

	// Containers created by the new version: the creation label is authoritative
	labeled := factsFor(spec)
	labeled.Networks = []string{"runner-net", "net-b"} // Manually connected to net-b

	// Config is still runner-net: the extra network was attached by ops themselves, so it should not trigger a rebuild
	if got := spec.driftReason(labeled, "sha256:aaa"); got != "" {
		t.Fatalf("an extra attached network should not trigger a rebuild: %q", got)
	}

	// Config changed to net-b: the label still records runner-net, so it must be reported
	moved := spec
	moved.Network = "net-b"
	if got := moved.driftReason(labeled, "sha256:aaa"); got != "container_network: runner-net → net-b" {
		t.Fatalf("still attached to the old network after config change, not detected: %q", got)
	}

	// Containers created by an old version have no label, so fall back to inferring from NetworkMode (it is the parameter used at create time)
	legacy := legacyFactsFor(spec)
	legacy.Networks = []string{"runner-net", "net-b"}
	if got := moved.driftReason(legacy, "sha256:aaa"); got != "container_network: runner-net → net-b" {
		t.Fatalf("old container NetworkMode inference not detected: %q", got)
	}
	if got := spec.driftReason(legacy, "sha256:aaa"); got != "" {
		t.Fatalf("old container with NetworkMode matching config should not report: %q", got)
	}

	// When NetworkMode is also unavailable, keep the original 'connected counts' check; better to miss a drift than rebuild without cause
	blind := legacyFactsFor(spec)
	blind.NetworkMode = ""
	blind.Networks = []string{"runner-net", "net-b"}
	if got := moved.driftReason(blind, "sha256:aaa"); got != "" {
		t.Fatalf("should not report drift when info is incomplete: %q", got)
	}
}

// TestCreateArgs_RecordsNetworkLabel: creation must record the network in a label, otherwise later checks can only rely on inference
func TestCreateArgs_RecordsNetworkLabel(t *testing.T) {
	spec := hostSocketSpec()
	args, err := spec.createArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, labelNetwork+"=runner-net") {
		t.Fatalf("create parameter is missing network label: %s", joined)
	}
}

func TestDriftReason_DockerGID(t *testing.T) {
	spec := hostSocketSpec()
	facts := factsFor(spec)

	changed := spec
	changed.DockerGID = 983
	if got := changed.driftReason(facts, "sha256:aaa"); !strings.Contains(got, "983") {
		t.Fatalf("docker group GID change not detected: %q", got)
	}

	// When the GID can't be probed, don't append --group-add; with nothing to compare against, it must not raise a false alarm
	unknown := spec
	unknown.DockerGID = unknownDockerGID
	if got := unknown.driftReason(facts, "sha256:aaa"); got != "" {
		t.Fatalf("drift must not be reported when GID is unknown: %q", got)
	}
}

// TestDriftReason_LegacyContainerWithoutNetworkInfo An old container or one whose inspect lacks fields stays silent,
// better to miss a drift than to delete and recreate a container on no evidence.
func TestDriftReason_MissingFieldsStaySilent(t *testing.T) {
	spec := hostSocketSpec()
	facts := factsFor(spec)
	facts.Networks = nil
	facts.ImageRef = ""
	facts.ImageID = ""
	if got := spec.driftReason(facts, "sha256:aaa"); got != "" {
		t.Fatalf("drift must not be reported when fields are missing: %q", got)
	}
}

func TestParseInspect(t *testing.T) {
	out := []byte(`[{
	  "Image": "sha256:abc",
	  "State": {"Status": "running", "Running": true},
	  "Config": {"Image": "img:tag", "Env": ["PATH=/usr/bin", "DOCKER_HOST=unix:///var/run/docker.sock"]},
	  "HostConfig": {
	    "Binds": ["/host/runners/a:/runner", "/var/run/docker.sock:/var/run/docker.sock"],
	    "GroupAdd": ["999"],
	    "NetworkMode": "runner-net"
	  },
	  "NetworkSettings": {"Networks": {"runner-net": {}}}
	}]`)
	facts, err := parseInspect(out)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Running || facts.Status != "running" {
		t.Fatalf("status parse error: %+v", facts)
	}
	if facts.ImageRef != "img:tag" || facts.ImageID != "sha256:abc" {
		t.Fatalf("image parse error: %+v", facts)
	}
	if !containsString(facts.Networks, "runner-net") {
		t.Fatalf("network parse error: %+v", facts.Networks)
	}
	if src, ok := bindSource(facts.Binds, runnerMountDest); !ok || src != "/host/runners/a" {
		t.Fatalf("mount parse error: %v", facts.Binds)
	}
	if v, ok := envValue(facts.Env, "DOCKER_HOST"); !ok || v != "unix:///var/run/docker.sock" {
		t.Fatalf("environment variable parse error: %v", facts.Env)
	}

	// An empty array means the container does not exist
	empty, err := parseInspect([]byte(`[]`))
	if err != nil || empty != nil {
		t.Fatalf("empty result should return nil, nil, got %v, %v", empty, err)
	}
}

// TestDesiredContainerSpec_UsesVolumeHostPath When the Manager runs inside a container, mount sources must be translated to host paths
func TestDesiredContainerSpec_UsesVolumeHostPath(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath:         "/app/runners",
		ContainerMode:    true,
		ContainerImage:   "img:tag",
		ContainerNetwork: "runner-net",
		JobDockerBackend: "none",
		VolumeHostPath:   "/data/runners",
	}}
	spec := desiredContainerSpec(cfg, "a", "/app/runners/a", "")
	if spec.MountSrc != "/data/runners/a" {
		t.Fatalf("MountSrc = %q, want /data/runners/a", spec.MountSrc)
	}
	if spec.ContainerName != "github-runner-a" {
		t.Fatalf("ContainerName = %q", spec.ContainerName)
	}
	args, err := spec.createArgs()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/data/runners/a:/runner") {
		t.Fatalf("create parameters do not use host path: %s", joined)
	}
}

// TestDriftFromFacts_SkipsNonContainerMode In non-container mode there is no Runner container, so drift should not be discussed
func TestDriftFromFacts_SkipsNonContainerMode(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: "/app/runners"}}
	spec := hostSocketSpec()
	if got := driftFromFacts(t.Context(), cfg, "a", "/app/runners/a", "tok", factsFor(spec), resolveImageID); got != "" {
		t.Fatalf("drift must not be reported in non-container mode: %q", got)
	}
	if got := driftFromFacts(t.Context(), nil, "a", "/app/runners/a", "tok", factsFor(spec), resolveImageID); got != "" {
		t.Fatalf("drift must not be reported when config is empty: %q", got)
	}
}

// TestDriftReason_ImageProvidedDockerHostIsNotOurs
// A custom Runner image that sets ENV DOCKER_HOST in its Dockerfile will also show it in Config.Env
// from docker inspect. If that is mistaken for one we injected, backend=none is repeatedly judged as drift, and the container is deleted and recreated on every start.
// Labels are the right answer: containers with labels are always judged by their labels, and the image's own ENV can no longer interfere.
func TestDriftReason_ImageProvidedDockerHostIsNotOurs(t *testing.T) {
	spec := hostSocketSpec()
	spec.JobBackend = "none"

	withLabel := factsFor(spec)
	withLabel.Env = append(withLabel.Env, "DOCKER_HOST=tcp://my-own-dind:2375")
	if got := spec.driftReason(withLabel, "sha256:aaa"); got != "" {
		t.Fatalf("drift must not be affected by the image's built-in DOCKER_HOST when labels are present: %q", got)
	}

	// Values we actually injected must be recognized: previously host-socket, now configured as none
	wasHostSocket := legacyFactsFor(hostSocketSpec())
	if got := spec.driftReason(wasHostSocket, "sha256:aaa"); got == "" {
		t.Fatal("host-socket → none missed drift")
	}
}

// TestDriftReason_LegacyImageDockerHostSettlesAfterOneRebuild
// Old containers without labels cannot tell whether DOCKER_HOST was injected by us or came from the image (see looksInjectedDockerHost),
// so we choose to recreate once more rather than miss a drift. The cost of this must be bounded: after recreation the container carries labels,
// and from then on is compared by labels, so it must not turn into a recreation on every start.
func TestDriftReason_LegacyImageDockerHostSettlesAfterOneRebuild(t *testing.T) {
	spec := hostSocketSpec()
	spec.JobBackend = "none"

	legacy := legacyFactsFor(spec)
	legacy.Env = append(legacy.Env, "DOCKER_HOST=tcp://my-own-dind:2375")
	if got := spec.driftReason(legacy, "sha256:aaa"); got == "" {
		t.Fatal("a tcp://…:2375 on an old container should always be treated as possibly injected by us and reported as drift")
	}

	// After recreation: the same ENV is still there (none does not strip variables the image sets), but the labels are present
	rebuilt := factsFor(spec)
	rebuilt.Env = append(rebuilt.Env, "DOCKER_HOST=tcp://my-own-dind:2375")
	if got := spec.driftReason(rebuilt, "sha256:aaa"); got != "" {
		t.Fatalf("after one recreation it should settle down, otherwise it is repeated recreation: %q", got)
	}
}

// TestDriftReason_LegacyDindHostChangedThenNone
// An old container was created with dind and dind_host=old-dind; the config was later changed to none and dind_host was also replaced
// (dropping dind is a natural thing to change along with it). If only the current dind_host is checked, tcp://old-dind:2375 is treated as
// unrelated to us and the drift is missed, so the container keeps running with a DOCKER_HOST pointing to the old DinD, which is exactly what none is meant to cut off.
func TestDriftReason_LegacyDindHostChangedThenNone(t *testing.T) {
	old := hostSocketSpec()
	old.JobBackend = "dind"
	old.DindHost = "old-dind"
	legacy := legacyFactsFor(old) // DOCKER_HOST=tcp://old-dind:2375, no socket mount

	for _, nowHost := range []string{"new-dind", "runner-dind"} {
		now := hostSocketSpec()
		now.JobBackend = "none"
		now.DindHost = nowHost // Changed to another address / falls back to default after removal
		if got := now.driftReason(legacy, "sha256:aaa"); got == "" {
			t.Fatalf("dind_host=%s drift missed: container can still reach old-dind while config is none", nowHost)
		}
	}
}

// TestDriftReason_LabelWins When labels and config disagree, it is drift; there is no need to look at DOCKER_HOST
func TestDriftReason_LabelWins(t *testing.T) {
	spec := hostSocketSpec()
	facts := factsFor(spec) // The label records host-socket

	want := spec
	want.JobBackend = "dind"
	got := want.driftReason(facts, "sha256:aaa")
	if got != "job_docker_backend: host-socket → dind" {
		t.Fatalf("label comparison result is wrong: %q", got)
	}
}

// TestCreateArgs_RecordsBackendLabel Labels must be set at create time; otherwise later code can only infer the backend.
func TestCreateArgs_RecordsBackendLabel(t *testing.T) {
	for _, backend := range []string{"dind", "host-socket", "none"} {
		spec := hostSocketSpec()
		spec.JobBackend = backend
		args, err := spec.createArgs()
		if err != nil {
			t.Fatal(err)
		}
		want := labelJobBackend + "=" + backend
		if !containsString(args, want) {
			t.Fatalf("create args for backend=%s are missing label %s: %v", backend, want, args)
		}
	}
}

// TestDriftReason_MissingAgentToken
// Containers created before this feature have no AGENT_TOKEN, so the agent falls back to no auth. Any other container on the same network can control it.
// Such containers should be flagged as drifted and get the token added automatically on the next start.
func TestDriftReason_MissingAgentToken(t *testing.T) {
	spec := hostSocketSpec()
	spec.AgentToken = "tok-abc"
	facts := factsFor(spec) // factsFor does not inject AGENT_TOKEN, which is exactly what old containers look like

	if got := spec.driftReason(facts, "sha256:aaa"); got != "agent_token: (none) → set" {
		t.Fatalf("missing AGENT_TOKEN was not detected: %q", got)
	}

	// Once injected, the value is not compared, whether or not it matches. Token rotation is a runtime failure that probing surfaces; it should not delete the container.
	withToken := factsFor(spec)
	withToken.Env = append(withToken.Env, "AGENT_TOKEN=tok-old")
	if got := spec.driftReason(withToken, "sha256:aaa"); got != "" {
		t.Fatalf("container with an injected token should not be reported as drifted: %q", got)
	}

	// Make no judgment when no token is available (the file cannot be read)
	noToken := spec
	noToken.AgentToken = ""
	if got := noToken.driftReason(facts, "sha256:aaa"); got != "" {
		t.Fatalf("should not report drift when no token is available to inject: %q", got)
	}
}

// TestDriftReason_EmptyAgentTokenCountsAsMissing
// Having the variable is not the same as having a token. An ENV AGENT_TOKEN= line in the image makes the variable exist with an empty value,
// The agent side TrimSpaces it and treats it as unconfigured, then falls back to reading the mounted .agent_token. If that cannot be read (root-owned 0600,
// exactly the migration case this change fixes), auth is skipped entirely, and no 401 exposes it.
// If only key presence is checked, such containers would bypass recreation forever, and this fix would do nothing for them.
func TestDriftReason_EmptyAgentTokenCountsAsMissing(t *testing.T) {
	spec := hostSocketSpec()
	spec.AgentToken = "tok-abc"

	for _, v := range []string{"", "   ", "\t"} {
		facts := factsFor(spec)
		facts.Env = append(facts.Env, "AGENT_TOKEN="+v)
		if got := spec.driftReason(facts, "sha256:aaa"); got != "agent_token: (none) → set" {
			t.Fatalf("AGENT_TOKEN=%q (agent treats it as unconfigured) was not detected: %q", v, got)
		}
	}

	// Contrast: a non-empty value still only checks presence and does not compare whether it matches the current token
	ok := factsFor(spec)
	ok.Env = append(ok.Env, "AGENT_TOKEN=tok-old")
	if got := spec.driftReason(ok, "sha256:aaa"); got != "" {
		t.Fatalf("non-empty token should not be reported as drifted just because its value differs: %q", got)
	}
}
