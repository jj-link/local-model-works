package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
)

type packageDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}
type receivedPackageManifest struct {
	SchemaVersion int                 `json:"schemaVersion"`
	ArtifactType  string              `json:"artifactType"`
	Config        packageDescriptor   `json:"config"`
	Layers        []packageDescriptor `json:"layers"`
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func decodePackageManifest(data []byte, digest string) (*receivedPackageManifest, error) {
	var manifest receivedPackageManifest
	if len(data) > recipe.MaxConfigBytes || sha256Bytes(data) != digest || json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 2 || manifest.ArtifactType != recipe.ArtifactType || len(manifest.Layers) != 1 {
		return nil, fmt.Errorf("download.package_manifest_invalid")
	}
	if manifest.Config.MediaType != recipe.ConfigMediaType || manifest.Layers[0].MediaType != recipe.LayerMediaType || manifest.Config.Size < 0 || manifest.Config.Size > recipe.MaxConfigBytes || manifest.Layers[0].Size < 0 || manifest.Layers[0].Size > recipe.MaxCompressedLayerBytes {
		return nil, fmt.Errorf("download.package_descriptor_invalid")
	}
	for _, descriptor := range []packageDescriptor{manifest.Config, manifest.Layers[0]} {
		if !validHFFileDigest(descriptor.Digest) || !strings.HasPrefix(descriptor.Digest, "sha256:") {
			return nil, fmt.Errorf("download.package_digest_invalid")
		}
	}
	return &manifest, nil
}
func (a *Agent) readRecipeTransport(ctx context.Context, digest string, withBytes bool) ([]byte, []byte, []byte, error) {
	transport, err := a.clientTransport()
	if err != nil {
		return nil, nil, nil, err
	}
	client := httpClient(transport)
	read := func(kind string, limit int64) ([]byte, error) {
		endpoint := strings.TrimSuffix(a.cfg.ServerURL, "/") + "/packages/" + url.PathEscape(digest) + "/" + kind
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download.package_transport_failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download.package_http_%d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, fmt.Errorf("download.package_blob_oversized")
		}
		return data, nil
	}
	manifestJSON, err := read("manifest", recipe.MaxConfigBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	manifest, err := decodePackageManifest(manifestJSON, digest)
	if err != nil {
		return nil, nil, nil, err
	}
	if !withBytes {
		return manifestJSON, nil, nil, nil
	}
	config, err := read("config", manifest.Config.Size)
	if err != nil {
		return nil, nil, nil, err
	}
	layer, err := read("layer", manifest.Layers[0].Size)
	if err != nil {
		return nil, nil, nil, err
	}
	if int64(len(config)) != manifest.Config.Size || sha256Bytes(config) != manifest.Config.Digest || int64(len(layer)) != manifest.Layers[0].Size || sha256Bytes(layer) != manifest.Layers[0].Digest {
		return nil, nil, nil, fmt.Errorf("download.package_blob_mismatch")
	}
	return manifestJSON, config, layer, nil
}
func writeReceivedLayout(root, digest string, manifest, config, layer []byte) error {
	decoded, err := decodePackageManifest(manifest, digest)
	if err != nil {
		return err
	}
	if sha256Bytes(config) != decoded.Config.Digest || sha256Bytes(layer) != decoded.Layers[0].Digest {
		return fmt.Errorf("download.package_blob_mismatch")
	}
	blobRoot := filepath.Join(root, "blobs", "sha256")
	if err := safeDestination(blobRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(blobRoot, 0755); err != nil {
		return err
	}
	for _, blob := range []struct {
		digest string
		bytes  []byte
	}{{digest, manifest}, {decoded.Config.Digest, config}, {decoded.Layers[0].Digest, layer}} {
		if err := writeImmutablePackageFile(filepath.Join(blobRoot, strings.TrimPrefix(blob.digest, "sha256:")), blob.bytes); err != nil {
			return err
		}
	}
	index, _ := json.Marshal(struct {
		SchemaVersion int                 `json:"schemaVersion"`
		Blobs         []packageDescriptor `json:"blobs"`
	}{2, []packageDescriptor{{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: digest, Size: int64(len(manifest))}}})
	if err := writeImmutablePackageFile(filepath.Join(root, "index.json"), index); err != nil {
		return err
	}
	return writeImmutablePackageFile(filepath.Join(root, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`))
}
func verifyRecipeBytes(ctx context.Context, root, digest string, manifest, config, layer []byte) error {
	decoded, err := decodePackageManifest(manifest, digest)
	if err != nil {
		return err
	}
	if sha256Bytes(config) != decoded.Config.Digest || sha256Bytes(layer) != decoded.Layers[0].Digest {
		return fmt.Errorf("download.package_blob_mismatch")
	}
	if _, err := os.Stat(filepath.Join(root, "oci-layout")); err == nil {
		packed, err := recipe.ReadLayout(root)
		if err != nil || packed.ManifestDigest != digest {
			return fmt.Errorf("download.package_layout_invalid")
		}
		return recipe.VerifyExtractedAssets(root)
	}
	// Older agents cached only assets. Prove their bytes against the authenticated
	// digest-pinned package without altering any helpers mounted by workloads.
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	expected := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("download.package_asset_type_invalid")
		}
		rel, err := safeRelativePath(header.Name)
		if err != nil {
			return err
		}
		if expected[rel] || header.Size < 0 || header.Size > recipe.MaxAssetFileBytes {
			return fmt.Errorf("download.package_asset_invalid")
		}
		expected[rel] = true
		path := filepath.Join(root, "assets", rel)
		if err := safeDestination(path); err != nil {
			return err
		}
		actual, size, err := digestFile(ctx, path)
		if err != nil || size != header.Size {
			return fmt.Errorf("download.package_asset_missing")
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, tr); err != nil {
			return err
		}
		if actual != "sha256:"+hex.EncodeToString(hash.Sum(nil)) {
			return fmt.Errorf("download.package_asset_corrupt")
		}
	}
	return filepath.WalkDir(filepath.Join(root, "assets"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(root, "assets"), path)
		if err != nil || !expected[rel] || !entry.Type().IsRegular() {
			return fmt.Errorf("download.package_unexpected_asset")
		}
		return nil
	})
}

func writeImmutablePackageFile(path string, data []byte) error {
	if err := safeDestination(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		digest, size, err := digestFile(context.Background(), path)
		if err != nil || size != int64(len(data)) || digest != sha256Bytes(data) {
			return fmt.Errorf("download.package_existing_metadata_conflict")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	partial := path + ".part"
	if err := safeDestination(partial); err != nil {
		return err
	}
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(partial, data, 0444); err != nil {
		return err
	}
	defer os.Remove(partial)
	return os.Link(partial, path)
}
