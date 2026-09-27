// Package owner identifies which daemon owns a set of runner VMs and ensures
// only one daemon per identity runs on a host.
package owner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// ID returns a stable identity for a scale set: its registration scope,
// runner group and name. Two daemons with the same ID manage the same scale
// set; daemons with different IDs must never touch each other's VMs.
//
// Inputs are normalized generously (case, trailing slashes, default port).
// Treating two spellings as one identity is the safe direction: the worst
// outcome is that the second daemon refuses to start.
func ID(registrationURL, runnerGroup, scaleSetName string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(registrationURL))
	if err != nil {
		return "", fmt.Errorf("parse registration URL: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	scope := strings.ToLower(strings.Trim(u.Path, "/"))

	h := sha256.New()
	for _, part := range []string{host, scope, strings.ToLower(strings.TrimSpace(runnerGroup)), strings.ToLower(strings.TrimSpace(scaleSetName))} {
		// Length-prefix each part so boundaries can't be shifted between fields.
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
