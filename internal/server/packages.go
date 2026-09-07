package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func (s *Server) authorizePackageRead(response http.ResponseWriter, request *http.Request) bool {
	peer := peerCertFrom(request.Context())
	if peer == nil || len(peer.DNSNames) == 0 {
		writeErr(response, http.StatusUnauthorized, "agent.unauthorized", "enrolled node certificate required")
		return false
	}
	authorized := false
	for _, nodeID := range peer.DNSNames {
		if _, err := s.q.GetNode(request.Context(), nodeID); err == nil {
			authorized = true
			break
		}
	}
	if !authorized {
		writeErr(response, http.StatusForbidden, "agent.unauthorized", "node identity is not enrolled")
		return false
	}
	return true
}

func (s *Server) handlePackageLayer(response http.ResponseWriter, request *http.Request) {
	if !s.authorizePackageRead(response, request) {
		return
	}
	manifestDigest := chi.URLParam(request, "digest")
	layer, layerDigest, err := recipe.ReadPackageLayer(s.cfg.RecipeRoot(), manifestDigest)
	if err != nil {
		writeErr(response, http.StatusNotFound, "recipe.package_not_found", err.Error())
		return
	}
	response.Header().Set("Content-Type", recipe.LayerMediaType)
	response.Header().Set("X-LMW-Layer-Digest", layerDigest)
	response.Header().Set("Content-Length", strconv.Itoa(len(layer)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(layer)
}

// Metadata is verified independently so acquiring the manifest does not read
// or duplicate the potentially large layer before the agent requests it.
func (s *Server) handlePackageMetadata(response http.ResponseWriter, request *http.Request) {
	if !s.authorizePackageRead(response, request) {
		return
	}
	digest := chi.URLParam(request, "digest")
	if !validPackageDigest(digest) {
		writeErr(response, http.StatusBadRequest, "recipe.package_digest", "invalid package digest")
		return
	}
	root := filepath.Join(s.cfg.RecipeRoot(), strings.TrimPrefix(digest, "sha256:"))
	data, err := readPackageMetadataBlob(root, digest)
	if err == nil && chi.URLParam(request, "part") == "config" {
		var manifest struct {
			Config struct {
				Digest string `json:"digest"`
				Size   int64  `json:"size"`
			} `json:"config"`
		}
		if err = json.Unmarshal(data, &manifest); err == nil {
			digest = manifest.Config.Digest
			data, err = readPackageMetadataBlob(root, digest)
			if err == nil && int64(len(data)) != manifest.Config.Size {
				err = fmt.Errorf("package config size mismatch")
			}
		}
	}
	if err != nil {
		writeErr(response, http.StatusNotFound, "recipe.package_not_found", "verified package metadata is unavailable")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-LMW-Blob-Digest", digest)
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}

func validPackageDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return false
	}
	_, err := hex.DecodeString(digest[7:])
	return err == nil && digest == strings.ToLower(digest)
}

func readPackageMetadataBlob(root, digest string) ([]byte, error) {
	if !validPackageDigest(digest) {
		return nil, fmt.Errorf("invalid package blob digest")
	}
	file, err := os.Open(filepath.Join(root, "blobs", "sha256", digest[7:]))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxMetadataBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		return nil, fmt.Errorf("package metadata integrity check failed")
	}
	return data, nil
}
