package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func (s *Server) handlePackageLayer(response http.ResponseWriter, request *http.Request) {
	peer := peerCertFrom(request.Context())
	if peer == nil || len(peer.DNSNames) == 0 {
		writeErr(response, http.StatusUnauthorized, "agent.unauthorized", "enrolled node certificate required")
		return
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
