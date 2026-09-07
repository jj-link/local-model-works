package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/runtime"
)

func redactDownloadError(err error, credential *downloads.CredentialMaterial) error {
	text := strings.ReplaceAll(err.Error(), credential.Value, "[redacted]")
	if credential.Purpose == "registry" {
		var auth runtime.Auth
		if json.Unmarshal([]byte(credential.Value), &auth) == nil {
			for _, value := range []string{auth.Username, auth.Password} {
				if value != "" {
					text = strings.ReplaceAll(text, value, "[redacted]")
				}
			}
		}
	}
	return fmt.Errorf("%s", text)
}
func inspectHTTPSFileSize(ctx context.Context, target string) (*int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return nil, fmt.Errorf("download.source_invalid")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) > 10 || request.URL.Scheme != "https" || request.URL.User != nil {
			return fmt.Errorf("download.redirect_unsafe")
		}
		request.Header.Del("Authorization")
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download.source_metadata_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download.source_metadata_http_%d", response.StatusCode)
	}
	if response.ContentLength < 0 {
		return nil, fmt.Errorf("download.size_unknown")
	}
	size := response.ContentLength
	return &size, nil
}
