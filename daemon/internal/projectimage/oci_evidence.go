package projectimage

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

type ociImageConfig struct {
	Config struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type ociEvidence struct {
	Config    ociImageConfig
	Files     map[string][]byte
	FileModes map[string]int64
}

const (
	ociIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ociConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	ociLayerMediaType    = "application/vnd.oci.image.layer.v1.tar"
	ociGzipMediaType     = "application/vnd.oci.image.layer.v1.tar+gzip"
	maxOCIJSONBytes      = 4 << 20
	maxOCILayers         = 128
	maxOCILayerBytes     = 1 << 30
)

func readOCIEvidence(
	archivePath string,
	expectedDigest string,
	wanted map[string]int64,
) (ociEvidence, error) {
	indexBytes, err := readOCIArchiveEntry(archivePath, "index.json", maxOCIJSONBytes)
	if err != nil {
		return ociEvidence{}, err
	}
	var index ociIndex
	if err := decodeStrictJSON(indexBytes, &index); err != nil {
		return ociEvidence{}, fmt.Errorf("decode OCI image index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != ociIndexMediaType ||
		len(index.Manifests) != 1 || index.Manifests[0].Digest != expectedDigest {
		return ociEvidence{}, fmt.Errorf(
			"OCI image index does not bind exact digest %s", expectedDigest)
	}
	descriptor := index.Manifests[0]
	payload, err := readOCIBlob(archivePath, descriptor, maxOCIJSONBytes)
	if err != nil {
		return ociEvidence{}, err
	}
	if descriptor.MediaType == ociIndexMediaType {
		var platformIndex ociIndex
		if err := decodeStrictJSON(payload, &platformIndex); err != nil {
			return ociEvidence{}, fmt.Errorf("decode OCI platform index: %w", err)
		}
		if platformIndex.SchemaVersion != 2 ||
			platformIndex.MediaType != ociIndexMediaType ||
			len(platformIndex.Manifests) != 1 {
			return ociEvidence{}, fmt.Errorf("OCI platform index is not a single proven image")
		}
		descriptor = platformIndex.Manifests[0]
		payload, err = readOCIBlob(archivePath, descriptor, maxOCIJSONBytes)
		if err != nil {
			return ociEvidence{}, err
		}
	}
	if descriptor.MediaType != ociManifestMediaType {
		return ociEvidence{}, fmt.Errorf(
			"OCI image resolved to unsupported manifest type %q", descriptor.MediaType)
	}
	var manifest ociManifest
	if err := decodeStrictJSON(payload, &manifest); err != nil {
		return ociEvidence{}, fmt.Errorf("decode OCI image manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ociManifestMediaType ||
		len(manifest.Layers) == 0 || len(manifest.Layers) > maxOCILayers {
		return ociEvidence{}, fmt.Errorf("OCI image manifest has invalid layer set")
	}
	config, err := readOCIConfig(archivePath, manifest)
	if err != nil {
		return ociEvidence{}, err
	}
	layerDir, err := os.MkdirTemp("", "freeside-project-layers-*")
	if err != nil {
		return ociEvidence{}, fmt.Errorf("create OCI layer scratch: %w", err)
	}
	defer os.RemoveAll(layerDir) //nolint:errcheck // private one-shot proof artifact
	layerPaths, err := extractOCILayers(archivePath, manifest.Layers, layerDir)
	if err != nil {
		return ociEvidence{}, err
	}
	if err := verifyOCIDiffIDs(layerPaths, manifest.Layers, config.RootFS.DiffIDs); err != nil {
		return ociEvidence{}, err
	}
	evidence := ociEvidence{Config: config}
	if len(wanted) == 0 {
		return evidence, nil
	}
	files := make(map[string][]byte, len(wanted))
	modes := make(map[string]int64, len(wanted))
	for index := len(manifest.Layers) - 1; index >= 0; index-- {
		if err := readProvenanceLayer(
			layerPaths[index], manifest.Layers[index].MediaType, wanted, files, modes,
		); err != nil {
			return ociEvidence{}, err
		}
		if len(files) == len(wanted) {
			break
		}
	}
	for name := range wanted {
		if _, ok := files[name]; !ok {
			return ociEvidence{}, fmt.Errorf("project image rootfs lacks %s", name)
		}
	}
	evidence.Files = files
	evidence.FileModes = modes
	return evidence, nil
}

func readOCIConfig(archivePath string, manifest ociManifest) (ociImageConfig, error) {
	if manifest.Config.MediaType != ociConfigMediaType {
		return ociImageConfig{}, fmt.Errorf(
			"OCI image config has unsupported media type %q", manifest.Config.MediaType)
	}
	body, err := readOCIBlob(archivePath, manifest.Config, maxOCIJSONBytes)
	if err != nil {
		return ociImageConfig{}, err
	}
	var config ociImageConfig
	if err := decodeStrictJSON(body, &config); err != nil {
		return ociImageConfig{}, fmt.Errorf("decode OCI image config: %w", err)
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return ociImageConfig{}, fmt.Errorf(
			"OCI image config has %d diff IDs for %d layers",
			len(config.RootFS.DiffIDs), len(manifest.Layers))
	}
	for _, digest := range config.RootFS.DiffIDs {
		if !validDigest(digest) {
			return ociImageConfig{}, fmt.Errorf(
				"OCI image config has invalid diff ID %q", digest)
		}
	}
	return config, nil
}

func verifyOCIDiffIDs(
	layerPaths []string,
	layers []ociDescriptor,
	diffIDs []string,
) error {
	if len(layerPaths) != len(layers) || len(layers) != len(diffIDs) {
		return fmt.Errorf("OCI layer and diff ID counts do not match")
	}
	for index, layerPath := range layerPaths {
		layer, err := os.Open(layerPath) //nolint:gosec // private verified layer path
		if err != nil {
			return err
		}
		var reader io.Reader = layer
		var compressed *gzip.Reader
		if layers[index].MediaType == ociGzipMediaType {
			compressed, err = gzip.NewReader(layer)
			if err != nil {
				_ = layer.Close()
				return fmt.Errorf("decompress OCI layer %d: %w", index, err)
			}
			reader = compressed
		}
		hash := sha256.New()
		written, copyErr := io.Copy(hash, io.LimitReader(reader, maxOCILayerBytes+1))
		if compressed != nil {
			copyErr = errors.Join(copyErr, compressed.Close())
		}
		closeErr := layer.Close()
		if copyErr != nil {
			return fmt.Errorf("hash OCI layer %d: %w", index, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		observed := contentaddr.Format(hash.Sum(nil))
		if written > maxOCILayerBytes || observed != diffIDs[index] {
			return fmt.Errorf(
				"OCI layer %d does not match config diff ID %s", index, diffIDs[index])
		}
	}
	return nil
}

func readOCIArchiveEntry(archivePath, entryName string, limit int64) ([]byte, error) {
	archive, err := os.Open(archivePath) //nolint:gosec // private runtime export
	if err != nil {
		return nil, err
	}
	defer archive.Close() //nolint:errcheck // read-only runtime export
	reader := tar.NewReader(archive)
	var result []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read OCI archive: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name != entryName {
			continue
		}
		if result != nil || header.Typeflag != tar.TypeReg ||
			header.Size < 0 || header.Size > limit {
			return nil, fmt.Errorf("OCI archive has invalid %s", entryName)
		}
		body, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from OCI archive: %w", entryName, err)
		}
		if int64(len(body)) != header.Size {
			return nil, fmt.Errorf("OCI archive truncated %s", entryName)
		}
		result = body
	}
	if result == nil {
		return nil, fmt.Errorf("OCI archive lacks %s", entryName)
	}
	return result, nil
}

func readOCIBlob(archivePath string, descriptor ociDescriptor, limit int64) ([]byte, error) {
	if !validDigest(descriptor.Digest) || descriptor.Size < 0 || descriptor.Size > limit {
		return nil, fmt.Errorf("OCI descriptor is invalid")
	}
	entryName := "blobs/sha256/" + contentaddr.Hex(descriptor.Digest)
	body, err := readOCIArchiveEntry(archivePath, entryName, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != descriptor.Size ||
		contentaddr.Sum(body) != descriptor.Digest {
		return nil, fmt.Errorf("OCI blob %s does not match its descriptor", descriptor.Digest)
	}
	return body, nil
}

func extractOCILayers(
	archivePath string,
	layers []ociDescriptor,
	destination string,
) ([]string, error) {
	byEntry := make(map[string]int, len(layers))
	paths := make([]string, len(layers))
	for index, descriptor := range layers {
		if !validDigest(descriptor.Digest) || descriptor.Size < 0 ||
			descriptor.Size > maxOCILayerBytes ||
			(descriptor.MediaType != ociLayerMediaType &&
				descriptor.MediaType != ociGzipMediaType) {
			return nil, fmt.Errorf("OCI layer descriptor %d is invalid", index)
		}
		entry := "blobs/sha256/" + contentaddr.Hex(descriptor.Digest)
		if _, duplicate := byEntry[entry]; duplicate {
			return nil, fmt.Errorf("OCI image repeats layer %s", descriptor.Digest)
		}
		byEntry[entry] = index
		paths[index] = filepath.Join(destination, strconv.Itoa(index)+".layer")
	}
	archive, err := os.Open(archivePath) //nolint:gosec // private runtime export
	if err != nil {
		return nil, err
	}
	defer archive.Close() //nolint:errcheck // read-only runtime export
	found := make([]bool, len(layers))
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read OCI archive layers: %w", err)
		}
		entry := strings.TrimPrefix(header.Name, "./")
		index, ok := byEntry[entry]
		if !ok {
			continue
		}
		descriptor := layers[index]
		if found[index] || header.Typeflag != tar.TypeReg || header.Size != descriptor.Size {
			return nil, fmt.Errorf("OCI archive has invalid layer %s", descriptor.Digest)
		}
		layer, err := os.OpenFile(paths[index], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(layer, hash), reader, descriptor.Size)
		closeErr := layer.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if written != descriptor.Size ||
			contentaddr.Format(hash.Sum(nil)) != descriptor.Digest {
			return nil, fmt.Errorf("OCI layer %s does not match its descriptor", descriptor.Digest)
		}
		found[index] = true
	}
	for index, ok := range found {
		if !ok {
			return nil, fmt.Errorf("OCI archive lacks layer %s", layers[index].Digest)
		}
	}
	return paths, nil
}

func readProvenanceLayer(
	layerPath, mediaType string,
	wanted map[string]int64,
	files map[string][]byte,
	modes map[string]int64,
) error {
	layer, err := os.Open(layerPath) //nolint:gosec // fixed index under private layer scratch
	if err != nil {
		return err
	}
	defer layer.Close() //nolint:errcheck // read-only verified OCI layer
	var source io.Reader = layer
	if mediaType == ociGzipMediaType {
		compressed, err := gzip.NewReader(layer)
		if err != nil {
			return fmt.Errorf("decompress OCI layer: %w", err)
		}
		defer compressed.Close() //nolint:errcheck // read-only verified OCI layer
		source = compressed
	}
	reader := tar.NewReader(source)
	seen := map[string]bool{}
	resolvedBeforeLayer := make(map[string]bool, len(files))
	for name := range files {
		resolvedBeforeLayer[name] = true
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read OCI layer archive: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" {
			if header.Name == "./" && header.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("OCI layer has unsafe path %q", header.Name)
		}
		if strings.HasPrefix(name, "/") || pathpkg.Clean(name) != name {
			return fmt.Errorf("OCI layer has unsafe path %q", header.Name)
		}
		absoluteName := "/" + name
		if limit, ok := wanted[absoluteName]; ok {
			if seen[absoluteName] {
				return fmt.Errorf("OCI layer repeats %s", absoluteName)
			}
			seen[absoluteName] = true
			if _, resolved := files[absoluteName]; resolved {
				continue
			}
			if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > limit {
				return fmt.Errorf("project image rootfs has invalid %s", absoluteName)
			}
			body, err := io.ReadAll(io.LimitReader(reader, limit+1))
			if err != nil || int64(len(body)) != header.Size {
				return fmt.Errorf("project image rootfs truncated %s", absoluteName)
			}
			files[absoluteName] = body
			modes[absoluteName] = header.Mode
			continue
		}
		for target := range wanted {
			if resolvedBeforeLayer[target] {
				continue
			}
			if name == ".wh..wh..opq" {
				return fmt.Errorf("project image rootfs opaquely hides %s", target)
			}
			relativeTarget := strings.TrimPrefix(target, "/")
			parent := pathpkg.Dir(relativeTarget)
			whiteout := pathpkg.Join(parent, ".wh."+pathpkg.Base(relativeTarget))
			if name == whiteout {
				return fmt.Errorf("project image rootfs whiteouts %s", target)
			}
			for ancestor := parent; ancestor != "." && ancestor != "/"; ancestor = pathpkg.Dir(ancestor) {
				ancestorWhiteout := pathpkg.Join(
					pathpkg.Dir(ancestor), ".wh."+pathpkg.Base(ancestor))
				if name == ancestorWhiteout {
					return fmt.Errorf(
						"project image rootfs whiteouts ancestor of %s", target)
				}
				if name == pathpkg.Join(ancestor, ".wh..wh..opq") {
					return fmt.Errorf("project image rootfs opaquely hides %s", target)
				}
				if name == ancestor && header.Typeflag != tar.TypeDir {
					return fmt.Errorf("project image rootfs replaces ancestor of %s", target)
				}
			}
		}
	}
}
