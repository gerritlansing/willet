//go:build integration

package microvm

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// fakeRunnerImage is alpine plus a stand-in runner in /opt/runner that
// reports the given version and, when started, idles like a real runner.
func fakeRunnerImage(t *testing.T, base v1.Image, version string) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, mode int64, body string) {
		typ := byte(tar.TypeReg)
		if strings.HasSuffix(name, "/") {
			typ = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: typ}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	add("opt/", 0o755, "")
	add("opt/runner/", 0o755, "")
	add("opt/runner/bin/", 0o755, "")
	add("opt/runner/run.sh", 0o755, "#!/bin/sh\nexec sleep 3600\n")
	add("opt/runner/bin/Runner.Listener", 0o755, "#!/bin/sh\necho "+version+"\n")
	add("opt/runner/version", 0o644, version+"\n")
	_ = tw.Close()

	b := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// F08: a tag that moves in the registry is picked up by Refresh, running VMs
// keep their image, superseded images are pruned once unused, and a failed
// refresh keeps the last good image.
func TestRefreshFollowsMovedTag(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: registry.New()}
	go srv.Serve(ln)
	defer srv.Close()

	tag := ln.Addr().String() + "/runner:latest"
	base, err := crane.Pull("alpine:3.20", crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	push := func(version string) string {
		t.Helper()
		img := fakeRunnerImage(t, base, version)
		if err := crane.Push(img, tag, crane.Insecure); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		return d.String()
	}

	p := newProvFor(t, "it00000000dd")
	p.cfg.Image = tag
	p.cfg.User = "nobody"
	p.cfg.RunnerDir = "/opt/runner"
	p.extra = []msb.SandboxOption{msb.WithRegistryInsecure()}
	readVersion := func(vm *VM) string {
		t.Helper()
		out, err := vm.sb.(*msb.Sandbox).Shell(t.Context(), "cat /opt/runner/version")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.Stdout())
	}

	d1 := push("1.0.0")
	res, err := p.Refresh(t.Context(), "willet-it-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != repository(tag)+"@"+d1 || res.RunnerVersion != "1.0.0" {
		t.Fatalf("first refresh: %+v, want digest %s", res, d1)
	}
	old, err := p.Start(t.Context(), "willet-it-v1", "jit")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Destroy(context.Background())

	// Move the tag. The refresh picks it up; the running VM is untouched.
	d2 := push("2.0.0")
	res, err = p.Refresh(t.Context(), "willet-it-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Ref != repository(tag)+"@"+d2 || res.RunnerVersion != "2.0.0" {
		t.Fatalf("after moving the tag: %+v, want digest %s", res, d2)
	}
	if v := readVersion(old); v != "1.0.0" {
		t.Fatalf("running VM now reports %s", v)
	}
	v1ref := repository(tag) + "@" + d1
	if _, err := msb.Image.Get(t.Context(), v1ref); err != nil {
		t.Fatalf("image of a running VM was pruned: %v", err)
	}
	fresh, err := p.Start(t.Context(), "willet-it-v2", "jit")
	if err != nil {
		t.Fatal(err)
	}
	if v := readVersion(fresh); v != "2.0.0" {
		t.Fatalf("new VM runs %s, want 2.0.0", v)
	}
	_ = fresh.Destroy(t.Context())

	// The VM on 1.0.0 finishes while 1.0.0 is still marked superseded, and
	// the tag is rolled back to it. 1.0.0 is current again and must stay
	// cached (review finding R02); 2.0.0, now unused, is pruned.
	if err := old.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d := push("1.0.0"); d != d1 {
		t.Fatalf("rebuilt 1.0.0 image has digest %s, want %s", d, d1)
	}
	res, err = p.Refresh(t.Context(), "willet-it-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != v1ref || !res.Changed {
		t.Fatalf("after rollback: %+v, want %s", res, v1ref)
	}
	if _, err := msb.Image.Get(t.Context(), v1ref); err != nil {
		t.Fatalf("image selected by the rollback was pruned: %v", err)
	}
	if _, err := msb.Image.Get(t.Context(), repository(tag)+"@"+d2); !msb.IsKind(err, msb.ErrImageNotFound) {
		t.Fatalf("superseded 2.0.0 image still cached (err %v)", err)
	}

	// Registry gone: refresh fails, and new VMs start from the cached
	// current image (1.0.0, restored by the rollback).
	_ = srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := p.Refresh(ctx, "willet-it-refresh"); err == nil {
		t.Fatal("refresh succeeded with the registry down")
	}
	if p.ImageRef() != res.Ref {
		t.Fatalf("image changed after a failed refresh: %s", p.ImageRef())
	}
	vm, err := p.Start(t.Context(), "willet-it-offline", "jit")
	if err != nil {
		t.Fatalf("new VM could not start from the cached image while the registry is down: %v", err)
	}
	if v := readVersion(vm); v != "1.0.0" {
		t.Fatalf("offline VM runs %s, want 1.0.0", v)
	}
	_ = vm.Destroy(t.Context())

	for _, ref := range []string{tag, res.Ref} {
		_ = msb.Image.Remove(context.Background(), ref, true)
	}
	if n := countOwnedBy(t, "it00000000dd"); n != 0 {
		t.Fatalf("%d sandboxes left", n)
	}
}

// A registry that requires HTTP Basic credentials, like a private GHCR package.
func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestPrivateRegistryCredentials(t *testing.T) {
	const user, pass = "bot", "s3cret-registry-token-7f3a9c"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: basicAuth(user, pass, registry.New())}
	go srv.Serve(ln)
	defer srv.Close()

	tag := ln.Addr().String() + "/private-runner:latest"
	base, err := crane.Pull("alpine:3.20", crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		t.Fatal(err)
	}
	creds := crane.WithAuth(&authn.Basic{Username: user, Password: pass})
	if err := crane.Push(fakeRunnerImage(t, base, "3.0.0"), tag, crane.Insecure, creds); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, img := range []string{tag} {
			_ = msb.Image.Remove(context.Background(), img, true)
		}
	}()

	newPrivateProv := func(u, p string) *Provisioner {
		prov := newProvFor(t, "it00000000ee")
		prov.cfg.Image = tag
		prov.cfg.User = "nobody"
		prov.cfg.RunnerDir = "/opt/runner"
		prov.cfg.RegistryUsername, prov.cfg.RegistryPassword = u, p
		prov.extra = []msb.SandboxOption{msb.WithRegistryInsecure()}
		return prov
	}

	if _, err := newPrivateProv("", "").Refresh(t.Context(), "willet-it-private"); err == nil {
		t.Fatal("pulled a private image without credentials")
	}
	if _, err := newPrivateProv(user, "wrong").Refresh(t.Context(), "willet-it-private"); err == nil {
		t.Fatal("pulled a private image with a wrong password")
	}

	p := newPrivateProv(user, pass)
	res, err := p.Refresh(t.Context(), "willet-it-private")
	if err != nil {
		t.Fatalf("refresh with credentials: %v", err)
	}
	defer msb.Image.Remove(context.Background(), res.Ref, true)
	if res.RunnerVersion != "3.0.0" {
		t.Fatalf("got %+v", res)
	}
	vm, err := p.Start(t.Context(), "willet-it-private-vm", "jit")
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Destroy(context.Background())

	// The password must not be written to microsandbox's catalog.
	home, _ := os.UserHomeDir()
	dbs, _ := filepath.Glob(filepath.Join(home, ".microsandbox", "db", "*"))
	if len(dbs) == 0 {
		t.Fatal("no microsandbox database files found to check")
	}
	sawImage := false
	for _, f := range dbs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(pass)) {
			t.Fatalf("registry password found in %s", f)
		}
		sawImage = sawImage || bytes.Contains(b, []byte("private-runner"))
	}
	if !sawImage {
		t.Fatal("the image reference isn't in the database files either, so this check proves nothing")
	}
	// Nor be visible inside the VM.
	out, err := vm.sb.(*msb.Sandbox).Shell(t.Context(), "grep -rs "+pass+" /etc /proc/1/environ /home /root /opt; env | grep -c "+pass+" || true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Stdout(), pass) || strings.TrimSpace(out.Stdout()) != "0" {
		t.Fatalf("registry password visible in the guest: %q", out.Stdout())
	}
}
