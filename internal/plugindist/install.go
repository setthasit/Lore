package plugindist

import (
	"context"
	"net/http"
	"time"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// downloadTimeout bounds a whole artifact fetch, not one read: a stalled supply chain must fail, not hang.
const downloadTimeout = 5 * time.Minute

type Installer struct {
	store   *Store
	client  *http.Client
	fetcher fetcher
}

func NewInstaller(store *Store) *Installer {
	return newInstaller(store, &http.Client{Timeout: downloadTimeout}, DefaultAPIBase())
}

func newInstaller(store *Store, client *http.Client, apiBase string) *Installer {
	return &Installer{store: store, client: client, fetcher: fetcher{client: client, apiBase: apiBase}}
}

type Request struct {
	Coordinate Coordinate
	Rewrite    bool
}

type Result struct {
	Report

	Signed bool // a declared pubkey verified the material the digest came from
	Locked bool // the digest was already pinned, and this install matched it
	Pinned bool // this install wrote the pin, so the lockfile changed
	Trust  bool // nothing vouched for the artifact but the artifact itself
}

// Pin resolves @latest to the concrete version the caller then writes back into lore.yaml.
func (ins *Installer) Pin(ctx context.Context, coord Coordinate) (Coordinate, error) {
	if !coord.Floating() {
		return coord, nil
	}

	latest, err := ins.fetcher.latestRelease(ctx, coord)
	if err != nil {
		return Coordinate{}, err
	}
	if latest.TagName == "" {
		return Coordinate{}, internalerror.NewPreconditionError(Label(coord.Name)+" cannot resolve "+coord.SafeFrom()+
			": github.com/"+coord.Owner+"/"+coord.Repo+" publishes no release", nil)
	}
	return coord.AtVersion(latest.TagName)
}

// Install records what it pinned in lock but never writes it: the caller saves once every install has succeeded.
func (ins *Installer) Install(ctx context.Context, req Request, lock *Lock) (Result, error) {
	coord := req.Coordinate
	if coord.Floating() {
		return Result{}, internalerror.NewInternalError(Label(coord.Name)+" reached install still pinned at @"+
			LatestVersion, nil)
	}

	if coord.Origin == OriginLocal {
		report, err := ins.store.Locate(coord, lock)
		if err != nil {
			return Result{}, err
		}
		return Result{Report: report}, nil
	}

	result := Result{Report: Report{
		Name: coord.Name, Origin: coord.Origin, Platform: ins.store.platform.Key(),
		Version: coord.Version, Warning: coord.Warning(),
	}}

	platform := ins.store.platform
	from := coord.SafeFrom()
	entry, hasEntry := lock.Entry(coord.Name)
	if hasEntry && !req.Rewrite {
		if entry.Version != coord.Version {
			return Result{}, internalerror.NewPreconditionError(Label(coord.Name)+" is locked at "+entry.Version+
				" but "+from+" asks for "+coord.Version+updateRemedy(coord.Name), nil)
		}
		if !sameFrom(entry.From, coord.From) {
			return Result{}, internalerror.NewPreconditionError(Label(coord.Name)+" is locked to "+
				lockedOrigin(safeFrom(entry.From))+", not "+from+updateRemedy(coord.Name), nil)
		}
	}
	locked, hasLocked := lock.Artifact(coord.Name, platform)
	pinned := hasLocked && !req.Rewrite

	artifactURL, checksumsURL, err := ins.locate(ctx, coord, platform, locked, pinned, coord.PubKey != "")
	if err != nil {
		return Result{}, err
	}
	lockedURL := safeTarget(artifactURL)
	result.LockedURL = lockedURL

	artifact, err := BoundedGet(ctx, ins.client, artifactURL, maxArtifactBytes)
	if err != nil {
		return Result{}, resolveFailure(coord, "downloading "+lockedURL, err)
	}

	fileName := artifactFileName(artifactURL)
	expected, signed, err := ins.expected(ctx, coord, fileName, artifact, checksumsURL)
	if err != nil {
		return Result{}, err
	}
	result.Signed = signed

	digest := digestOf(artifact)
	if expected != "" && expected != digest {
		return Result{}, digestMismatch(coord.Name, platform, expected, digest)
	}
	if pinned && locked.Digest != digest {
		return Result{}, digestMismatch(coord.Name, platform, locked.Digest, digest)
	}
	result.LockedDigest, result.Locked = digest, pinned
	result.Trust = expected == "" && !pinned

	binaryName, body, err := unpack(coord, platform, fileName, artifact)
	if err != nil {
		return Result{}, err
	}
	path, binaryDigest, err := ins.store.write(coord, binaryName, body, digest)
	if err != nil {
		return Result{}, err
	}
	result.Binary, result.BinaryDigest = path, binaryDigest

	if !pinned {
		lock.Set(coord.Name, coord.Version, from, platform, LockArtifact{URL: lockedURL, Digest: digest})
		result.Pinned = true
	}
	return result, nil
}

func updateRemedy(name string) string {
	return " — run: lore plugin update " + name
}

func lockedOrigin(from string) string {
	if from == "" {
		return "an origin " + LockFileName + " does not record"
	}
	return from
}

// A pinned platform is fetched from the URL the lockfile recorded; the release is not consulted at all.
func (ins *Installer) locate(
	ctx context.Context,
	coord Coordinate,
	platform Platform,
	locked LockArtifact,
	pinned, signatureDeclared bool,
) (artifactURL, checksumsURL string, err error) {
	if coord.Origin == OriginURL {
		// A URL coordinate publishes no checksums file by convention; its signature covers the artifact itself.
		return coord.URL, "", nil
	}

	if pinned {
		artifactURL = locked.URL
		if signatureDeclared {
			checksumsURL = siblingURL(artifactURL, ChecksumsAsset)
		}
		return artifactURL, checksumsURL, nil
	}

	published, err := ins.fetcher.releaseByTag(ctx, coord)
	if err != nil {
		return "", "", err
	}
	if artifactURL, err = published.asset(coord, coord.assetName(platform)); err != nil {
		return "", "", err
	}
	// An unpinned install has nothing to compare a download against, so the checksums file is mandatory here.
	if checksumsURL, err = published.asset(coord, ChecksumsAsset); err != nil {
		return "", "", err
	}
	return artifactURL, checksumsURL, nil
}

// The signature is verified before any digest is compared: a digest from a file nobody signed is only a checksum.
func (ins *Installer) expected(
	ctx context.Context,
	coord Coordinate,
	fileName string,
	artifact []byte,
	checksumsURL string,
) (digest string, signed bool, err error) {
	client := ins.client

	checksums := []byte(nil)
	if checksumsURL != "" {
		if checksums, err = BoundedGet(ctx, client, checksumsURL, MaxMetadataBytes); err != nil {
			return "", false, resolveFailure(coord, "downloading "+safeTarget(checksumsURL), err)
		}
	}

	if coord.PubKey != "" {
		signedName, signedURL, material := ChecksumsAsset, checksumsURL, checksums
		if coord.Origin == OriginURL {
			signedName, signedURL, material = fileName, coord.URL, artifact
		}

		verify, err := loadVerifier(coord.Name, coord.PubKey)
		if err != nil {
			return "", false, err
		}
		signature, err := BoundedGet(ctx, client, signedURL+verify.signatureSuffix(), maxSignatureSize)
		if err != nil {
			return "", false, internalerror.NewPreconditionError(Label(coord.Name)+" declares pubkey: "+coord.PubKey+
				", but "+signedName+verify.signatureSuffix()+" is not published beside it: an unsigned artifact"+
				" is refused, not accepted unsigned", err)
		}
		if err := verify.verify(signedName, material, signature); err != nil {
			return "", false, err
		}
		signed = true
	}

	if checksumsURL == "" {
		return "", signed, nil
	}
	digest, found := checksumFor(checksums, fileName)
	if !found {
		return "", signed, internalerror.NewPreconditionError(Label(coord.Name)+": "+ChecksumsAsset+" for "+
			coord.SafeFrom()+" records no usable digest for "+fileName, nil)
	}
	return digest, signed, nil
}
