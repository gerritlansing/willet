// Package upgrade finds, downloads, verifies and installs willet releases.
package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
)

const (
	// Repo is the GitHub repository releases come from.
	Repo = "gerritlansing/willet"

	apiBase = "https://api.github.com"

	// maxDownload and maxBinary bound a release tarball and the binary in it.
	maxDownload = 256 << 20
	maxBinary   = 512 << 20
)

// Release is a published GitHub release.
type Release struct {
	Tag    string
	Assets map[string]string // asset name -> download URL
}

// Client talks to the GitHub API.
type Client struct {
	HTTP *http.Client
	// APIBase overrides https://api.github.com in tests.
	APIBase string
	// Verify checks that the tarball with this SHA-256 digest was built and
	// signed by the release workflow for tag. Defaults to VerifyAttestation.
	Verify func(ctx context.Context, c *Client, tag string, digest []byte) error
	// TrustedRoot overrides the Sigstore trusted root VerifyAttestation
	// fetches over TUF. For tests.
	TrustedRoot root.TrustedMaterial
}

func (c *Client) api() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return apiBase
}

func (c *Client) get(ctx context.Context, u string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "willet-upgrade")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp, nil
}

// Release returns the release tagged tag, or the latest release if tag is
// empty.
func (c *Client) Release(ctx context.Context, tag string) (Release, error) {
	u := c.api() + "/repos/" + Repo + "/releases/latest"
	if tag != "" {
		u = c.api() + "/repos/" + Repo + "/releases/tags/" + url.PathEscape(tag)
	}
	resp, err := c.get(ctx, u, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	var body struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	if body.Tag == "" {
		return Release{}, errors.New("release has no tag")
	}
	r := Release{Tag: body.Tag, Assets: map[string]string{}}
	for _, a := range body.Assets {
		r.Assets[a.Name] = a.URL
	}
	return r, nil
}

// AssetName is the tarball name release.yml gives a build for tag.
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("willet_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// Download saves url to a new file in dir and returns its path and SHA-256.
func (c *Client) Download(ctx context.Context, u, dir string) (string, []byte, error) {
	resp, err := c.get(ctx, u, "application/octet-stream")
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir, "willet-download-*")
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if err == nil && n > maxDownload {
		err = fmt.Errorf("download exceeds %d bytes", maxDownload)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), h.Sum(nil), nil
}

// ExtractBinary copies the willet executable out of a release tarball, which
// holds willet_<version>_<os>_<arch>/willet, into w.
func ExtractBinary(tarball string, w io.Writer) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("release archive has no willet binary")
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "willet" || path.Dir(path.Dir(h.Name)) != "." {
			continue
		}
		if h.Size > maxBinary {
			return fmt.Errorf("willet binary is %d bytes, over the %d limit", h.Size, maxBinary)
		}
		_, err = io.Copy(w, io.LimitReader(tr, maxBinary))
		return err
	}
}
