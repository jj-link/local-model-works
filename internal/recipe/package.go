package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Media types and artifact types for the recipe package format.
const (
	ArtifactType               = "application/vnd.localmodelworks.recipe.v1"
	ConfigMediaType            = "application/vnd.localmodelworks.recipe.config.v1+json"
	LayerMediaType             = "application/vnd.localmodelworks.recipe.assets.v1.tar+gzip"
	CatalogArtifactType        = "application/vnd.localmodelworks.catalog.v1"
	CatalogConfigType          = "application/vnd.localmodelworks.catalog.config.v1+json"
	SigstoreBundleArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json"

	// LabelPrefix marks LMW-managed containers.
	LabelPrefix = "dev.localmodelworks."
)

const (
	MaxConfigBytes          = 1 << 20
	MaxCompressedLayerBytes = 64 << 20
	MaxAssetFiles           = 256
	MaxAssetFileBytes       = 16 << 20
	MaxExtractedAssetBytes  = 128 << 20
)

// packageTarTime is the fixed mtime for deterministic layers.
var packageTarTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ociDescriptor is the OCI content descriptor.
type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ociManifest is the OCI image manifest for a recipe package.
type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	ArtifactType  string            `json:"artifactType"`
	Subject       *ociDescriptor    `json:"subject,omitempty"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType,omitempty"`
	Blobs         []ociDescriptor `json:"blobs"`
}

// PackResult describes a materialized package.
type PackResult struct {
	ManifestDigest string
	ManifestJSON   []byte
	ConfigDigest   string
	LayerDigest    string
	ConfigSize     int64
	LayerSize      int64
	ConfigJSON     []byte
	layerBytes     []byte
}

// PackError is a stable, machine-readable packaging failure. Code follows
// the recipe.* diagnostic convention so install/launch previews and API
// surfaces can surface it unchanged; Message is human-readable.
type PackError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Asset   string `json:"asset,omitempty"`
}

func (e *PackError) Error() string {
	if e.Asset != "" {
		return fmt.Sprintf("%s: asset %q: %s", e.Code, e.Asset, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func packErr(code, asset, msg string) *PackError {
	return &PackError{Code: code, Asset: asset, Message: msg}
}

// PackManifest builds the OCI manifest for a validated canonical document and
// its asset files (path -> content). Output is fully deterministic.
func PackManifest(doc []byte, assets map[string][]byte, annotations map[string]string) (*PackResult, error) {
	if len(doc) > MaxConfigBytes {
		return nil, packErr("recipe.config_too_large", "", "canonical config exceeds 1 MiB")
	}
	canon, err := Canonical(doc)
	if err != nil {
		return nil, err
	}
	if len(canon) > MaxConfigBytes {
		return nil, packErr("recipe.config_too_large", "", "canonical config exceeds 1 MiB")
	}
	configSum := sha256.Sum256(canon)
	configDigest := "sha256:" + hex.EncodeToString(configSum[:])
	layerBytes, err := buildAssetLayer(assets)
	if err != nil {
		return nil, err
	}
	if len(layerBytes) > MaxCompressedLayerBytes {
		return nil, packErr("recipe.layer_too_large", "", "compressed asset layer exceeds 64 MiB")
	}
	lsum := sha256.Sum256(layerBytes)
	layerDigest := "sha256:" + hex.EncodeToString(lsum[:])
	layerSize := int64(len(layerBytes))

	ann := map[string]string{
		"org.opencontainers.artifact.created": "2026-01-01T00:00:00Z",
	}
	for k, v := range annotations {
		ann[k] = v
	}

	m := ociManifest{
		SchemaVersion: 2,
		ArtifactType:  ArtifactType,
		Config:        ociDescriptor{MediaType: ConfigMediaType, Digest: configDigest, Size: int64(len(canon))},
		Layers:        []ociDescriptor{{MediaType: LayerMediaType, Digest: layerDigest, Size: layerSize}},
		Annotations:   ann,
	}
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	msum := sha256.Sum256(manifestJSON)
	return &PackResult{
		ManifestDigest: "sha256:" + hex.EncodeToString(msum[:]),
		ManifestJSON:   manifestJSON,
		ConfigDigest:   configDigest,
		LayerDigest:    layerDigest,
		ConfigSize:     int64(len(canon)),
		LayerSize:      layerSize,
		ConfigJSON:     canon,
		layerBytes:     layerBytes,
	}, nil
}

// buildAssetLayer produces a deterministic tar.gz of the asset files:
// sorted entries, fixed mtime, PAX format, gzip with zero header time.
func buildAssetLayer(assets map[string][]byte) ([]byte, error) {
	if len(assets) > MaxAssetFiles {
		return nil, packErr("recipe.too_many_assets", "", "asset count exceeds 256")
	}
	total := 0
	for name, data := range assets {
		if len(data) > MaxAssetFileBytes {
			return nil, packErr("recipe.asset_too_large", name, "asset exceeds 16 MiB")
		}
		total += len(data)
		if total > MaxExtractedAssetBytes {
			return nil, packErr("recipe.assets_too_large", "", "assets exceed 128 MiB")
		}
	}
	var names []string
	for n := range assets {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		clean := path.Clean(name)
		if clean != name || strings.HasPrefix(name, "/") || clean == ".." || strings.Contains(clean, "/../") {
			return nil, packErr("recipe.asset-escape", name, "escapes the package root after normalization")
		}
		data := assets[name]
		if err := tw.WriteHeader(&tar.Header{
			Name:     clean,
			Mode:     0o555,
			Size:     int64(len(data)),
			ModTime:  packageTarTime,
			Format:   tar.FormatPAX,
			Typeflag: tar.TypeReg,
		}); err != nil {
			return nil, fmt.Errorf("asset %q: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("asset %q: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteLayout writes a complete OCI layout directory for the pack result and
// verifies every blob's digest matches its descriptor.
func WriteLayout(dir string, res *PackResult) error {
	blobDir := path.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(blobDir, res.ConfigDigest[len("sha256:"):]), res.ConfigJSON, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(blobDir, res.LayerDigest[len("sha256:"):]), res.layerBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(path.Join(blobDir, res.ManifestDigest[len("sha256:"):]), res.ManifestJSON, 0o644); err != nil {
		return err
	}
	idx := ociIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Blobs:         []ociDescriptor{{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: res.ManifestDigest, Size: int64(len(res.ManifestJSON))}},
	}
	idxJSON, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(path.Join(dir, "index.json"), idxJSON, 0o644); err != nil {
		return err
	}
	return VerifyLayout(dir)
}

// ReadLayout loads and verifies one complete recipe package.
func ReadLayout(dir string) (*PackResult, error) {
	if err := VerifyLayout(dir); err != nil {
		return nil, err
	}
	idxRaw, err := os.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var idx ociIndex
	if err := json.Unmarshal(idxRaw, &idx); err != nil || len(idx.Blobs) != 1 {
		return nil, fmt.Errorf("invalid recipe package index")
	}
	manifestJSON, err := os.ReadFile(path.Join(dir, "blobs", "sha256", strings.TrimPrefix(idx.Blobs[0].Digest, "sha256:")))
	if err != nil {
		return nil, err
	}
	var manifest ociManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return nil, err
	}
	configJSON, err := os.ReadFile(path.Join(dir, "blobs", "sha256", strings.TrimPrefix(manifest.Config.Digest, "sha256:")))
	if err != nil {
		return nil, err
	}
	layerBytes, err := os.ReadFile(path.Join(dir, "blobs", "sha256", strings.TrimPrefix(manifest.Layers[0].Digest, "sha256:")))
	if err != nil {
		return nil, err
	}
	return &PackResult{
		ManifestDigest: idx.Blobs[0].Digest,
		ManifestJSON:   manifestJSON,
		ConfigDigest:   manifest.Config.Digest,
		LayerDigest:    manifest.Layers[0].Digest,
		ConfigSize:     int64(len(configJSON)),
		LayerSize:      int64(len(layerBytes)),
		ConfigJSON:     configJSON,
		layerBytes:     layerBytes,
	}, nil
}

// ReadPackageLayer returns the verified deterministic asset layer for one
// installed manifest digest.
func ReadPackageLayer(root, manifestDigest string) ([]byte, string, error) {
	if !sha256DigestRE.MatchString(manifestDigest) {
		return nil, "", fmt.Errorf("invalid recipe package digest")
	}
	dir := filepath.Join(root, strings.TrimPrefix(manifestDigest, "sha256:"))
	result, err := ReadLayout(dir)
	if err != nil {
		return nil, "", err
	}
	if result.ManifestDigest != manifestDigest {
		return nil, "", fmt.Errorf("recipe package digest mismatch")
	}
	return append([]byte(nil), result.layerBytes...), result.LayerDigest, nil
}

// PersistPackage atomically publishes a verified package and extracted asset
// tree below root/<manifest-sha>. The returned bool is true only when this
// call created the package and therefore owns rollback on a later DB failure.
func PersistPackage(root string, res *PackResult) (string, bool, error) {
	if !sha256DigestRE.MatchString(res.ManifestDigest) {
		return "", false, fmt.Errorf("invalid recipe manifest digest")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", false, err
	}
	final := path.Join(root, strings.TrimPrefix(res.ManifestDigest, "sha256:"))
	if _, err := os.Stat(final); err == nil {
		if err := VerifyLayout(final); err != nil {
			return "", false, fmt.Errorf("stored package verification: %w", err)
		}
		if err := verifyExtractedAssets(final); err != nil {
			return "", false, fmt.Errorf("stored assets verification: %w", err)
		}
		return final, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	tmp, err := os.MkdirTemp(root, ".package-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(tmp)
	if err := WriteLayout(tmp, res); err != nil {
		return "", false, err
	}
	assets := path.Join(tmp, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		return "", false, err
	}
	if err := UnpackLayer(res.layerBytes, assets); err != nil {
		return "", false, err
	}
	if err := verifyExtractedAssets(tmp); err != nil {
		return "", false, err
	}
	if err := freezeAssets(assets); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			if verifyErr := VerifyLayout(final); verifyErr != nil {
				return "", false, verifyErr
			}
			return final, false, nil
		}
		return "", false, err
	}
	return final, true, nil
}

func freezeAssets(root string) error {
	return filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(current, 0o555)
		}
		return os.Chmod(current, 0o555)
	})
}

func verifyExtractedAssets(packageDir string) error {
	res, err := ReadLayout(packageDir)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(res.layerBytes))
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
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxAssetFileBytes+1))
		if err != nil || len(data) > MaxAssetFileBytes {
			return fmt.Errorf("read extracted asset %q", header.Name)
		}
		stored, err := os.ReadFile(filepath.Join(packageDir, "assets", filepath.FromSlash(header.Name)))
		if err != nil || !bytes.Equal(data, stored) {
			return fmt.Errorf("extracted asset %q mismatch", header.Name)
		}
		expected[filepath.Clean(header.Name)] = true
	}
	return filepath.Walk(filepath.Join(packageDir, "assets"), func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(packageDir, "assets"), current)
		if err != nil || !expected[rel] {
			return fmt.Errorf("unexpected extracted asset %q", rel)
		}
		return nil
	})
}

// RemovePackage thaws a service-owned immutable tree before deletion.
func RemovePackage(root string) error {
	if err := filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return os.Chmod(current, 0o755)
		}
		return os.Chmod(current, 0o644)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(root)
}

// VerifyLayout checks that every descriptor target exists with the exact
// digest and size the descriptor promises.
func VerifyLayout(dir string) error {
	layoutRaw, err := os.ReadFile(path.Join(dir, "oci-layout"))
	if err != nil || strings.TrimSpace(string(layoutRaw)) != `{"imageLayoutVersion":"1.0.0"}` {
		return fmt.Errorf("invalid oci-layout")
	}
	idxRaw, err := os.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		return fmt.Errorf("layout index: %w", err)
	}
	if len(idxRaw) > MaxConfigBytes {
		return fmt.Errorf("layout index exceeds size limit")
	}
	var idx ociIndex
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return fmt.Errorf("layout index: %w", err)
	}
	if len(idx.Blobs) != 1 {
		return fmt.Errorf("layout index must hold exactly one recipe manifest")
	}
	if err := readBlob(dir, idx.Blobs[0]); err != nil {
		return err
	}
	manifestRaw, err := os.ReadFile(path.Join(dir, "blobs", "sha256", strings.TrimPrefix(idx.Blobs[0].Digest, "sha256:")))
	if err != nil {
		return err
	}
	var man ociManifest
	if err := json.Unmarshal(manifestRaw, &man); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if man.ArtifactType != ArtifactType || man.Config.MediaType != ConfigMediaType ||
		len(man.Layers) != 1 || man.Layers[0].MediaType != LayerMediaType {
		return fmt.Errorf("recipe package must contain one config and exactly one asset layer")
	}
	if err := readBlob(dir, man.Config); err != nil {
		return fmt.Errorf("config blob: %w", err)
	}
	if err := readBlob(dir, man.Layers[0]); err != nil {
		return fmt.Errorf("layer blob: %w", err)
	}
	return nil
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// readBlob verifies one bounded blob against its descriptor.
func readBlob(dir string, d ociDescriptor) error {
	if !sha256DigestRE.MatchString(d.Digest) {
		return fmt.Errorf("invalid blob digest %q", d.Digest)
	}
	max := int64(MaxConfigBytes)
	if d.MediaType == LayerMediaType {
		max = MaxCompressedLayerBytes
	}
	if d.Size < 0 || d.Size > max {
		return fmt.Errorf("blob %s exceeds size limit", d.Digest)
	}
	p := path.Join(dir, "blobs", "sha256", d.Digest[len("sha256:"):])
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	if info.Size() > max || info.Size() != d.Size {
		return fmt.Errorf("blob %s size mismatch", d.Digest)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != d.Digest {
		return fmt.Errorf("blob %s digest mismatch", d.Digest)
	}
	return nil
}

// PackFromDir reads a recipe directory (recipe.yaml + declared assets),
// validates it, and returns the pack result.
func PackFromDir(dir string, v *Validator) (manifest *Manifest, res *PackResult, err error) {
	return packFromDir(dir, v, nil)
}

// PackRepositoryDir packages a native repository bundle while pinning its
// canonical metadata.source to the exact checked-out commit.
func PackRepositoryDir(dir string, v *Validator, source Source) (manifest *Manifest, res *PackResult, err error) {
	return packFromDir(dir, v, &source)
}

func packFromDir(dir string, v *Validator, source *Source) (manifest *Manifest, res *PackResult, err error) {
	raw, err := os.ReadFile(path.Join(dir, "recipe.yaml"))
	if os.IsNotExist(err) {
		raw, err = os.ReadFile(path.Join(dir, "recipe.json"))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("recipe document: %w", err)
	}
	doc, err := YAMLOrJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	m, diags, err := v.ValidateStrict(doc)
	if err != nil {
		return nil, nil, err
	}
	if len(diags) > 0 {
		return nil, nil, fmt.Errorf("recipe invalid: %v", diags)
	}
	if source != nil {
		m.Metadata.Source = source
		doc, err = json.Marshal(m)
		if err != nil {
			return nil, nil, err
		}
		if _, diags, err = v.ValidateStrict(doc); err != nil {
			return nil, nil, err
		}
		if len(diags) > 0 {
			return nil, nil, fmt.Errorf("recipe invalid: %v", diags)
		}
	}
	assets := map[string][]byte{}
	for _, a := range m.Assets {
		data, err := loadAsset(dir, a)
		if err != nil {
			return nil, nil, err
		}
		assets[a] = data
	}
	ann := map[string]string{
		"dev.localmodelworks.recipe.name":    m.Metadata.Name,
		"dev.localmodelworks.recipe.version": m.Metadata.Version,
	}
	if m.Metadata.Source != nil {
		ann["dev.localmodelworks.source.url"] = m.Metadata.Source.URL
		ann["dev.localmodelworks.source.revision"] = m.Metadata.Source.Revision
		ann["dev.localmodelworks.source.path"] = m.Metadata.Source.Path
	}
	res, err = PackManifest(doc, assets, ann)
	if err != nil {
		return nil, nil, err
	}
	return m, res, nil
}

// loadAsset reads one declared asset. The path is validated before any
// filesystem access (canonical, relative, no .. segments), then walked
// component by component from the package root: every intermediate
// component and the final entry must be a non-symlink, and the final entry
// a regular file. This stops both `..` traversal and an intermediate
// symlink from aliasing an asset to a file outside the package root.
func loadAsset(dir, a string) ([]byte, error) {
	if a == "" || strings.HasPrefix(a, "/") {
		return nil, packErr("recipe.asset-absolute", a, "path must be a non-empty relative path")
	}
	if path.Clean(a) != a {
		return nil, packErr("recipe.asset-path", a, "path is not canonical")
	}
	segs := strings.Split(a, "/")
	for _, seg := range segs {
		if seg == ".." {
			return nil, packErr("recipe.asset-traversal", a, "path contains a .. segment")
		}
	}
	p := dir
	var st os.FileInfo
	for i, seg := range segs {
		p = path.Join(p, seg)
		var err error
		st, err = os.Lstat(p)
		if err != nil {
			return nil, packErr("recipe.asset-missing", a, err.Error())
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, packErr("recipe.asset-symlink", a, fmt.Sprintf("symlink not allowed at %q", seg))
		}
		if i < len(segs)-1 && !st.IsDir() {
			return nil, packErr("recipe.asset-path", a, fmt.Sprintf("%q is not a directory", seg))
		}
	}
	if !st.Mode().IsRegular() {
		return nil, packErr("recipe.asset-notregular", a, "not a regular file")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, packErr("recipe.asset-missing", a, err.Error())
	}
	return data, nil
}

// ReadLayoutDigest returns the manifest digest recorded in an OCI layout dir.
func ReadLayoutDigest(dir string) (string, error) {
	idxRaw, err := os.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		return "", err
	}
	var idx ociIndex
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		return "", err
	}
	if len(idx.Blobs) != 1 {
		return "", fmt.Errorf("layout index must hold exactly one recipe manifest")
	}
	return idx.Blobs[0].Digest, nil
}

// UnpackLayer extracts a recipe asset layer (tar.gz) into dest, preserving
// file modes and rejecting entries that escape dest.
func UnpackLayer(layer []byte, dest string) error {
	if len(layer) > MaxCompressedLayerBytes {
		return packErr("recipe.layer_too_large", "", "compressed asset layer exceeds 64 MiB")
	}
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean(hdr.Name)
		if clean == "." || clean != hdr.Name || clean == ".." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("layer entry %q escapes destination", hdr.Name)
		}
		full := path.Join(dest, clean)
		switch hdr.Typeflag {
		case tar.TypeReg:
			files++
			if files > MaxAssetFiles {
				return packErr("recipe.too_many_assets", "", "asset count exceeds 256")
			}
			if hdr.Size < 0 || hdr.Size > MaxAssetFileBytes {
				return packErr("recipe.asset_too_large", hdr.Name, "asset exceeds 16 MiB")
			}
			total += hdr.Size
			if total > MaxExtractedAssetBytes {
				return packErr("recipe.assets_too_large", "", "assets exceed 128 MiB")
			}
			if err := os.MkdirAll(path.Dir(full), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, hdr.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		default:
			return fmt.Errorf("layer entry %q: unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
}
