package microvm

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

func TestRepository(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/actions/actions-runner:latest": "ghcr.io/actions/actions-runner",
		"ghcr.io/actions/actions-runner":        "ghcr.io/actions/actions-runner",
		"localhost:5000/runner:2.337.0":         "localhost:5000/runner",
		"localhost:5000/runner":                 "localhost:5000/runner",
		"ubuntu:24.04":                          "ubuntu",
		"ubuntu":                                "ubuntu",
	}
	for in, want := range cases {
		if got := repository(in); got != want {
			t.Errorf("repository(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeImages simulates a registry tag that can be moved, and a local cache.
type fakeImages struct {
	tagDigest string
	pullErr   error
	probeErr  error
	inUse     map[string]bool
	cached    map[string]bool

	pulls   []string
	probes  []string
	removed []string
}

func (f *fakeImages) pull(_ context.Context, _, ref string) error {
	f.pulls = append(f.pulls, ref)
	return f.pullErr
}

func (f *fakeImages) digest(context.Context, string) (string, error) { return f.tagDigest, nil }

func (f *fakeImages) probe(_ context.Context, _, ref string) (string, error) {
	f.probes = append(f.probes, ref)
	if f.probeErr != nil {
		return "", f.probeErr
	}
	f.cached[ref] = true
	return "2.337.0", nil
}

func (f *fakeImages) remove(_ context.Context, ref string) error {
	if f.inUse[ref] {
		return &msb.Error{Kind: msb.ErrImageInUse}
	}
	if !f.cached[ref] {
		return &msb.Error{Kind: msb.ErrImageNotFound}
	}
	delete(f.cached, ref)
	f.removed = append(f.removed, ref)
	return nil
}

func newImageTest(image string) (*Provisioner, *fakeImages) {
	c := validConfig()
	c.Image = image
	f := &fakeImages{tagDigest: "sha256:aaa", inUse: map[string]bool{}, cached: map[string]bool{}}
	return &Provisioner{cfg: c, logger: slog.New(slog.DiscardHandler), rt: f}, f
}

func TestRefreshPinsTagToDigest(t *testing.T) {
	p, f := newImageTest("ghcr.io/actions/actions-runner:latest")
	if got := p.ImageRef(); got != "ghcr.io/actions/actions-runner:latest" {
		t.Fatalf("before any refresh, want the configured image, got %s", got)
	}

	res, err := p.Refresh(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/actions/actions-runner@sha256:aaa"
	if res.Ref != want || !res.Changed || res.RunnerVersion != "2.337.0" || p.ImageRef() != want {
		t.Fatalf("got %+v, ImageRef %s", res, p.ImageRef())
	}
	if !slices.Equal(f.pulls, []string{"ghcr.io/actions/actions-runner:latest"}) {
		t.Fatalf("pulls = %v, want the tag once", f.pulls)
	}
	if !slices.Equal(f.probes, []string{want}) {
		t.Fatalf("probes = %v, want the pinned digest (which also primes the cache)", f.probes)
	}

	// Same digest again: unchanged.
	res, err = p.Refresh(context.Background(), "x")
	if err != nil || res.Changed {
		t.Fatalf("second refresh: %+v %v", res, err)
	}
}

func TestRefreshDigestConfigIsUsedAsIs(t *testing.T) {
	pinned := "ghcr.io/actions/actions-runner@sha256:fixed"
	p, f := newImageTest(pinned)
	res, err := p.Refresh(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pulls) != 0 {
		t.Fatalf("a digest-pinned image must not be re-pulled: %v", f.pulls)
	}
	if res.Ref != pinned || p.ImageRef() != pinned {
		t.Fatalf("got %+v", res)
	}
}

func TestRefreshFailureKeepsPreviousImage(t *testing.T) {
	p, f := newImageTest("ghcr.io/actions/actions-runner:latest")
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	good := p.ImageRef()

	f.tagDigest = "sha256:bbb"
	f.pullErr = errors.New("registry down")
	if _, err := p.Refresh(context.Background(), "x"); err == nil {
		t.Fatal("pull failure reported as success")
	}
	if p.ImageRef() != good {
		t.Fatalf("image changed despite failed pull: %s", p.ImageRef())
	}

	f.pullErr = nil
	f.probeErr = errors.New("run.sh missing")
	if _, err := p.Refresh(context.Background(), "x"); err == nil {
		t.Fatal("probe failure reported as success")
	}
	if p.ImageRef() != good {
		t.Fatalf("image changed to one that failed its check: %s", p.ImageRef())
	}

	f.probeErr = nil
	f.tagDigest = ""
	if _, err := p.Refresh(context.Background(), "x"); err == nil {
		t.Fatal("missing digest reported as success")
	}
}

func TestRefreshPrunesSupersededImagesOnceUnused(t *testing.T) {
	p, f := newImageTest("ghcr.io/actions/actions-runner:latest")
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	old := p.ImageRef()
	f.inUse[old] = true // a running job VM still boots from it

	f.tagDigest = "sha256:bbb"
	res, err := p.Refresh(context.Background(), "x")
	if err != nil || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	if !f.cached[old] {
		t.Fatal("removed an image a VM is still using")
	}

	// The VM finished; the next refresh prunes the old image.
	delete(f.inUse, old)
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if f.cached[old] || !slices.Contains(f.removed, old) {
		t.Fatalf("superseded image not pruned: removed=%v", f.removed)
	}
	if !f.cached[p.ImageRef()] {
		t.Fatal("pruned the current image")
	}
}

// Found in review: A -> B -> A while A was still in use left A in the stale
// list, so the refresh that selected A again deleted it.
func TestRefreshRollbackKeepsCurrentImage(t *testing.T) {
	p, f := newImageTest("ghcr.io/actions/actions-runner:latest")
	f.tagDigest = "sha256:aaa"
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	a := p.ImageRef()
	f.inUse[a] = true

	f.tagDigest = "sha256:bbb"
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	b := p.ImageRef()

	delete(f.inUse, a)
	f.tagDigest = "sha256:aaa" // tag rolled back
	if _, err := p.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if p.ImageRef() != a {
		t.Fatalf("current image %s, want %s", p.ImageRef(), a)
	}
	if !f.cached[a] || slices.Contains(f.removed, a) {
		t.Fatalf("the current image was pruned: removed=%v", f.removed)
	}
	if f.cached[b] {
		t.Fatalf("superseded image %s not pruned", b)
	}
	if n := len(p.stale); n != 0 {
		t.Fatalf("stale list not empty: %v", p.stale)
	}
}

func TestStaleListHasNoDuplicates(t *testing.T) {
	p, f := newImageTest("ghcr.io/actions/actions-runner:latest")
	f.tagDigest = "sha256:aaa"
	_, _ = p.Refresh(context.Background(), "x")
	a := p.ImageRef()
	f.inUse[a] = true
	for _, d := range []string{"sha256:bbb", "sha256:aaa", "sha256:bbb"} {
		f.tagDigest = d
		if _, err := p.Refresh(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, s := range p.stale {
		if seen[s] {
			t.Fatalf("duplicate in stale list: %v", p.stale)
		}
		seen[s] = true
		if s == p.ImageRef() {
			t.Fatalf("current image in stale list: %v", p.stale)
		}
	}
}
