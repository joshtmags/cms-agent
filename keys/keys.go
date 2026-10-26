// Package keys holds the update-signing public key, compiled into the agent.
//
// The key is embedded rather than read from disk so the trust anchor ships
// inside the signed binary itself: a new key can only arrive in a release
// signed with the old one.
package keys

import (
	_ "embed"

	"github.com/joshtmags/cms-agent/internal/minisign"
)

//go:embed update.pub
var updatePub []byte

// localTestKey replaces the embedded key in local test builds only
// (scripts/local-test.sh sets it with -ldflags -X). Release builds never set
// it: build-release.sh only passes it when CMS_AGENT_LOCAL_TEST_KEY is set,
// which CI doesn't do.
var localTestKey string

// UsingLocalTestKey reports whether this binary trusts a local test key.
func UsingLocalTestKey() bool { return localTestKey != "" }

// UpdateKey returns the public key that update bundles must be signed with.
// It fails while keys/update.pub is still the placeholder, so an agent built
// without a real key refuses every update.
func UpdateKey() (minisign.PublicKey, error) {
	if localTestKey != "" {
		return minisign.ParsePublicKey([]byte(localTestKey))
	}
	return minisign.ParsePublicKey(updatePub)
}
