package plugindist

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/plugindist/plugindisttest"
)

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

	path := filepath.Join(t.TempDir(), "signer.pub")
	k.writePublicKey(t, path)
	return path
}

func (k minisignKey) writePublicKey(t *testing.T, path string) {
	t.Helper()

	payload := append(append([]byte(minisignLegacy), k.id[:]...), k.public...)
	body := "untrusted comment: minisign public key\n" + base64.StdEncoding.EncodeToString(payload) + "\n"

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create the key directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write a public key: %v", err)
	}
}

// sign renders a .minisig; algorithm is the two-byte tag the format leads with.
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

func (s *scene) requiring(pubkeyPath string) Coordinate {
	coord := s.coord
	coord.PubKey = pubkeyPath
	return coord
}

func TestInstallVerifiesAMinisignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)
	scene.fake.Attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		key.sign(t, minisignLegacy, scene.fake.Asset("v0.3.1", ChecksumsAsset)))

	result, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(key.publicKeyFile(t))}, &Lock{})
	if err != nil {
		t.Fatalf("install a signed release: %v", err)
	}
	if !result.Signed {
		t.Fatal("a verified signature is not reported")
	}
}

func TestInstallRefusesABadSignatureBeforeComparingDigests(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)

	signature := key.sign(t, minisignLegacy, scene.fake.Asset("v0.3.1", ChecksumsAsset))
	scene.fake.Attach("v0.3.1", ChecksumsAsset+minisignSuffix, signature)
	scene.fake.Attach("v0.3.1", ChecksumsAsset,
		[]byte(strings.Repeat("0", 64)+"  "+scene.asset+"\n"))

	_, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(key.publicKeyFile(t))}, &Lock{})
	if err == nil {
		t.Fatal("installing an unsigned checksums file succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "fails minisign verification") {
		t.Fatalf("error %q is not a signature refusal", err)
	}
	if strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error %q compared digests before verifying the signature", err)
	}
	if _, err := os.Stat(cacheDir(t, scene.store, "linear", "v0.3.1")); !os.IsNotExist(err) {
		t.Fatal("a refused install left a cached version behind")
	}
}

func TestInstallRefusesASignatureFromAnotherKey(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	declared, attacker := newMinisignKey(t), newMinisignKey(t)
	scene.fake.Attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		attacker.sign(t, minisignLegacy, scene.fake.Asset("v0.3.1", ChecksumsAsset)))

	_, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(declared.publicKeyFile(t))}, &Lock{})
	if err == nil {
		t.Fatal("installing a release signed by another key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "another key") {
		t.Fatalf("error %q does not say the key is the wrong one", err)
	}
}

// A prehashed minisign signature hashes with BLAKE2b, which the standard library has no implementation of.
func TestInstallRefusesAPrehashedMinisignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)
	scene.fake.Attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		key.sign(t, minisignPrehash, scene.fake.Asset("v0.3.1", ChecksumsAsset)))

	_, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(key.publicKeyFile(t))}, &Lock{})
	if err == nil {
		t.Fatal("installing a prehashed signature succeeded, want a refusal")
	}
	for _, want := range []string{"prehashed", "BLAKE2b", "refused rather than skipped"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestInstallRefusesAMissingSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key := newMinisignKey(t)

	_, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(key.publicKeyFile(t))}, &Lock{})
	if err == nil {
		t.Fatal("installing with no signature published succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "is not published beside it") {
		t.Fatalf("error %q does not say the signature is missing", err)
	}
}

func TestInstallVerifiesAgainstTheKeyBesideTheConfiguration(t *testing.T) {
	scene := newScene(t)
	signer, decoy := newMinisignKey(t), newMinisignKey(t)
	scene.fake.Attach("v0.3.1", ChecksumsAsset+minisignSuffix,
		signer.sign(t, minisignLegacy, scene.fake.Asset("v0.3.1", ChecksumsAsset)))

	const declared = "./keys/signer.pub"
	configDir, elsewhere := t.TempDir(), t.TempDir()
	signer.writePublicKey(t, filepath.Join(configDir, declared))
	decoy.writePublicKey(t, filepath.Join(elsewhere, declared))
	t.Chdir(elsewhere)

	coord, err := Resolve(configDir, config.PluginDecl{
		Name: scene.coord.Name, From: scene.coord.From, PubKey: declared,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	result, err := scene.installer.Install(context.Background(), Request{Coordinate: coord}, &Lock{})
	if err != nil {
		t.Fatalf("install a release signed by the key beside lore.yaml: %v", err)
	}
	if !result.Signed {
		t.Fatal("a verified signature is not reported")
	}
}

func TestInstallVerifiesACosignSignature(t *testing.T) {
	t.Parallel()

	scene := newScene(t)
	key, pubkeyPath := newCosignKey(t, elliptic.P256())
	checksums := scene.fake.Asset("v0.3.1", ChecksumsAsset)
	scene.fake.Attach("v0.3.1", ChecksumsAsset+cosignSuffix, cosignSign(t, key, checksums))

	result, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(pubkeyPath)}, &Lock{})
	if err != nil {
		t.Fatalf("install a cosign-signed release: %v", err)
	}
	if !result.Signed {
		t.Fatal("a verified signature is not reported")
	}

	scene.fake.Attach("v0.3.1", ChecksumsAsset+cosignSuffix, cosignSign(t, key, []byte("something else")))
	if _, err := scene.installer.Install(context.Background(),
		Request{Coordinate: scene.requiring(pubkeyPath)}, &Lock{}); err == nil {
		t.Fatal("a signature over other bytes verified, want a refusal")
	} else if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error %q is not a signature refusal", err)
	}
}

func newCosignKey(t *testing.T, curve elliptic.Curve) (*ecdsa.PrivateKey, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate an ecdsa key: %v", err)
	}
	return key, cosignPublicKeyFile(t, &key.PublicKey)
}

func cosignPublicKeyFile(t *testing.T, public crypto.PublicKey) string {
	t.Helper()

	encoded, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatalf("marshal the public key: %v", err)
	}

	path := filepath.Join(t.TempDir(), "cosign.pub")
	body := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the public key: %v", err)
	}
	return path
}

// The digest per curve is spelled out here rather than read from the code under test: it is cosign's choice.
func cosignSign(t *testing.T, key *ecdsa.PrivateKey, signed []byte) []byte {
	t.Helper()

	var hashed []byte
	switch curve := key.Curve.Params().Name; curve {
	case "P-256":
		sum := sha256.Sum256(signed)
		hashed = sum[:]
	case "P-384":
		sum := sha512.Sum384(signed)
		hashed = sum[:]
	case "P-521":
		sum := sha512.Sum512(signed)
		hashed = sum[:]
	default:
		t.Fatalf("no cosign digest for curve %s", curve)
	}

	signature, err := ecdsa.SignASN1(rand.Reader, key, hashed)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(signature) + "\n")
}

func TestInstallVerifiesACosignSignatureOnTheLargerCurves(t *testing.T) {
	t.Parallel()

	for _, curve := range []elliptic.Curve{elliptic.P384(), elliptic.P521()} {
		t.Run(curve.Params().Name, func(t *testing.T) {
			t.Parallel()

			scene := newScene(t)
			key, pubkeyPath := newCosignKey(t, curve)
			scene.fake.Attach("v0.3.1", ChecksumsAsset+cosignSuffix,
				cosignSign(t, key, scene.fake.Asset("v0.3.1", ChecksumsAsset)))

			result, err := scene.installer.Install(context.Background(),
				Request{Coordinate: scene.requiring(pubkeyPath)}, &Lock{})
			if err != nil {
				t.Fatalf("install a release signed on %s: %v", curve.Params().Name, err)
			}
			if !result.Signed {
				t.Fatal("a verified signature is not reported")
			}
		})
	}
}

func TestInstallRefusesAnUnusableKeyBeforeFetchingASignature(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate an rsa key: %v", err)
	}
	shortCurve, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a p-224 key: %v", err)
	}

	for _, test := range []struct {
		name   string
		public crypto.PublicKey
		want   string
	}{
		{
			name:   "an rsa key",
			public: &rsaKey.PublicKey,
			want:   "a *rsa.PublicKey public key, which this build cannot verify",
		},
		{
			name:   "an ecdsa key on a curve cosign does not sign with",
			public: &shortCurve.PublicKey,
			want:   "an ECDSA key on curve P-224, which this build cannot verify",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			served := serveArtifact(t, "acme-crm", "/lore/acme-crm/v2.0.1.tar.gz",
				plugindisttest.Archive(t, "acme-crm", []byte(stubBinary)))

			_, err := served.install(t, &Lock{}, cosignPublicKeyFile(t, test.public))
			if err == nil {
				t.Fatal("installing against an unusable key succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not name the key this build cannot use", err)
			}
			if asked := served.asked(cosignSuffix); len(asked) > 0 {
				t.Fatalf("an unusable key still fetched %v", asked)
			}
		})
	}
}

func TestVerifyTrustedCommentRefusesACommentNothingVouchesFor(t *testing.T) {
	t.Parallel()

	key := newMinisignKey(t)
	verify, err := loadVerifier("linear", key.publicKeyFile(t))
	if err != nil {
		t.Fatalf("load a minisign public key: %v", err)
	}

	const comment = "timestamp:1756900000\tfile:checksums.txt"
	signature := ed25519.Sign(key.private, []byte("checksums"))
	global := base64.StdEncoding.EncodeToString(
		ed25519.Sign(key.private, append(append([]byte(nil), signature...), comment...)))

	for _, test := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name:  "no global signature under the comment",
			lines: []string{minisignTrustedP + " " + comment},
			want:  "a trusted comment with no global signature over it",
		},
		{
			name:  "the global signature is not base64",
			lines: []string{minisignTrustedP + " " + comment, "!not base64!"},
			want:  "the global signature is not an Ed25519 signature",
		},
		{
			name:  "the global signature is the wrong length",
			lines: []string{minisignTrustedP + " " + comment, base64.StdEncoding.EncodeToString(signature[:32])},
			want:  "the global signature is not an Ed25519 signature",
		},
		{
			name:  "the comment was rewritten under a valid global signature",
			lines: []string{minisignTrustedP + " timestamp:1799999999\tfile:checksums.txt", global},
			want:  "the global signature over the trusted comment does not verify",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := verify.verifyTrustedComment(ChecksumsAsset, test.lines, signature)
			if err == nil {
				t.Fatal("an unvouched trusted comment was accepted, want a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not refuse with %q", err, test.want)
			}
		})
	}
}
