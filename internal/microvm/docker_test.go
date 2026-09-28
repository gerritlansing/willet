package microvm

import (
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

func TestDockerOptions(t *testing.T) {
	c := validConfig()
	if opts := (&Provisioner{cfg: c}).dockerOptions(); len(opts) != 0 {
		t.Fatal("Docker disk added although Docker is off")
	}

	c.Docker, c.DockerDiskMiB = true, 20480
	got := applyOptions((&Provisioner{cfg: c}).dockerOptions())
	m, ok := got.Volumes[dockerDataDir]
	if !ok {
		t.Fatalf("no mount at %s: %+v", dockerDataDir, got.Volumes)
	}
	if m.Owned != string(msb.VolumeKindDisk) || m.SizeMiB != 20480 {
		t.Fatalf("want an owned 20480 MiB ext4 disk, got %+v", m)
	}

	env := map[string]string{}
	(&Provisioner{cfg: c}).runnerDockerEnv(env)
	if env["RUNNER_WAIT_FOR_DOCKER_IN_SECONDS"] == "" {
		t.Fatal("runner not told to wait for Docker")
	}
}

func TestDockerConfigValidate(t *testing.T) {
	c := validConfig()
	c.Docker, c.DockerDiskMiB = true, 512
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a Docker disk under 1 GiB")
	}
	c.DockerDiskMiB = 20480
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Docker, c.DockerDiskMiB = false, 0
	if err := c.Validate(); err != nil {
		t.Fatalf("disk size must not matter with Docker off: %v", err)
	}
}

func TestParseRegistryMirror(t *testing.T) {
	for in, want := range map[string]string{
		"http://host.microsandbox.internal:5000":    "http://host.microsandbox.internal:5000",
		" http://host.microsandbox.internal:5000/ ": "http://host.microsandbox.internal:5000",
		"https://mirror.example.com":                "https://mirror.example.com",
		"http://10.0.5.10:5000":                     "http://10.0.5.10:5000",
	} {
		got, err := ParseRegistryMirror(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"host.microsandbox.internal:5000",   // no scheme
		"ftp://mirror.example.com",          // wrong scheme
		"http://",                           // no host
		"http://mirror.example.com/v2",      // path
		"http://user:pw@mirror.example.com", // credentials
		"http://mirror.example.com?x=1",     // query
		"http://mirror.example.com:0",       // bad port
		"http://mirror.example.com:99999",   // bad port
	} {
		if got, err := ParseRegistryMirror(in); err == nil {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
}

func TestMirrorHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       "",
		"http://host.microsandbox.internal:5000": "5000",
		"http://host.microsandbox.internal":      "80",
		"https://HOST.microsandbox.internal":     "443",
		"http://mirror.example.com:5000":         "",
		"http://10.0.5.10:5000":                  "",
	} {
		if got := mirrorHostPort(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestRegistryMirrorNeedsDocker(t *testing.T) {
	c := validConfig()
	c.DockerRegistryMirror = "http://host.microsandbox.internal:5000"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a registry mirror with Docker off")
	}
	c.Docker, c.DockerDiskMiB = true, 20480
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
