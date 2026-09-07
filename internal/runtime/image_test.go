package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

type imageTransport func(*http.Request) (*http.Response, error)

func (f imageTransport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestInspectImageRejectsWrongPlatformAndDigestWithoutPull(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	actualDigest := digest
	platform := "amd64"
	cli, err := client.NewClientWithOpts(client.WithHost("http://engine.invalid"), client.WithVersion("1.47"), client.WithHTTPClient(&http.Client{Transport: imageTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || !strings.Contains(request.URL.Path, "/images/") || !strings.HasSuffix(request.URL.Path, "/json") {
			t.Fatalf("inspection tried to mutate engine: %s %s", request.Method, request.URL.Path)
		}
		raw, _ := json.Marshal(map[string]any{"Id": "sha256:" + strings.Repeat("b", 64), "RepoDigests": []string{"busybox@" + actualDigest}, "Os": "linux", "Architecture": platform, "Size": 123})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	rt := &dockerRuntime{cli: cli}
	info, err := rt.InspectImage(t.Context(), "docker.io/library/busybox:stable@"+digest, "linux/amd64")
	if err != nil || info.Digest != digest {
		t.Fatalf("exact local image rejected: %+v %v", info, err)
	}
	platform = "arm64"
	if _, err := rt.InspectImage(t.Context(), "docker.io/library/busybox@"+digest, "linux/amd64"); err == nil {
		t.Fatal("wrong platform was certified available")
	}
	platform = "amd64"
	actualDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := rt.InspectImage(t.Context(), "docker.io/library/busybox@"+digest, "linux/amd64"); err == nil {
		t.Fatal("different installed digest silently substituted")
	}
}

func TestInspectIndexDiscoversOnlyAvailablePlatformManifest(t *testing.T) {
	index := "sha256:" + strings.Repeat("a", 64)
	manifest := "sha256:" + strings.Repeat("b", 64)
	available := true
	cli, err := client.NewClientWithOpts(client.WithHost("http://engine.invalid"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: imageTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("image discovery mutated engine: %s", request.Method)
		}
		payload := map[string]any{"Id": "sha256:" + strings.Repeat("c", 64), "RepoDigests": []string{"example.com/image@" + index}, "Os": "linux", "Architecture": "amd64", "Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": index}}
		if request.URL.Query().Get("manifests") == "1" {
			payload["Manifests"] = []map[string]any{{"ID": manifest, "Kind": "image", "Available": available, "Descriptor": map[string]any{"digest": manifest}, "ImageData": map[string]any{"Platform": map[string]any{"os": "linux", "architecture": "amd64"}}, "Size": map[string]any{"Total": 123}}}
		}
		raw, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	rt := &dockerRuntime{cli: cli}
	info, err := rt.InspectImage(t.Context(), "example.com/image@"+index, "linux/amd64")
	if err != nil || info.IndexDigest != index || info.ManifestDigest != manifest {
		t.Fatalf("exact native pair not discovered: %+v %v", info, err)
	}
	available = false
	if _, err := rt.InspectImage(t.Context(), "example.com/image@"+index, "linux/amd64"); err == nil {
		t.Fatal("index metadata certified unavailable child layers")
	}
}
