package backend

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/id"
)

const maxSourcePDF = 50 << 20

var (
	arxivIDPattern = regexp.MustCompile(`(?i)^(?:https://arxiv\.org/(?:abs|pdf)/)?([a-z-]+(?:\.[A-Z]{2})?/\d{7}|\d{4}\.\d{4,5})(?:v\d+)?(?:\.pdf)?$`)
	titlePattern   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

type sourceResolution struct {
	Locator  string
	Title    string
	Metadata map[string]any
	Status   string
	Error    string
}

func isPublicIP(ip netip.Addr) bool {
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

func publicAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if parsed, err := netip.ParseAddr(host); err == nil {
		if !isPublicIP(parsed.Unmap()) {
			return nil, errors.New("autoresearch.source_private_address")
		}
		return []netip.Addr{parsed.Unmap()}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve source host: %w", err)
	}
	public := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !isPublicIP(address) {
			return nil, errors.New("autoresearch.source_private_address")
		}
		public = append(public, address)
	}
	if len(public) == 0 {
		return nil, errors.New("autoresearch.source_unresolved")
	}
	return public, nil
}

func validatePublicHTTPS(ctx context.Context, target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil {
		return errors.New("autoresearch.source_https_required")
	}
	_, err := publicAddresses(ctx, target.Hostname())
	return err
}

func safeSourceClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := publicAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, candidate := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
			if err == nil {
				return connection, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return errors.New("autoresearch.source_redirect_limit")
			}
			return validatePublicHTTPS(req.Context(), req.URL)
		},
	}
}

func extractHTMLTitle(body []byte) string {
	match := titlePattern.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return strings.Join(strings.Fields(html.UnescapeString(string(match[1]))), " ")
}

func fetchPublicSource(ctx context.Context, target *url.URL, accept string) (*http.Response, []byte, error) {
	if err := validatePublicHTTPS(ctx, target); err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("User-Agent", "LocalModelWorks-AutoResearch/1")
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response, err := safeSourceClient().Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return response, nil, err
	}
	return response, body, nil
}

func resolveSource(ctx context.Context, kind AutoResearchSourceKind, locator string) sourceResolution {
	locator = strings.TrimSpace(locator)
	result := sourceResolution{Locator: locator, Metadata: map[string]any{}, Status: "failed"}
	switch string(kind) {
	case "arxiv":
		match := arxivIDPattern.FindStringSubmatch(locator)
		if len(match) != 2 {
			result.Error = "autoresearch.source_arxiv_invalid"
			return result
		}
		arxivID := match[1]
		result.Locator = "https://arxiv.org/abs/" + arxivID
		result.Metadata = map[string]any{"arxiv_id": arxivID, "canonical_url": result.Locator}
		result.Status = "ready"
		return result
	case "doi":
		doi := strings.TrimPrefix(strings.TrimPrefix(locator, "https://doi.org/"), "http://doi.org/")
		doi = strings.TrimSpace(doi)
		if doi == "" || strings.ContainsAny(doi, " \t\r\n") {
			result.Error = "autoresearch.source_doi_invalid"
			return result
		}
		target, _ := url.Parse("https://doi.org/" + doi)
		response, body, err := fetchPublicSource(ctx, target, "application/vnd.citationstyles.csl+json, text/html;q=0.8")
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Locator = "https://doi.org/" + doi
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnavailableForLegalReasons {
			result.Status, result.Error = "blocked", "autoresearch.source_access_blocked"
			return result
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			result.Error = fmt.Sprintf("autoresearch.source_http_%d", response.StatusCode)
			return result
		}
		if strings.Contains(response.Header.Get("Content-Type"), "json") {
			_ = json.Unmarshal(body, &result.Metadata)
			if title, ok := result.Metadata["title"].(string); ok {
				result.Title = title
			}
		} else {
			result.Title = extractHTMLTitle(body)
		}
		result.Metadata["doi"] = doi
		result.Status = "ready"
		return result
	case "url":
		target, err := url.Parse(locator)
		if err != nil {
			result.Error = "autoresearch.source_url_invalid"
			return result
		}
		response, body, err := fetchPublicSource(ctx, target, "text/html, application/pdf;q=0.8, text/plain;q=0.5")
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Locator = response.Request.URL.String()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnavailableForLegalReasons {
			result.Status, result.Error = "blocked", "autoresearch.source_access_blocked"
			return result
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			result.Error = fmt.Sprintf("autoresearch.source_http_%d", response.StatusCode)
			return result
		}
		result.Title = extractHTMLTitle(body)
		result.Metadata = map[string]any{"canonical_url": result.Locator, "content_type": response.Header.Get("Content-Type")}
		result.Status = "ready"
		return result
	default:
		result.Error = "autoresearch.source_kind_invalid"
		return result
	}
}

func (m *Module) sourceView(row db.AutoresearchSource) (AutoResearchSource, error) {
	idValue, err := uuidValue(row.ID)
	if err != nil {
		return AutoResearchSource{}, err
	}
	projectID, err := uuidValue(row.ProjectID)
	if err != nil {
		return AutoResearchSource{}, err
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(row.MetadataJson), &metadata); err != nil {
		return AutoResearchSource{}, err
	}
	return AutoResearchSource{
		Id: idValue, ProjectId: projectID, Kind: AutoResearchSourceKind(row.Kind), Locator: row.Locator,
		Title: ptrString(row.Title), Metadata: metadata, Sha256: ptrString(row.Sha256),
		Status: AutoResearchSourceStatus(row.Status), Error: ptrString(row.Error), CreatedAt: parseDBTime(row.CreatedAt),
	}, nil
}

func (m *Module) ListAutoResearchSources(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	rows, err := m.env.Q.ListAutoResearchSources(r.Context(), projectID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]AutoResearchSource, 0, len(rows))
	for _, row := range rows {
		view, err := m.sourceView(row)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		out = append(out, view)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) CreateAutoResearchSource(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	if _, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	var req AutoResearchSourceCreate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if req.Kind == "pdf" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "PDF sources use source-files")
		return
	}
	resolved := resolveSource(r.Context(), req.Kind, req.Locator)
	metadata, _ := json.Marshal(resolved.Metadata)
	sourceID, err := id.New()
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if err := m.env.Q.CreateAutoResearchSource(r.Context(), db.CreateAutoResearchSourceParams{
		ID: sourceID, ProjectID: projectID.String(), Kind: string(req.Kind), Locator: resolved.Locator,
		Title: sql.NullString{String: resolved.Title, Valid: resolved.Title != ""}, MetadataJson: string(metadata),
		Status: resolved.Status, Error: sql.NullString{String: resolved.Error, Valid: resolved.Error != ""},
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	row, err := m.env.Q.GetAutoResearchSource(r.Context(), db.GetAutoResearchSourceParams{ID: sourceID, ProjectID: projectID.String()})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view, err := m.sourceView(row)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func copyPDF(file multipart.File, destination string) (string, int64, error) {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upload-*.pdf")
	if err != nil {
		return "", 0, err
	}
	name := temporary.Name()
	defer func() {
		temporary.Close()
		_ = os.Remove(name)
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(file, maxSourcePDF+1))
	if err != nil {
		return "", written, err
	}
	if written > maxSourcePDF {
		return "", written, errors.New("autoresearch.source_pdf_too_large")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return "", written, err
	}
	magic := make([]byte, 5)
	if _, err := io.ReadFull(temporary, magic); err != nil || string(magic) != "%PDF-" {
		return "", written, errors.New("autoresearch.source_pdf_invalid")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	final := filepath.Join(filepath.Dir(destination), digest+".pdf")
	if err := temporary.Close(); err != nil {
		return "", written, err
	}
	if err := os.Rename(name, final); err != nil && !errors.Is(err, os.ErrExist) {
		return "", written, err
	}
	return digest, written, nil
}

func (m *Module) UploadAutoResearchSourceFile(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	if _, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSourcePDF+(1<<20))
	if err := r.ParseMultipartForm(maxSourcePDF + (1 << 20)); err != nil {
		httpx.WriteErr(w, http.StatusRequestEntityTooLarge, "autoresearch.source_pdf_too_large", err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "file is required")
		return
	}
	defer file.Close()
	sourceDir := filepath.Join(m.projectRoot(projectID.String()), ".lmw", "sources")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	digest, _, err := copyPDF(file, filepath.Join(sourceDir, "upload.pdf"))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if err.Error() == "autoresearch.source_pdf_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		httpx.WriteErr(w, status, err.Error(), err.Error())
		return
	}
	sourceID, err := id.New()
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	localPath := filepath.Join(".lmw", "sources", digest+".pdf")
	metadata, _ := json.Marshal(map[string]any{"sha256": digest})
	if err := m.env.Q.CreateAutoResearchSource(r.Context(), db.CreateAutoResearchSourceParams{
		ID: sourceID, ProjectID: projectID.String(), Kind: "pdf", Locator: "sha256:" + digest,
		MetadataJson: string(metadata), LocalPath: sql.NullString{String: localPath, Valid: true},
		Sha256: sql.NullString{String: digest, Valid: true}, Status: "ready",
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	row, err := m.env.Q.GetAutoResearchSource(r.Context(), db.GetAutoResearchSourceParams{ID: sourceID, ProjectID: projectID.String()})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view, err := m.sourceView(row)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}
