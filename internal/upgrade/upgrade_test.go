package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractBinary(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
		err   string
	}{
		{name: "release layout", files: map[string]string{"willet_1.0.0_linux_amd64/willet": "BIN", "willet_1.0.0_linux_amd64/README.md": "doc"}, want: "BIN"},
		{name: "missing", files: map[string]string{"willet_1.0.0_linux_amd64/README.md": "doc"}, err: "no willet binary"},
		{name: "nested too deep", files: map[string]string{"a/b/willet": "BIN"}, err: "no willet binary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := ExtractBinary(writeTemp(t, tarball(t, c.files)), &out)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || out.String() != c.want {
				t.Fatalf("got %q, %v; want %q", out.String(), err, c.want)
			}
		})
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("v0.3.2", "linux", "arm64"); got != "willet_0.3.2_linux_arm64.tar.gz" {
		t.Fatal(got)
	}
}

// fakeGitHub serves a release whose tarball holds the given script as willet.
func fakeGitHub(t *testing.T, tag, script string) *httptest.Server {
	t.Helper()
	asset := AssetName(tag, "linux", "amd64")
	tgz := tarball(t, map[string]string{strings.TrimSuffix(asset, ".tar.gz") + "/willet": script})
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+Repo+"/releases/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q}]}`, tag, asset, srv.URL+"/dl/"+asset)
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func target(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "willet")
	if err := os.WriteFile(p, []byte("OLD"), 0o750); err != nil {
		t.Fatal(err)
	}
	return p
}

func opts(target, current string) Options {
	return Options{Current: current, Target: target, GOOS: "linux", GOARCH: "amd64", Log: io.Discard}
}

func passVerify(context.Context, *Client, string, []byte) error { return nil }

func TestRunInstalls(t *testing.T) {
	srv := fakeGitHub(t, "v1.1.0", "#!/bin/sh\necho 'willet version v1.1.0 (abc)'\n")
	tgt := target(t)
	var gotTag string
	var gotDigest []byte
	c := &Client{APIBase: srv.URL, Verify: func(_ context.Context, _ *Client, tag string, d []byte) error {
		gotTag, gotDigest = tag, d
		return nil
	}}
	res, err := c.Run(context.Background(), opts(tgt, "v1.0.0"))
	if err != nil || !res.Installed || res.Tag != "v1.1.0" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	b, _ := os.ReadFile(tgt)
	if !strings.Contains(string(b), "v1.1.0") {
		t.Fatalf("target not replaced: %q", b)
	}
	if fi, _ := os.Stat(tgt); fi.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %v, want the old binary's 0750", fi.Mode().Perm())
	}
	if gotTag != "v1.1.0" {
		t.Fatalf("verified tag %q", gotTag)
	}
	tgz := tarball(t, map[string]string{"willet_1.1.0_linux_amd64/willet": "#!/bin/sh\necho 'willet version v1.1.0 (abc)'\n"})
	if sum := sha256.Sum256(tgz); !bytes.Equal(sum[:], gotDigest) {
		t.Fatalf("verified digest %x does not match the tarball", gotDigest)
	}
	entries, _ := os.ReadDir(filepath.Dir(tgt))
	if len(entries) != 1 {
		t.Fatalf("leftover files beside target: %v", entries)
	}
}

func TestRunLeavesTargetOnFailure(t *testing.T) {
	cases := []struct {
		name   string
		script string
		verify func(context.Context, *Client, string, []byte) error
		err    string
	}{
		{name: "verification fails", script: "#!/bin/sh\necho v1.1.0\n", verify: func(context.Context, *Client, string, []byte) error { return fmt.Errorf("bad signature") }, err: "bad signature"},
		{name: "wrong version", script: "#!/bin/sh\necho v9.9.9\n", verify: passVerify, err: "want v1.1.0"},
		{name: "does not run", script: "not an executable", verify: passVerify, err: "does not run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := fakeGitHub(t, "v1.1.0", c.script)
			tgt := target(t)
			cl := &Client{APIBase: srv.URL, Verify: c.verify}
			res, err := cl.Run(context.Background(), opts(tgt, "v1.0.0"))
			if err == nil || !strings.Contains(err.Error(), c.err) || res.Installed {
				t.Fatalf("res = %+v, err = %v; want %q", res, err, c.err)
			}
			if b, _ := os.ReadFile(tgt); string(b) != "OLD" {
				t.Fatalf("target changed: %q", b)
			}
			if entries, _ := os.ReadDir(filepath.Dir(tgt)); len(entries) != 1 {
				t.Fatalf("leftover files beside target: %v", entries)
			}
		})
	}
}

func TestRunVersionChecks(t *testing.T) {
	srv := fakeGitHub(t, "v1.1.0", "#!/bin/sh\necho v1.1.0\n")
	cases := []struct {
		name      string
		o         func(*Options)
		installed bool
		err       string
	}{
		{name: "up to date", o: func(o *Options) { o.Current = "v1.1.0" }},
		{name: "newer than latest", o: func(o *Options) { o.Current = "v2.0.0" }},
		{name: "dev build", o: func(o *Options) { o.Current = "dev" }, err: "development build"},
		{name: "dev build forced", o: func(o *Options) { o.Current = "dev"; o.Force = true }, installed: true},
		{name: "explicit same tag", o: func(o *Options) { o.Current = "v1.1.0"; o.Tag = "v1.1.0" }, err: "not newer"},
		{name: "explicit same tag forced", o: func(o *Options) { o.Current = "v1.1.0"; o.Tag = "v1.1.0"; o.Force = true }, installed: true},
		{name: "bad tag", o: func(o *Options) { o.Tag = "latest" }, err: "not a version tag"},
		{name: "check only", o: func(o *Options) { o.Check = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt := target(t)
			o := opts(tgt, "v1.0.0")
			c.o(&o)
			cl := &Client{APIBase: srv.URL, Verify: passVerify}
			res, err := cl.Run(context.Background(), o)
			if (err != nil) != (c.err != "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
			if res.Installed != c.installed {
				t.Fatalf("installed = %v, want %v", res.Installed, c.installed)
			}
		})
	}
}

func TestRunUnwritableTargetDirFailsBeforeDownload(t *testing.T) {
	srv := fakeGitHub(t, "v1.1.0", "x")
	o := opts(filepath.Join(t.TempDir(), "missing", "willet"), "v1.0.0")
	_, err := (&Client{APIBase: srv.URL, Verify: passVerify}).Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "write access") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunMissingAsset(t *testing.T) {
	srv := fakeGitHub(t, "v1.1.0", "x")
	o := opts(target(t), "v1.0.0")
	o.GOARCH = "riscv64"
	_, err := (&Client{APIBase: srv.URL, Verify: passVerify}).Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "has no willet_1.1.0_linux_riscv64") {
		t.Fatalf("err = %v", err)
	}
}

// The fixtures are GitHub's real attestations for the v0.3.2 linux/amd64
// tarball, and the public-good Sigstore trusted root.
func fixtureClient(t *testing.T) (*Client, []byte) {
	t.Helper()
	atts, err := os.ReadFile("testdata/attestations.json")
	if err != nil {
		t.Fatal(err)
	}
	hexDigest, _ := os.ReadFile("testdata/digest.txt")
	digest, err := hex.DecodeString(strings.TrimSpace(string(hexDigest)))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Served for any digest, as if an attacker pointed at someone
		// else's genuine attestation.
		w.Write(atts)
	}))
	t.Cleanup(srv.Close)
	return &Client{APIBase: srv.URL, TrustedRoot: trusted}, digest
}

func TestVerifyAttestation(t *testing.T) {
	c, digest := fixtureClient(t)
	ctx := context.Background()
	if err := VerifyAttestation(ctx, c, "v0.3.2", digest); err != nil {
		t.Fatalf("genuine release rejected: %v", err)
	}
	t.Run("signed for another tag", func(t *testing.T) {
		if err := VerifyAttestation(ctx, c, "v0.3.1", digest); err == nil {
			t.Fatal("attestation for v0.3.2 accepted for v0.3.1")
		}
	})
	t.Run("different tarball", func(t *testing.T) {
		other := append([]byte(nil), digest...)
		other[0] ^= 1
		if err := VerifyAttestation(ctx, c, "v0.3.2", other); err == nil {
			t.Fatal("tarball with no attestation accepted")
		}
	})
}
