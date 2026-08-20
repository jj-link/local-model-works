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
	"regexp"
	"sort"
	"strings"
	"time"
)

// Media types and artifact types for the recipe package format.
const (
	ArtifactType        = "application/vnd.localmodelworks.recipe.v1"
	ConfigMediaType     = "application/vnd.localmodelworks.recipe.config.v1+json"
	LayerMediaType      = "application/vnd.localmodelworks.recipe.assets.v1.tar+gzip"
	CatalogArtifactType = "application/vnd.localmodelworks.catalog.v1"
	CatalogConfigType   = "application/vnd.localmodelworks.catalog.config.v1+json"

	// LabelPrefix marks LMW-managed containers.
	LabelPrefix = "dev.localmodelworks."
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
	LayerDigest    string // empty when the package has no assets
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
	canon, err := Canonical(doc)
	if err != nil {
		return nil, err
	}
	configSum := sha256.Sum256(canon)
	configDigest := "sha256:" + hex.EncodeToString(configSum[:])

	var layerDigest string
	var layerSize int64
	var layerBytes []byte
	if len(assets) > 0 {
		layerBytes, err = buildAssetLayer(assets)
		if err != nil {
			return nil, err
		}
		lsum := sha256.Sum256(layerBytes)
		layerDigest = "sha256:" + hex.EncodeToString(lsum[:])
		layerSize = int64(len(layerBytes))
	}

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
		Annotations:   ann,
	}
	if layerDigest != "" {
		m.Layers = []ociDescriptor{{MediaType: LayerMediaType, Digest: layerDigest, Size: layerSize}}
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
	if res.LayerDigest != "" {
		if err := os.WriteFile(path.Join(blobDir, res.LayerDigest[len("sha256:"):]), res.layerBytes, 0o644); err != nil {
			return err
		}
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

// VerifyLayout checks that every descriptor target exists with the exact
// digest and size the descriptor promises.
func VerifyLayout(dir string) error {
	idxRaw, err := os.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		return fmt.Errorf("layout index: %w", err)
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
	if err := readBlob(dir, man.Config); err != nil {
		return fmt.Errorf("config blob: %w", err)
	}
	for _, l := range man.Layers {
		if err := readBlob(dir, l); err != nil {
			return fmt.Errorf("layer blob: %w", err)
		}
	}
	return nil
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// readBlob verifies one blob against its descriptor. The digest is matched
// against the exact OCI form before it is used as a file name, so a
// malformed or crafted descriptor cannot panic or escape the blob dir.
func readBlob(dir string, d ociDescriptor) error {
	if !sha256DigestRE.MatchString(d.Digest) {
		return fmt.Errorf("invalid blob digest %q", d.Digest)
	}
	p := path.Join(dir, "blobs", "sha256", d.Digest[len("sha256:"):])
	raw, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != d.Digest {
		return fmt.Errorf("blob %s digest mismatch", d.Digest)
	}
	if d.Size > 0 && int64(len(raw)) != d.Size {
		return fmt.Errorf("blob %s size mismatch", d.Digest)
	}
	return nil
}

// PackFromDir reads a recipe directory (recipe.yaml + declared assets),
// validates it, and returns the pack result.
func PackFromDir(dir string, v *Validator) (manifest *Manifest, res *PackResult, err error) {
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
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean(hdr.Name)
		if clean == ".." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("layer entry %q escapes destination", hdr.Name)
		}
		full := path.Join(dest, clean)
		switch hdr.Typeflag {
		case tar.TypeReg:
			if err := os.MkdirAll(path.Dir(full), 0o755); err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(tr, 256<<20))
			if err != nil {
				return err
			}
			if err := os.WriteFile(full, data, 0o555); err != nil {
				return err
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
