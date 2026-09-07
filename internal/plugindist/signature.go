package plugindist

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"hash"
	"os"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// A signature defends against a different attacker than the lockfile does: a
// compromised publisher account cutting a new release with internally valid
// checksums. It is therefore checked against the material the digests are read
// from, and checked before any digest is compared — a digest that came from a
// file nobody vouched for is only a checksum.
//
// Two formats are recognised, both verifiable with the standard library:
//
//   - cosign — a PEM public key, an ECDSA (P-256, P-384 or P-521) or Ed25519
//     signature over the signed file, base64 in a `.sig` sibling. This is what
//     `cosign sign-blob --key` produces and goreleaser publishes.
//   - minisign — a `.minisig` sibling, Ed25519 over the file itself.
//
// Prehashed minisign signatures — the `ED` algorithm — hash the file with
// BLAKE2b, which is not in the standard library, and this repository's
// dependencies are allowlisted. Such a signature is refused by name rather
// than skipped: a signature layer that silently does nothing is worse than no
// signature layer, because the user believes it is there.
const (
	cosignSuffix   = ".sig"
	minisignSuffix = ".minisig"

	minisignPublicKeySize = 42 // algorithm(2) + key id(8) + ed25519 public key(32)
	minisignSignatureSize = 74 // algorithm(2) + key id(8) + ed25519 signature(64)

	minisignLegacy   = "Ed" // Ed25519 over the file's own bytes
	minisignPrehash  = "ED" // Ed25519 over a BLAKE2b-512 hash of the file
	minisignCommentP = "untrusted comment:"
	minisignTrustedP = "trusted comment:"
)

// verifier is a loaded public key and the signature format that key implies.
type verifier struct {
	name   string // the plugin, so a refusal names the declaration to fix
	format string
	suffix string

	cosign cosignKey
	pub    ed25519.PublicKey
	keyID  [8]byte
}

type cosignKey struct {
	algorithm string
	verify    func(signed, signature []byte) bool
}

// cosign hashes with the digest the curve's size implies, so a P-384 signature
// checked against SHA-256 rejects a signature that is valid.
var cosignCurveHashes = map[elliptic.Curve]func() hash.Hash{
	elliptic.P256(): sha256.New,
	elliptic.P384(): sha512.New384,
	elliptic.P521(): sha512.New,
}

// loadVerifier reads the resolved `pubkey:` and decides the format from the
// file's own shape, so a user never declares which tool signed a release twice.
func loadVerifier(name, pubkeyPath string) (verifier, error) {
	raw, err := os.ReadFile(pubkeyPath)
	if err != nil {
		return verifier{}, internalerror.NewPreconditionError(Label(name)+" declares pubkey: "+pubkeyPath+
			", which cannot be read", err)
	}

	if block, _ := pem.Decode(raw); block != nil {
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return verifier{}, internalerror.NewPreconditionError(Label(name)+" declares pubkey: "+pubkeyPath+
				", which is PEM but holds no public key this build can read", err)
		}
		return loadCosignKey(name, pubkeyPath, key)
	}
	return loadMinisignKey(name, pubkeyPath, raw)
}

// An unusable key is refused here, before any signature is fetched from a
// remote host.
func loadCosignKey(name, pubkeyPath string, key crypto.PublicKey) (verifier, error) {
	refuse := func(detail string) error {
		return internalerror.NewPreconditionError(Label(name)+" declares pubkey: "+pubkeyPath+", "+detail, nil)
	}

	loaded := verifier{name: name, format: "cosign", suffix: cosignSuffix}
	switch key := key.(type) {
	case ed25519.PublicKey:
		loaded.cosign = cosignKey{algorithm: "Ed25519", verify: func(signed, signature []byte) bool {
			return ed25519.Verify(key, signed, signature)
		}}
	case *ecdsa.PublicKey:
		newHash, usable := cosignCurveHashes[key.Curve]
		if !usable {
			return verifier{}, refuse("an ECDSA key on curve " + key.Curve.Params().Name +
				", which this build cannot verify — publish a key on P-256 (the cosign default), P-384 or P-521")
		}
		loaded.cosign = cosignKey{algorithm: "ECDSA", verify: func(signed, signature []byte) bool {
			digest := newHash()
			digest.Write(signed)
			return ecdsa.VerifyASN1(key, digest.Sum(nil), signature)
		}}
	default:
		return verifier{}, refuse("a " + fmt.Sprintf("%T", key) + " public key, which this build cannot verify" +
			" with the standard library — publish an ECDSA (the cosign default) or Ed25519 key")
	}
	return loaded, nil
}

func loadMinisignKey(name, pubkeyPath string, raw []byte) (verifier, error) {
	refuse := func(detail string) error {
		return internalerror.NewPreconditionError(Label(name)+" declares pubkey: "+pubkeyPath+
			", which is neither a PEM public key nor a minisign public key: "+detail, nil)
	}

	line := payloadLine(raw, minisignCommentP)
	if line == "" {
		return verifier{}, refuse("no key line")
	}
	decoded, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return verifier{}, refuse("the key line is not base64")
	}
	if len(decoded) != minisignPublicKeySize {
		return verifier{}, refuse(fmt.Sprintf("the key is %d bytes, not %d", len(decoded), minisignPublicKeySize))
	}
	if algorithm := string(decoded[:2]); algorithm != minisignLegacy {
		return verifier{}, refuse("the key names algorithm " + algorithm + ", and only " + minisignLegacy + " is Ed25519")
	}

	loaded := verifier{
		name: name, format: "minisign", suffix: minisignSuffix,
		pub: ed25519.PublicKey(decoded[10:]),
	}
	copy(loaded.keyID[:], decoded[2:10])
	return loaded, nil
}

// signatureSuffix is the sibling file the signature is published as, which the
// installer appends to the URL of whatever it is about to trust.
func (v verifier) signatureSuffix() string {
	return v.suffix
}

// verify refuses unless the signature is valid for exactly this key. Every
// failure is a refusal: there is no path through this function that reports a
// problem and continues.
func (v verifier) verify(signedName string, signed, signature []byte) error {
	if v.format == "cosign" {
		return v.verifyCosign(signedName, signed, signature)
	}
	return v.verifyMinisign(signedName, signed, signature)
}

func (v verifier) verifyCosign(signedName string, signed, signature []byte) error {
	encoded := strings.Join(strings.Fields(string(signature)), "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return v.refuse(signedName, "the signature is not base64")
	}
	if !v.cosign.verify(signed, decoded) {
		return v.refuse(signedName, "the "+v.cosign.algorithm+" signature does not verify against the declared key")
	}
	return nil
}

func (v verifier) verifyMinisign(signedName string, signed, signature []byte) error {
	lines := contentLines(signature)
	payload := ""
	for _, line := range lines {
		if !strings.HasPrefix(line, minisignCommentP) && !strings.HasPrefix(line, minisignTrustedP) {
			payload = line
			break
		}
	}
	if payload == "" {
		return v.refuse(signedName, "the signature file holds no signature line")
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return v.refuse(signedName, "the signature line is not base64")
	}
	if len(decoded) != minisignSignatureSize {
		return v.refuse(signedName, fmt.Sprintf("the signature is %d bytes, not %d", len(decoded), minisignSignatureSize))
	}

	switch algorithm := string(decoded[:2]); algorithm {
	case minisignLegacy:
	case minisignPrehash:
		return internalerror.NewPreconditionError(Label(v.name)+": "+signedName+" carries a prehashed minisign"+
			" signature ("+minisignPrehash+"), which is Ed25519 over a BLAKE2b hash. BLAKE2b is not in the Go"+
			" standard library and this build adds no dependency for it, so the signature cannot be checked and"+
			" is refused rather than skipped — publish a non-prehashed minisign signature or a cosign one", nil)
	default:
		return v.refuse(signedName, "the signature names algorithm "+algorithm+", which is not Ed25519")
	}

	if string(decoded[2:10]) != string(v.keyID[:]) {
		return v.refuse(signedName, "the signature was made by another key than the declared one")
	}
	if !ed25519.Verify(v.pub, signed, decoded[10:]) {
		return v.refuse(signedName, "the signature does not verify against the declared key")
	}
	return v.verifyTrustedComment(signedName, lines, decoded[10:])
}

// The global signature covers the trusted comment, which is the only part of a
// minisign file the signer's key vouches for besides the file itself. Checking
// it costs one Ed25519 verification and closes the gap where an attacker keeps
// a valid signature and rewrites the comment around it.
func (v verifier) verifyTrustedComment(signedName string, lines []string, signature []byte) error {
	comment, global := "", ""
	for at, line := range lines {
		if trusted, isTrusted := strings.CutPrefix(line, minisignTrustedP); isTrusted {
			comment = strings.TrimPrefix(trusted, " ")
			if at+1 < len(lines) {
				global = lines[at+1]
			}
			break
		}
	}
	if comment == "" {
		return nil
	}
	if global == "" {
		return v.refuse(signedName, "the signature carries a trusted comment with no global signature over it")
	}

	decoded, err := base64.StdEncoding.DecodeString(global)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return v.refuse(signedName, "the global signature is not an Ed25519 signature")
	}
	if !ed25519.Verify(v.pub, append(append([]byte(nil), signature...), comment...), decoded) {
		return v.refuse(signedName, "the global signature over the trusted comment does not verify")
	}
	return nil
}

func (v verifier) refuse(signedName, detail string) error {
	return internalerror.NewPreconditionError(Label(v.name)+": "+signedName+" fails "+v.format+
		" verification — "+detail, nil)
}

// payloadLine returns the first line that is neither blank nor the comment the
// format puts above its payload.
func payloadLine(raw []byte, commentPrefix string) string {
	for _, line := range contentLines(raw) {
		if !strings.HasPrefix(line, commentPrefix) {
			return line
		}
	}
	return ""
}

func contentLines(raw []byte) []string {
	lines := make([]string, 0, 4)
	for line := range strings.Lines(string(raw)) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
