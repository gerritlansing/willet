//go:build integration

package microvm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// loopbackMirror serves a registry on the host's loopback, holding one image
// under a Docker Hub name, and records the paths requested after setup.
type loopbackMirror struct {
	port string
	mu   sync.Mutex
	reqs []string
}

func (m *loopbackMirror) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.reqs...)
}

func startLoopbackMirror(t *testing.T, repo string) *loopbackMirror {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &loopbackMirror{port: fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)}
	reg := registry.New()
	recording := false
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		if recording {
			m.reqs = append(m.reqs, r.Method+" "+r.URL.Path)
		}
		m.mu.Unlock()
		reg.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, "127.0.0.1:"+m.port+"/library/"+repo+":latest", crane.Insecure); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	recording = true
	m.mu.Unlock()
	return m
}

// mirrorVM boots a Docker-enabled VM from the project image with p's config
// and starts its Docker daemon.
func mirrorVM(t *testing.T, p *Provisioner, name string) *msb.Sandbox {
	t.Helper()
	sb, err := msb.CreateSandbox(t.Context(), name, append(p.sandboxOptions(name, p.cfg.Image), p.dockerOptions()...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	if err := p.startDocker(t.Context(), sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

// A VM pulls Docker Hub images through a mirror bound to the host's loopback,
// and reaches only that port on the host.
func TestDockerRegistryMirror(t *testing.T) {
	ref := serveImage(t, projectRunnerImage(t))
	mirror := startLoopbackMirror(t, "willetmirrortest")
	// Another service on the host's loopback, which VMs must not reach.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otherSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}
	go otherSrv.Serve(other)
	defer otherSrv.Close()

	p := dockerProv(t, "it0000000205", ref)
	p.cfg.DockerRegistryMirror = "http://host.microsandbox.internal:" + mirror.port
	sb := mirrorVM(t, p, "willet-it-mirror")

	if err := p.checkRegistryMirror(t.Context(), sb); err != nil {
		t.Fatalf("mirror check: %v", err)
	}
	script := fmt.Sprintf(`set -e
docker pull willetmirrortest
if curl -sf -m 5 -o /dev/null http://host.microsandbox.internal:%d/; then echo "other host port reachable"; exit 7; fi`, other.Addr().(*net.TCPAddr).Port)
	out, err := sb.Exec(t.Context(), "bash", []string{"-c", script}, msb.WithExecUser("runner"), msb.WithExecTimeout(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode() != 0 {
		t.Fatalf("exit %d:\n%s%s", out.ExitCode(), out.Stdout(), out.Stderr())
	}
	// Docker falls back to Docker Hub silently, so only the mirror's own log
	// proves the image came from it.
	blobs := 0
	for _, r := range mirror.requests() {
		if strings.HasPrefix(r, "GET /v2/library/willetmirrortest/blobs/") {
			blobs++
		}
	}
	if blobs == 0 {
		t.Fatalf("no blobs pulled through the mirror; requests: %v", mirror.requests())
	}
}

// An unreachable mirror is reported, but Docker still starts.
func TestDockerRegistryMirrorUnreachable(t *testing.T) {
	ref := serveImage(t, projectRunnerImage(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := dockerProv(t, "it0000000206", ref)
	p.cfg.DockerRegistryMirror = fmt.Sprintf("http://host.microsandbox.internal:%d", port)
	sb := mirrorVM(t, p, "willet-it-mirror-down")

	err = p.checkRegistryMirror(t.Context(), sb)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want an unreachable-mirror error, got %v", err)
	}
}
