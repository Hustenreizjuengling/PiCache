package update

// trustedKeys are the Ed25519 public keys (raw 32 bytes, base64) whose
// signature on SHA256SUMS makes a release installable
// (docs/ARCHITECTURE.md 14.1). The first key is published as
// docs/release-key.pem. Rotation: a release signed with the old key adds
// the new key here; later releases are signed with the new key only, and
// the old key is removed once no supported version needs it. Tests replace
// the list.
var trustedKeys = []string{
	"ZyR3aUYRK9l9vOA0WP7bTmE1C7LT3nI6IsFYEnqWHj0=", // docs/release-key.pem (September 2026)
}
