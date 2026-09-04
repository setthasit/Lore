package plugindist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// unpack takes the artifact apart and returns the file name and bytes of the
// plugin binary. A URL may also serve a bare binary, which private
// distribution often does, so an artifact that is not an archive is the binary.
func unpack(c Coordinate, p Platform, artifactName string, artifact []byte) (string, []byte, error) {
	if !isArchive(artifactName) {
		return c.binaryName(p), artifact, nil
	}

	files, err := untar(c, artifact)
	if err != nil {
		return "", nil, err
	}
	return pickBinary(c, p, files)
}

func isArchive(name string) bool {
	for _, suffix := range archiveSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// archived is one regular file out of an archive. Only its base name is kept:
// nothing nested is ever written, so an entry named ../../etc/cron.d/x is
// structurally unable to escape rather than escaping unless a check catches it.
type archived struct {
	name       string
	executable bool
	body       []byte
}

func untar(c Coordinate, artifact []byte) ([]archived, error) {
	stream, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return nil, internalerror.NewPreconditionError(label(c.Name)+": the artifact for "+c.From+
			" is not a gzip archive", err)
	}
	defer func() { _ = stream.Close() }()

	files, budget := []archived(nil), int64(maxArtifactBytes)
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, internalerror.NewPreconditionError(label(c.Name)+": the artifact for "+c.From+
				" is not a readable tar archive", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		body, err := io.ReadAll(io.LimitReader(reader, budget+1))
		if err != nil {
			return nil, internalerror.NewPreconditionError(label(c.Name)+": cannot read "+header.Name+
				" out of the artifact for "+c.From, err)
		}
		if int64(len(body)) > budget {
			return nil, internalerror.NewPreconditionError(label(c.Name)+": the artifact for "+c.From+
				" unpacks to more than the "+strconv.Itoa(maxArtifactBytes>>20)+" MiB this build will accept", nil)
		}
		budget -= int64(len(body))

		name := path.Base(header.Name)
		if name == "." || name == ".." || name == "/" {
			continue
		}
		files = append(files, archived{name: name, executable: header.FileInfo().Mode()&0o111 != 0, body: body})
	}
	return files, nil
}

// pickBinary chooses deterministically: the name the convention promises, then
// the one executable file if there is exactly one. Guessing among several is
// refused, because the wrong guess is a program the user did not agree to run.
func pickBinary(c Coordinate, p Platform, files []archived) (string, []byte, error) {
	want := c.binaryName(p)
	for _, file := range files {
		if file.name == want {
			return file.name, file.body, nil
		}
	}

	executables := make([]archived, 0, 1)
	for _, file := range files {
		if file.executable {
			executables = append(executables, file)
		}
	}
	if len(executables) == 1 {
		return executables[0].name, executables[0].body, nil
	}

	held := "nothing"
	if names := archivedNames(files); len(names) > 0 {
		held = strings.Join(names, ", ")
	}
	return "", nil, internalerror.NewPreconditionError(label(c.Name)+": the artifact for "+c.From+
		" holds no plugin binary — looked for "+want+", and it holds: "+held, nil)
}

func archivedNames(files []archived) []string {
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.name)
	}
	return names
}
