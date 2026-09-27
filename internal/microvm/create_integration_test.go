//go:build integration

package microvm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// stalledImage serves the fake runner image from a registry whose layer
// downloads block until release is called, so pulling it, and creating a VM
// from it, hangs on demand.
func stalledImage(t *testing.T) (ref string, release func()) {
	t.Helper()
	base, err := crane.Pull("alpine:3.20", crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)

	reg := registry.New()
	stalled := false
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		block := stalled && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/")
		mu.Unlock()
		if block {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		reg.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	ref = ln.Addr().String() + "/stalled-runner:latest"
	if err := crane.Push(fakeRunnerImage(t, base, "1.0.0"), ref, crane.Insecure); err != nil {
		t.Fatal(err)
	}
	removeCachedImagesFrom(t, ln.Addr().String())
	mu.Lock()
	stalled = true // pushes are done; from now on, pulls hang
	mu.Unlock()
	return ref, release
}

func stalledProv(t *testing.T, owner, ref string) *Provisioner {
	t.Helper()
	p := newProvFor(t, owner)
	p.cfg.Image = ref
	p.cfg.User = "nobody"
	p.cfg.RunnerDir = "/opt/runner"
	p.extra = []msb.SandboxOption{msb.WithRegistryInsecure()}
	return p
}

// V04 (seventh review): a stalled creation must not hold up the caller past
// its context; the creation becomes a pending VM that is cleaned up once it
// finishes.
func TestStartReturnsPromptlyWhileCreationStalls(t *testing.T) {
	const owner = "it0000000400"
	ref, release := stalledImage(t)
	p := stalledProv(t, owner, ref)
	name := "willet-it-stalled"

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	begin := time.Now()
	vm, err := p.Start(ctx, name, "jit")
	cancel()
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("Start waited %s for a stalled creation; want it to return when its context ends", d)
	}
	if err == nil {
		t.Fatal("Start succeeded although the image pull was stalled")
	}
	if vm == nil {
		t.Fatal("the in-flight creation was dropped instead of returned as a pending VM")
	}

	// While the creation is stuck, teardown can't finish, and says so.
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err = vm.Destroy(short)
	cancelShort()
	if !errors.Is(err, errStillCreating) {
		t.Fatalf("teardown during creation: %v; want errStillCreating", err)
	}

	release()
	long, cancelLong := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelLong()
	if err := vm.Destroy(long); err != nil {
		t.Fatalf("teardown after the creation finished: %v", err)
	}
	if _, err := msb.GetSandbox(context.Background(), name); !msb.IsKind(err, msb.ErrSandboxNotFound) {
		t.Fatalf("sandbox remains after teardown (lookup: %v)", err)
	}
	assertNoSandboxFiles(t, name)
	if n := countOwnedBy(t, owner); n != 0 {
		t.Fatalf("%d sandboxes left", n)
	}
}

// V04: the startup warm-up returns promptly on cancellation too, and the
// pending temporary VM is removed by Close, as at shutdown.
func TestWarmupReturnsPromptlyWhileCreationStalls(t *testing.T) {
	const owner = "it0000000401"
	ref, release := stalledImage(t)
	p := stalledProv(t, owner, ref)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	begin := time.Now()
	_, err := p.Refresh(ctx, "willet-it-stalled-warmup")
	cancel()
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("warm-up waited %s for a stalled creation", d)
	}
	if err == nil {
		t.Fatal("warm-up succeeded although the image pull was stalled")
	}
	if p.pendingAux == nil {
		t.Fatal("the in-flight temporary VM was dropped instead of kept as pending")
	}

	release()
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer ccancel()
	if err := p.Close(cctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := countOwnedBy(t, owner); n != 0 {
		t.Fatalf("%d sandboxes left after Close", n)
	}
}

// V02 (seventh review): when creation hits its own time limit, whatever it
// left must still be removed or kept as an obligation, never forgotten.
func TestCreateTimeoutKeepsOwnership(t *testing.T) {
	const owner = "it0000000402"
	p := newProvFor(t, owner)
	if _, err := p.Refresh(t.Context(), "willet-it-ct-refresh"); err != nil {
		t.Fatal(err) // caches the image, so creation below runs straight to boot
	}
	p.cfg.CreateTimeout = 100 * time.Millisecond

	name := "willet-it-createtimeout"
	vm, err := p.Start(t.Context(), name, "jit")
	if err == nil {
		t.Fatal("Start succeeded within a 100ms creation limit")
	}
	if vm != nil {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if derr := vm.Destroy(dctx); derr != nil {
			t.Fatalf("teardown of the kept VM: %v", derr)
		}
	}
	time.Sleep(time.Second)
	if h, gerr := msb.GetSandbox(context.Background(), name); !msb.IsKind(gerr, msb.ErrSandboxNotFound) {
		if gerr == nil {
			t.Fatalf("untracked sandbox remains after the creation limit: %s (%s)", name, h.Status())
		}
		t.Fatalf("lookup failed, so absence is unproven: %v", gerr)
	}
	t.Logf("creation limit: err=%v, kept VM=%v", err, vm != nil)
}
