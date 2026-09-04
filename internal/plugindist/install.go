package plugindist

import (
	"context"
	"net/http"
	"time"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// downloadTimeout bounds a whole artifact fetch. A plugin binary is tens of
// megabytes at most, and a stalled supply chain must fail rather than hang the
// command that a person is watching.
const downloadTimeout = 5 * time.Minute

// Installer resolves, downloads, verifies and unpacks plugin binaries. It is
// only ever driven by a person running `lore plugin install|update`: nothing
// inside a sync round fetches a plugin, because a background scheduler must not
// download and execute code on a timer.
type Installer struct {
	Store   *Store
	HTTP    *http.Client
	APIBase string
}

func NewInstaller(store *Store) *Installer {
	return &Installer{Store: store, HTTP: &http.Client{Timeout: downloadTimeout}, APIBase: DefaultAPIBase()}
}

// Request is one plugin to install. PubKey is the declaration's `pubkey:`, and
// Rewrite is what separates `update` from `install`: update is the only command
// allowed to replace a locked version, URL or digest.
type Request struct {
	Coordinate Coordinate
	PubKey     string
	Rewrite    bool
}

// Result is what an install did, in the terms the CLI reports and the caller
// needs to hand the binary to the protocol layer for its manifest handshake.
type Result struct {
	Name     string
	Origin   Origin
	Platform string
	Version  string
	From     string
	Binary   string

	ArtifactURL    string
	ArtifactDigest string
	BinaryDigest   string

	Signed  bool // a declared pubkey verified the material the digest came from
	Locked  bool // the digest was already pinned, and this install matched it
	Pinned  bool // this install wrote the pin, so the lockfile changed
	Trust   bool // nothing vouched for the artifact but the artifact itself
	Warning string
}

func (ins *Installer) fetcher() fetcher {
	client := ins.HTTP
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}
	base := ins.APIBase
	if base == "" {
		base = DefaultAPIBase()
	}
	return fetcher{client: client, apiBase: base}
}

// Pin turns @latest into the version it resolves to now. It is separate from
// Install because the concrete version has to go back into lore.yaml: a
// floating version is legal as an argument and illegal in a file.
func (ins *Installer) Pin(ctx context.Context, coord Coordinate) (Coordinate, error) {
	if !coord.Floating() {
		return coord, nil
	}

	latest, err := ins.fetcher().latestRelease(ctx, coord)
	if err != nil {
		return Coordinate{}, err
	}
	if latest.TagName == "" {
		return Coordinate{}, internalerror.NewPreconditionError(label(coord.Name)+" cannot resolve "+coord.From+
			": github.com/"+coord.Owner+"/"+coord.Repo+" publishes no release", nil)
	}
	return coord.AtVersion(latest.TagName)
}

// Install fetches, verifies and unpacks one plugin, and records what it pinned
// in lock. It never writes the lockfile: the caller saves it once every
// requested install has succeeded, so an abort leaves the committed file alone.
func (ins *Installer) Install(ctx context.Context, req Request, lock *Lock) (Result, error) {
	coord := req.Coordinate
	if coord.Floating() {
		return Result{}, internalerror.NewInternalError(label(coord.Name)+" reached install still pinned at @"+
			LatestVersion, nil)
	}

	result := Result{
		Name: coord.Name, Origin: coord.Origin, Platform: ins.Store.platform.Key(),
		Version: coord.Version, From: coord.From, Warning: coord.Warning(),
	}

	if coord.Origin == OriginLocal {
		// A local plugin is executed in place: there is nothing to download, and
		// by construction nothing to lock. That is the whole cost of the
		// development escape hatch, and Warning says so.
		report, err := ins.Store.Locate(coord.Name, coord, lock)
		if err != nil {
			return Result{}, err
		}
		result.Version, result.Binary = report.Version, report.Binary
		return result, nil
	}

	platform := ins.Store.platform
	entry, hasEntry := lock.Entry(coord.Name)
	if hasEntry && !req.Rewrite && entry.Version != coord.Version {
		return Result{}, internalerror.NewPreconditionError(label(coord.Name)+" is locked at "+entry.Version+
			" but "+coord.From+" asks for "+coord.Version+" — run: lore plugin update "+coord.Name, nil)
	}
	locked, hasLocked := lock.Artifact(coord.Name, platform)
	pinned := hasLocked && !req.Rewrite

	artifactURL, checksumsURL, err := ins.locate(ctx, coord, platform, locked, pinned, req.PubKey != "")
	if err != nil {
		return Result{}, err
	}
	result.ArtifactURL = artifactURL

	fetch := ins.fetcher()
	artifact, err := fetch.get(ctx, artifactURL, maxArtifactBytes)
	if err != nil {
		return Result{}, resolveFailure(coord, "downloading "+artifactURL, err)
	}

	fileName := artifactFileName(artifactURL)
	expected, signed, err := ins.expected(ctx, req, coord, fileName, artifact, checksumsURL)
	if err != nil {
		return Result{}, err
	}
	result.Signed = signed

	digest := digestOf(artifact)
	if expected != "" && expected != digest {
		return Result{}, internalerror.NewPreconditionError(label(coord.Name)+": digest mismatch for "+
			platform.Key()+" (expected "+expected+", got "+digest+")", nil)
	}
	if pinned && locked.Digest != digest {
		return Result{}, internalerror.NewPreconditionError(label(coord.Name)+": digest mismatch for "+
			platform.Key()+" (expected "+locked.Digest+", got "+digest+")", nil)
	}
	result.ArtifactDigest, result.Locked = digest, pinned
	result.Trust = expected == "" && !pinned

	binaryName, body, err := unpack(coord, platform, fileName, artifact)
	if err != nil {
		return Result{}, err
	}
	path, binaryDigest, err := ins.Store.write(coord.Name, coord.Version, binaryName, body)
	if err != nil {
		return Result{}, err
	}
	result.Binary, result.BinaryDigest = path, binaryDigest

	// install writes only entries that do not exist yet; update is the one
	// command that replaces a locked digest.
	if !pinned {
		lock.Set(coord.Name, coord.Version, coord.From, platform, LockArtifact{URL: artifactURL, Digest: digest})
		result.Pinned = true
	}
	return result, nil
}

// locate reports the artifact URL and, when one is needed, the checksums URL. A
// pinned platform is fetched from the URL the lockfile recorded and the release
// is not consulted at all: the lockfile is the pin, and re-resolving it would
// let a rewritten release decide where the bytes come from.
func (ins *Installer) locate(
	ctx context.Context,
	coord Coordinate,
	platform Platform,
	locked LockArtifact,
	pinned, signatureDeclared bool,
) (artifactURL, checksumsURL string, err error) {
	if coord.Origin == OriginURL {
		// A URL coordinate publishes no checksums file by convention: its
		// signature, when there is one, covers the artifact itself.
		return coord.URL, "", nil
	}

	if pinned {
		artifactURL = locked.URL
		if signatureDeclared {
			checksumsURL = siblingURL(artifactURL, ChecksumsAsset)
		}
		return artifactURL, checksumsURL, nil
	}

	published, err := ins.fetcher().releaseByTag(ctx, coord)
	if err != nil {
		return "", "", err
	}
	if artifactURL, err = published.asset(coord, coord.AssetName(platform)); err != nil {
		return "", "", err
	}
	// An unpinned install has nothing to compare a download against, so the
	// convention's checksums file is mandatory here rather than optional.
	if checksumsURL, err = published.asset(coord, ChecksumsAsset); err != nil {
		return "", "", err
	}
	return artifactURL, checksumsURL, nil
}

// expected reports the digest the download must have, and whether a signature
// vouched for where that digest came from. The signature is verified before any
// digest is compared: a digest read out of a file nobody signed is a checksum,
// not a guarantee.
func (ins *Installer) expected(
	ctx context.Context,
	req Request,
	coord Coordinate,
	fileName string,
	artifact []byte,
	checksumsURL string,
) (digest string, signed bool, err error) {
	fetch := ins.fetcher()

	checksums := []byte(nil)
	if checksumsURL != "" {
		if checksums, err = fetch.get(ctx, checksumsURL, maxMetadataBytes); err != nil {
			return "", false, resolveFailure(coord, "downloading "+checksumsURL, err)
		}
	}

	if req.PubKey != "" {
		signedName, signedURL, material := ChecksumsAsset, checksumsURL, checksums
		if coord.Origin == OriginURL {
			signedName, signedURL, material = fileName, coord.URL, artifact
		}

		verify, err := loadVerifier(coord.Name, req.PubKey)
		if err != nil {
			return "", false, err
		}
		signature, err := fetch.get(ctx, signedURL+verify.signatureSuffix(), maxSignatureSize)
		if err != nil {
			return "", false, internalerror.NewPreconditionError(label(coord.Name)+" declares pubkey: "+req.PubKey+
				", but "+signedName+verify.signatureSuffix()+" is not published beside it: an unsigned artifact"+
				" is refused, not accepted unsigned", err)
		}
		if err := verify.verify(signedName, material, signature); err != nil {
			return "", false, err
		}
		signed = true
	}

	if len(checksums) == 0 {
		return "", signed, nil
	}
	digest, found := checksumFor(checksums, fileName)
	if !found {
		return "", signed, internalerror.NewPreconditionError(label(coord.Name)+": "+ChecksumsAsset+" for "+
			coord.From+" records no digest for "+fileName, nil)
	}
	return digest, signed, nil
}
