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
