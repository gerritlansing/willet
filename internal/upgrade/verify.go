package upgrade

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	oidcIssuer     = "https://token.actions.githubusercontent.com"
	provenanceType = "https://slsa.dev/provenance/v1"
)

// workflowIdentity is the certificate identity of release.yml running for
// the tag push that published tag.
func workflowIdentity(tag string) string {
	return "https://github.com/" + Repo + "/.github/workflows/release.yml@refs/tags/" + tag
}

// VerifyAttestation checks the build provenance GitHub stores for the tarball
// with this SHA-256 digest. It passes only if a Sigstore-signed attestation,
// logged in a transparency log, binds that digest to the release workflow of
// this repository running for tag. A checksum file from the release itself
// proves nothing, since anyone able to replace the tarball can replace it too.
func VerifyAttestation(ctx context.Context, c *Client, tag string, digest []byte) error {
	bundles, err := c.attestations(ctx, digest)
	if err != nil {
		return err
	}
	if len(bundles) == 0 {
		return errors.New("no build attestation published for this release")
	}

	trusted := c.TrustedRoot
	if trusted == nil {
		fetched, err := root.FetchTrustedRoot()
		if err != nil {
			return fmt.Errorf("fetch Sigstore trusted root: %w", err)
		}
		trusted = fetched
	}
	verifier, err := verify.NewVerifier(trusted,
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return err
	}
	id, err := verify.NewShortCertificateIdentity(oidcIssuer, "", workflowIdentity(tag), "")
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))

	var errs []error
	for _, b := range bundles {
		res, err := verifier.Verify(b, policy)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if res.Statement == nil || res.Statement.PredicateType != provenanceType {
			continue // e.g. the SBOM attestation
		}
		return nil
	}
	if len(errs) == 0 {
		return errors.New("no build provenance attestation published for this release")
	}
	return fmt.Errorf("attestation does not verify: %w", errors.Join(errs...))
}

// attestations fetches the attestation bundles GitHub holds for a digest.
func (c *Client) attestations(ctx context.Context, digest []byte) ([]*bundle.Bundle, error) {
	resp, err := c.get(ctx, c.api()+"/repos/"+Repo+"/attestations/sha256:"+hex.EncodeToString(digest), "application/json")
	if err != nil {
		return nil, fmt.Errorf("fetch attestations: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Attestations []struct {
			Bundle json.RawMessage `json:"bundle"`
		} `json:"attestations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode attestations: %w", err)
	}
	var out []*bundle.Bundle
	for _, a := range body.Attestations {
		b := new(bundle.Bundle)
		if err := b.UnmarshalJSON(a.Bundle); err != nil {
			return nil, fmt.Errorf("decode attestation bundle: %w", err)
		}
		out = append(out, b)
	}
	return out, nil
}
