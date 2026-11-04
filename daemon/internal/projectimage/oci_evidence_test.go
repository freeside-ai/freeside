package projectimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestReadOCIConfigBindsManifestDescriptor(t *testing.T) {
	configBody := []byte(`{"config":{"Labels":{"proof":"bound"}},"rootfs":{"diff_ids":["sha256:` +
		strings.Repeat("a", 64) + `"]}}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(configBody))
	descriptor := ociDescriptor{
		MediaType: ociConfigMediaType,
		Digest:    digest,
		Size:      int64(len(configBody)),
	}
	writeArchive := func(t *testing.T, body []byte) string {
		t.Helper()
		path := t.TempDir() + "/image.tar"
		file, err := os.Create(path) //nolint:gosec // fixed filename under test-owned scratch
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		header := &tar.Header{
			Name: "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:"),
			Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	manifest := ociManifest{
		Config: descriptor,
		Layers: []ociDescriptor{{Digest: "sha256:" + strings.Repeat("b", 64)}},
	}
	config, err := readOCIConfig(writeArchive(t, configBody), manifest)
	if err != nil || config.Config.Labels["proof"] != "bound" ||
		len(config.RootFS.DiffIDs) != 1 {
		t.Fatalf("readOCIConfig = %+v, %v", config, err)
	}
	if _, err := readOCIConfig(writeArchive(t, append(configBody, '\n')), manifest); err == nil {
		t.Fatal("readOCIConfig accepted bytes that did not match the manifest descriptor")
	}
}

func TestVerifyOCIDiffIDsHashesUncompressedLayerBytes(t *testing.T) {
	body := []byte("uncompressed layer bytes")
	diffID := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	plainPath := t.TempDir() + "/plain.layer"
	if err := os.WriteFile(plainPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	gzipPath := t.TempDir() + "/gzip.layer"
	if err := os.WriteFile(gzipPath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		path      string
		mediaType string
	}{
		{"plain", plainPath, ociLayerMediaType},
		{"gzip", gzipPath, ociGzipMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyOCIDiffIDs(
				[]string{tc.path},
				[]ociDescriptor{{MediaType: tc.mediaType}},
				[]string{diffID},
			); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := verifyOCIDiffIDs(
		[]string{plainPath},
		[]ociDescriptor{{MediaType: ociLayerMediaType}},
		[]string{"sha256:" + strings.Repeat("0", 64)},
	); err == nil {
		t.Fatal("verifyOCIDiffIDs accepted a layer that did not match the bound config")
	}
}

func TestReadProvenanceArchiveRequiresUniqueRegularBoundedFiles(t *testing.T) {
	type archiveEntry struct {
		name     string
		body     string
		typeflag byte
	}
	writeArchive := func(t *testing.T, entries []archiveEntry) string {
		t.Helper()
		path := t.TempDir() + "/rootfs.tar"
		file, err := os.Create(path) //nolint:gosec // fixed filename under the test-owned temporary directory
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		for _, entry := range entries {
			header := &tar.Header{
				Name: entry.name, Size: int64(len(entry.body)),
				Mode: 0o600, Typeflag: entry.typeflag,
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	validEntries := []archiveEntry{
		{"./", "", tar.TypeDir},
		{strings.TrimPrefix(ward.ProjectRecipePath, "/"), testRecipe, tar.TypeReg},
		{"usr/local/bin/freeside-project-prepare", prepareScript, tar.TypeReg},
	}
	wanted := map[string]int64{
		ward.ProjectRecipePath: maxRecipeBytes,
		PreparationPath:        maxPrepareBytes,
	}
	files := map[string][]byte{}
	modes := map[string]int64{}
	err := readProvenanceLayer(
		writeArchive(t, validEntries), ociLayerMediaType, wanted, files, modes,
	)
	if err != nil || string(files[PreparationPath]) != prepareScript {
		t.Fatalf("readProvenanceLayer = %v, %v", files, err)
	}
	duplicate := append(append([]archiveEntry{}, validEntries...), validEntries[1])
	if err := readProvenanceLayer(
		writeArchive(t, duplicate), ociLayerMediaType, wanted,
		map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a duplicate provenance file")
	}
	symlink := append([]archiveEntry{}, validEntries...)
	symlink[1].typeflag = tar.TypeSymlink
	symlink[1].body = ""
	if err := readProvenanceLayer(
		writeArchive(t, symlink), ociLayerMediaType, wanted,
		map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a symlinked preparation helper")
	}
	ancestorWhiteout := []archiveEntry{
		{"usr/local/share/.wh.freeside", "", tar.TypeReg},
	}
	if err := readProvenanceLayer(
		writeArchive(t, ancestorWhiteout), ociLayerMediaType, wanted,
		map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted an ancestor whiteout")
	}
	rootOpaqueWhiteout := []archiveEntry{
		{".wh..wh..opq", "", tar.TypeReg},
	}
	if err := readProvenanceLayer(
		writeArchive(t, rootOpaqueWhiteout), ociLayerMediaType, wanted,
		map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a root opaque whiteout")
	}
	laterAncestorReplacement := append(append([]archiveEntry{}, validEntries...),
		archiveEntry{"usr/local", "", tar.TypeSymlink})
	if err := readProvenanceLayer(
		writeArchive(t, laterAncestorReplacement),
		ociLayerMediaType, wanted, map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a later same-layer ancestor replacement")
	}
	// #518: the builder clears the launcher paths with a base-independent rm
	// before the launcher COPYs, so a launcher whiteout lands in a layer OLDER
	// than the launcher's regular file. readOCIEvidence walks newest-first, so
	// the launcher resolves before the older whiteout layer is read; that
	// whiteout must then be skipped, not refused.
	launcherWanted := map[string]int64{nodeLauncherPath: maxPrepareBytes}
	launcherWhiteout := []archiveEntry{{"usr/local/bin/.wh.node", "", tar.TypeReg}}
	resolvedLauncher := map[string][]byte{nodeLauncherPath: []byte(nodeToolchainLauncher)}
	if err := readProvenanceLayer(
		writeArchive(t, launcherWhiteout), ociLayerMediaType, launcherWanted,
		resolvedLauncher, map[string]int64{nodeLauncherPath: 0o755},
	); err != nil {
		t.Fatalf(
			"readProvenanceLayer refused an older-layer launcher whiteout under a resolved launcher: %v",
			err)
	}
	// A launcher whiteout in a layer NEWER than the launcher (unresolved when
	// the whiteout layer is read) still refuses.
	if err := readProvenanceLayer(
		writeArchive(t, launcherWhiteout), ociLayerMediaType, launcherWanted,
		map[string][]byte{}, map[string]int64{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a launcher whiteout newer than the launcher layer")
	}
}
