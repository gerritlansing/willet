package microvm

import (
	"testing"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

func applyOptions(opts []msb.SandboxOption) msb.SandboxConfig {
	var c msb.SandboxConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// sandboxOptions sets every VM's security-relevant properties; check them
// without needing KVM.
func TestSandboxOptions(t *testing.T) {
	cfg := validConfig()
	cfg.MaxDuration = 6 * time.Hour
	cfg.DiskMiB = 8192
	p := &Provisioner{cfg: cfg}

	c := applyOptions(p.sandboxOptions("vm-1", "repo@sha256:abc"))
	switch {
	case c.Image != "repo@sha256:abc":
		t.Errorf("image %q", c.Image)
	case !c.Ephemeral:
		t.Error("VMs must be ephemeral so no disk state survives a job")
	case c.Detached:
		t.Error("VMs must not be detached; they die with the daemon")
	case c.Labels[LabelOwner] != cfg.OwnerID:
		t.Errorf("owner label %q, want %q: orphan cleanup depends on it", c.Labels[LabelOwner], cfg.OwnerID)
	case c.Labels[LabelScaleSet] != cfg.ScaleSetName:
		t.Errorf("scale set label %q", c.Labels[LabelScaleSet])
	case c.Hostname != "vm-1":
		t.Errorf("hostname %q", c.Hostname)
	case c.CPUs != cfg.CPUs || c.MemoryMiB != cfg.MemoryMiB:
		t.Errorf("shape %d CPUs / %d MiB", c.CPUs, c.MemoryMiB)
	case c.MaxDuration != 6*time.Hour:
		t.Errorf("max duration %s", c.MaxDuration)
	case c.RootDisk == nil:
		t.Error("disk size not applied")
	case len(c.Env) != 0:
		t.Errorf("no environment may be set on the sandbox itself (it is persisted): %v", c.Env)
	case c.RegistryAuth != nil:
		t.Error("registry auth set without credentials")
	case c.Network != nil:
		t.Error("with no allow rules the runtime's default network policy must be left untouched")
	}

	cfg.MaxDuration, cfg.DiskMiB = 0, 0
	cfg.RegistryUsername, cfg.RegistryPassword = "bot", "pw"
	cfg.NetworkAllow, _ = ParseAllowRules([]string{"10.0.5.10"})
	c = applyOptions((&Provisioner{cfg: cfg}).sandboxOptions("vm-2", "img"))
	switch {
	case c.MaxDuration != 0:
		t.Error("zero max duration must mean no cap")
	case c.RootDisk != nil:
		t.Error("zero disk must leave the runtime default")
	case c.RegistryAuth == nil || c.RegistryAuth.Username != "bot" || c.RegistryAuth.Password != "pw":
		t.Errorf("registry auth %+v", c.RegistryAuth)
	case c.Network == nil:
		t.Error("allow rules not applied")
	}
}
