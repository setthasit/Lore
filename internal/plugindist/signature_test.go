package plugindist

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minisignKey is an Ed25519 keypair in minisign's own file format, generated
// per test: a fixture key checked into the repository would be a signing key
// checked into the repository.
type minisignKey struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
	id      [8]byte
}

func newMinisignKey(t *testing.T) minisignKey {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an ed25519 key: %v", err)
	}

	key := minisignKey{public: public, private: private}
	if _, err := rand.Read(key.id[:]); err != nil {
		t.Fatalf("generate a key id: %v", err)
	}
	return key
}

func (k minisignKey) publicKeyFile(t *testing.T) string {
	t.Helper()

	payload := append(append([]byte(minisignLegacy), k.id[:]...), k.public...)
	body := "untrusted comment: minisign public key\n" + base64.StdEncoding.EncodeToString(payload) + "\n"

	path := filepath.Join(t.TempDir(), "signer.pub")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write a public key: %v", err)
	}
	return path
}

// sign renders a .minisig. algorithm is the two bytes the format leads with, so
// a test can publish the prehashed variant this build refuses.
func (k minisignKey) sign(t *testing.T, algorithm string, signed []byte) []byte {
	t.Helper()

	signature := ed25519.Sign(k.private, signed)
	payload := append(append([]byte(algorithm), k.id[:]...), signature...)

	const trusted = "timestamp:1756900000\tfile:checksums.txt"
	global := ed25519.Sign(k.private, append(append([]byte(nil), signature...), trusted...))

	return []byte("untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(payload) + "\n" +
		minisignTrustedP + " " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

func TestInstallVerifiesAMinisignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)
	scene.fake.attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		key.sign(t, minisignLegacy, scene.fake.asset("v0.3.1", ChecksumsAsset)))

	result, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: key.publicKeyFile(t),
	}, &Lock{})
	if err != nil {
		t.Fatalf("install a signed release: %v", err)
	}
	if !result.Signed {
		t.Fatal("a verified signature is not reported")
	}
}

// The signature is checked before any digest is compared, so a release whose
// checksums file is signed by nobody is refused for that and not for the digest
// it happens to disagree about.
func TestInstallRefusesABadSignatureBeforeComparingDigests(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)

	// The checksums file is rewritten after it was signed: both the signature
	// and the digest it records are now wrong.
	signature := key.sign(t, minisignLegacy, scene.fake.asset("v0.3.1", ChecksumsAsset))
	scene.fake.attach("v0.3.1", ChecksumsAsset+minisignSuffix, signature)
	scene.fake.attach("v0.3.1", ChecksumsAsset,
		[]byte(strings.Repeat("0", 64)+"  "+scene.asset+"\n"))

	_, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: key.publicKeyFile(t),
	}, &Lock{})
	if err == nil {
		t.Fatal("installing an unsigned checksums file succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "fails minisign verification") {
		t.Fatalf("error %q is not a signature refusal", err)
	}
	if strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error %q compared digests before verifying the signature", err)
	}
	if _, err := os.Stat(scene.store.Dir("linear", "v0.3.1")); !os.IsNotExist(err) {
		t.Fatal("a refused install left a cached version behind")
	}
}

func TestInstallRefusesASignatureFromAnotherKey(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	declared, attacker := newMinisignKey(t), newMinisignKey(t)
	scene.fake.attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		attacker.sign(t, minisignLegacy, scene.fake.asset("v0.3.1", ChecksumsAsset)))

	_, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: declared.publicKeyFile(t),
	}, &Lock{})
	if err == nil {
		t.Fatal("installing a release signed by another key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "another key") {
		t.Fatalf("error %q does not say the key is the wrong one", err)
	}
}

// A prehashed minisign signature hashes with BLAKE2b, which is not in the
// standard library. It is refused by name: a signature layer that silently does
// nothing is worse than none, because the user believes it is there.
func TestInstallRefusesAPrehashedMinisignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)
	scene.fake.attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		key.sign(t, minisignPrehash, scene.fake.asset("v0.3.1", ChecksumsAsset)))

	_, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: key.publicKeyFile(t),
	}, &Lock{})
	if err == nil {
		t.Fatal("installing a prehashed signature succeeded, want a refusal")
	}
	for _, want := range []string{"prehashed", "BLAKE2b", "refused rather than skipped"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// A declared signature that is not published is refused rather than accepted
// unsigned: opting in must not be able to fail open.
func TestInstallRefusesAMissingSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)

	_, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: key.publicKeyFile(t),
	}, &Lock{})
	if err == nil {
		t.Fatal("installing with no signature published succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "is not published beside it") {
		t.Fatalf("error %q does not say the signature is missing", err)
	}
}

func TestInstallVerifiesACosignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key, pubkeyPath := newCosignKey(t)
	checksums := scene.fake.asset("v0.3.1", ChecksumsAsset)
	scene.fake.attach("v0.3.1", ChecksumsAsset+cosignSuffix, cosignSign(t, key, checksums))

	result, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: pubkeyPath,
	}, &Lock{})
	if err != nil {
		t.Fatalf("install a cosign-signed release: %v", err)
	}
	if !result.Signed {
		t.Fatal("a verified signature is not reported")
	}

	// The same key over other bytes must not verify.
	scene.fake.attach("v0.3.1", ChecksumsAsset+cosignSuffix, cosignSign(t, key, []byte("something else")))
	if _, err := scene.installer.Install(context.Background(), Request{
		Coordinate: scene.coord, PubKey: pubkeyPath,
	}, &Lock{}); err == nil {
		t.Fatal("a signature over other bytes verified, want a refusal")
	} else if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error %q is not a signature refusal", err)
	}
}

func newCosignKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate an ecdsa key: %v", err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal the public key: %v", err)
	}

	path := filepath.Join(t.TempDir(), "cosign.pub")
	body := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the public key: %v", err)
	}
	return key, path
}

func cosignSign(t *testing.T, key *ecdsa.PrivateKey, signed []byte) []byte {
	t.Helper()

	hashed := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, hashed[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
}
