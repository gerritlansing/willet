//go:build integration

package microvm

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// runnerBase is the image images/runner/Dockerfile builds on by default.
const runnerBase = "ghcr.io/actions/actions-runner:latest"

// projectRunnerImage builds images/runner/Dockerfile and returns the path of
// the resulting `docker save` archive. Docker on the host may not be usable,
// so it builds inside a microsandbox VM running docker:dind, with Docker's
// data on an ext4 disk.
//
// The base tag is resolved to a digest first and the build pins it, so the
// archive is cached by Dockerfile, base digest and platform: a new upstream
// base image means a new build, never a stale cached one.
func projectRunnerImage(t *testing.T) string {
	t.Helper()
	dockerfile, err := os.ReadFile("../../images/runner/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	platform := "linux/" + runtime.GOARCH
	digest, err := crane.Digest(runnerBase, crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatalf("resolve %s: %v", runnerBase, err)
	}
	base := strings.SplitN(runnerBase, ":", 2)[0] + "@" + digest
	t.Logf("project image base: %s (%s)", base, platform)

	h := sha256.New()
	h.Write(dockerfile)
	h.Write([]byte("\x00" + base + "\x00" + platform))
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(cacheDir, "willet-test", "runner-image-"+hex.EncodeToString(h.Sum(nil)[:8])+".tar")
	if _, err := os.Stat(archive); err == nil {
		return archive
	}
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Log("building the project runner image (cached afterwards)")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	sb, err := msb.CreateSandbox(ctx, "willet-it-imagebuild",
		msb.WithImage("docker:dind"), msb.WithCPUs(2), msb.WithMemory(2048),
		msb.WithLabel(LabelOwner, "it0000000200"), msb.WithEphemeral(true), msb.WithReplace(),
		msb.WithMounts(map[string]msb.MountConfig{"/var/lib/docker": msb.Mount.Owned(msb.OwnedVolumeOptions{Kind: msb.VolumeKindDisk, SizeMiB: 20480})}))
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Destroy(context.Background())

	run := func(script string) {
		t.Helper()
		out, err := sb.Exec(ctx, "sh", []string{"-c", script}, msb.WithExecUser("root"), msb.WithExecTimeout(15*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if out.ExitCode() != 0 {
			t.Fatalf("%s\n%s%s", script, out.Stdout(), out.Stderr())
		}
	}
	if err := sb.FS().Mkdir(ctx, "/build"); err != nil {
		t.Fatal(err)
	}
	if err := sb.FS().Write(ctx, "/build/Dockerfile", dockerfile); err != nil {
		t.Fatal(err)
	}
	run(`nohup dockerd >/var/log/dockerd.log 2>&1 </dev/null & timeout 60 sh -c 'until docker info >/dev/null 2>&1; do sleep 0.5; done'`)
	run(`docker build -q --pull --platform ` + platform + ` --build-arg BASE=` + base + ` -t willet-runner:test /build && docker save -o /var/lib/docker/image.tar willet-runner:test`)
	if err := sb.FS().CopyToHost(ctx, "/var/lib/docker/image.tar", archive+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(archive+".tmp", archive); err != nil {
		t.Fatal(err)
	}
	return archive
}

// serveImage pushes a docker-save archive to an in-memory registry and returns
// its tag reference.
func serveImage(t *testing.T, archive string) string {
	t.Helper()
	img, err := tarball.ImageFromPath(archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	return serveV1(t, img, "willet-runner")
}

// serveV1 pushes img to an in-memory registry and returns its tag reference.
func serveV1(t *testing.T, img v1.Image, repo string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: registry.New()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ref := ln.Addr().String() + "/" + repo + ":latest"
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatal(err)
	}
	removeCachedImagesFrom(t, ln.Addr().String())
	return ref
}

// removeCachedImagesFrom removes, at test cleanup, every image cached from
// the given test registry: the pushed tag and the digest references that
// Refresh pins, which would otherwise accumulate across runs.
func removeCachedImagesFrom(t *testing.T, registryHost string) {
	t.Helper()
	t.Cleanup(func() {
		imgs, err := msb.Image.List(context.Background())
		if err != nil {
			t.Logf("list cached images: %v", err)
			return
		}
		for _, img := range imgs {
			if strings.HasPrefix(img.Reference(), registryHost+"/") {
				_ = msb.Image.Remove(context.Background(), img.Reference(), true)
			}
		}
	})
}

// brokenDockerImage serves a fixture whose dockerd exits at once, logging
// that iptables is missing, the way the real daemon does without it. Tests
// of Docker start failures use it instead of depending on what an upstream
// image happens to lack. The runner layout is the fake one under /opt/runner.
func brokenDockerImage(t *testing.T) string {
	t.Helper()
	base, err := crane.Pull("alpine:3.20", crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatal(err)
	}
	img := fakeRunnerImage(t, base, "9.9.9")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{
		{"usr/", "", 0o755},
		{"usr/local/", "", 0o755},
		{"usr/local/bin/", "", 0o755},
		{"usr/local/bin/dockerd", "#!/bin/sh\necho 'failed to create NAT chain DOCKER: iptables not found' >&2\nexit 1\n", 0o755},
	} {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(f.name, "/") {
			typ = tar.TypeDir
		}
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: typ})
		_, _ = tw.Write([]byte(f.body))
	}
	_ = tw.Close()
	b := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	if img, err = mutate.AppendLayers(img, layer); err != nil {
		t.Fatal(err)
	}
	return serveV1(t, img, "broken-docker")
}

func brokenDockerProv(t *testing.T, owner string) *Provisioner {
	t.Helper()
	p := dockerProv(t, owner, brokenDockerImage(t))
	p.cfg.User = "nobody"
	p.cfg.RunnerDir = "/opt/runner"
	return p
}

// sandboxDir is where microsandbox keeps a sandbox's files on the host,
// including its owned Docker disk: under $MSB_HOME if set, else ~/.microsandbox.
func sandboxDir(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("MSB_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(home, ".microsandbox")
	}
	return filepath.Join(root, "sandboxes", name)
}

func assertNoSandboxFiles(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Stat(sandboxDir(t, name)); !os.IsNotExist(err) {
		t.Fatalf("files of sandbox %s, including its Docker disk, remain on the host (stat: %v)", name, err)
	}
}

func dockerProv(t *testing.T, owner, image string) *Provisioner {
	t.Helper()
	p := newProvFor(t, owner)
	p.cfg.Image = image
	p.cfg.MemoryMiB = 2048
	p.cfg.Docker, p.cfg.DockerDiskMiB = true, 20480
	p.extra = []msb.SandboxOption{msb.WithRegistryInsecure()}
	return p
}

// A daemon that exits must be reported at once, not after the full readiness
// wait. Only startDocker is timed; image pulls and VM boot are excluded.
func TestDockerStartFailsFast(t *testing.T) {
	p := brokenDockerProv(t, "it0000000201")
	name := "willet-it-dockerfast"
	sb, err := msb.CreateSandbox(t.Context(), name, append(p.sandboxOptions(name, p.cfg.Image), p.dockerOptions()...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Destroy(context.Background())

	begin := time.Now()
	err = p.startDocker(t.Context(), sb)
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("startDocker succeeded with a dockerd that exits")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("a daemon that exits at once took %s to report; want well under the %s readiness wait", elapsed, dockerReady)
	}
	if !strings.Contains(err.Error(), "iptables") {
		t.Fatalf("error should carry the iptables hint: %v", err)
	}
	t.Logf("reported in %s: %v", elapsed, err)
}

// The warm-up reports a Docker start failure with its hint.
func TestDockerWarmupReportsStartFailure(t *testing.T) {
	p := brokenDockerProv(t, "it0000000204")
	_, err := p.Refresh(t.Context(), "willet-it-docker")
	if err == nil {
		t.Fatal("warm-up passed although Docker could not start")
	}
	if !strings.Contains(err.Error(), "iptables") {
		t.Fatalf("error should carry the iptables hint: %v", err)
	}
	if n := countOwnedBy(t, "it0000000204"); n != 0 {
		t.Fatalf("%d VMs left after the warm-up", n)
	}
}

func TestDockerStartFailureRollsBack(t *testing.T) {
	p := brokenDockerProv(t, "it0000000202")
	if _, err := p.Refresh(t.Context(), "willet-it-docker"); err == nil {
		t.Fatal("warm-up unexpectedly passed")
	}
	name := "willet-it-dockerfail"
	vm, err := p.Start(t.Context(), name, "jit")
	if err == nil {
		_ = vm.Destroy(context.Background())
		t.Fatal("runner started although Docker could not")
	}
	if vm != nil {
		t.Fatalf("VM returned although it was destroyed: %v", err)
	}
	if n := countOwnedBy(t, "it0000000202"); n != 0 {
		t.Fatalf("%d VMs left after a failed Docker start", n)
	}
	assertNoSandboxFiles(t, name)
}

func TestDockerInRunnerVM(t *testing.T) {
	ref := serveImage(t, projectRunnerImage(t))
	p := dockerProv(t, "it0000000203", ref)

	res, err := p.Refresh(t.Context(), "willet-it-docker")
	if err != nil {
		t.Fatalf("warm-up with Docker: %v", err)
	}
	t.Logf("project image %s, runner %s", res.Ref, res.RunnerVersion)

	// This uses the real run.sh rather than a patched stand-in: patching a file
	// under /home/runner resets that directory to root:root 755, so the runner
	// user can't write there (superradcompany/microsandbox#1686).
	//
	// Capture the runner's output: with RUNNER_WAIT_FOR_DOCKER_IN_SECONDS set,
	// its run-helper.sh prints "Docker is ready." once `docker ps` works. The
	// dummy JIT config then makes the runner exit, but the VM stays up.
	var logs syncBuffer
	p.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p.cfg.RunnerOutput = true
	vm, err := p.Start(t.Context(), "willet-it-dockervm", "jit")
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Destroy(context.Background())
	sb := vm.sb.(*msb.Sandbox)
	select {
	case <-vm.Done():
	case <-time.After(2 * time.Minute):
		t.Fatal("runner did not exit")
	}
	if !strings.Contains(logs.String(), "Docker is ready.") {
		t.Fatalf("the runner did not see Docker ready before starting:\n%s", logs.String())
	}

	sh := func(user, script string) string {
		t.Helper()
		out, err := sb.Exec(t.Context(), "bash", []string{"-c", script}, msb.WithExecUser(user), msb.WithExecTimeout(5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if out.ExitCode() != 0 {
			t.Fatalf("%s: exit %d\n%s%s", script, out.ExitCode(), out.Stdout(), out.Stderr())
		}
		return strings.TrimSpace(out.Stdout())
	}

	if fs := sh("root", "findmnt -no FSTYPE /var/lib/docker"); fs != "ext4" {
		t.Fatalf("Docker data on %q, want ext4", fs)
	}
	if got := sh("runner", "docker run --rm alpine:3.20 wget -qO- https://api.github.com/zen"); got == "" {
		t.Fatal("container could not reach the internet")
	}
	got := sh("runner", "mkdir -p /home/runner/_work/ws && echo in > /home/runner/_work/ws/f && docker run --rm -v /home/runner/_work/ws:/__w alpine:3.20 sh -c 'cat /__w/f; echo out > /__w/g' && cat /home/runner/_work/ws/g")
	if got != "in\nout" {
		t.Fatalf("workspace bind mount: %q", got)
	}

	// Make sure the cleanup check below looks where the files really are.
	if _, err := os.Stat(sandboxDir(t, "willet-it-dockervm")); err != nil {
		t.Fatalf("sandbox files not found where expected, so the cleanup check would prove nothing: %v", err)
	}
	if err := vm.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countOwnedBy(t, "it0000000203"); n != 0 {
		t.Fatalf("%d VMs left", n)
	}
	assertNoSandboxFiles(t, "willet-it-dockervm")
}

// syncBuffer collects log output from concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
