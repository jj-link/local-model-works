package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/parquet-go/parquet-go"
)

const (
	openHandsCommit = "e644a2ca45c3623b27a7e6c169e3d479f0a87fbc"
	sweBenchCommit  = "242429c188fcfd06aad13fce9a54d450470bf0ac"
	fullRevision    = "bb94ed9e39bbeb96a7fcbfb533b80f25a7fd59cb"
	liteRevision    = "f70b1a29ab120eb0a0ee7a1deb029825e735b2b0"
	fullLFS         = "60569cea74bb281f7a5579467436a2bc1932c6e0c5f2f7fa0d084392abd9ad97"
	liteLFS         = "f3a7cd934e8cc523b6053298d0abb2c82fd7db2b83f9f2ccba5944545aaa4eb1"
)

type datasetSpec struct {
	Name     string
	Revision string
	LFS      string
	Count    int
}

var datasets = map[string]datasetSpec{
	"full": {Name: "SWE-Gym/SWE-Gym", Revision: fullRevision, LFS: fullLFS, Count: 2438},
	"lite": {Name: "SWE-Gym/SWE-Gym-Lite", Revision: liteRevision, LFS: liteLFS, Count: 230},
}

type sweGymRow struct {
	InstanceID       string   `parquet:"instance_id"`
	HintsText        string   `parquet:"hints_text"`
	Patch            string   `parquet:"patch"`
	TestPatch        string   `parquet:"test_patch"`
	CreatedAt        string   `parquet:"created_at"`
	ProblemStatement string   `parquet:"problem_statement"`
	Repo             string   `parquet:"repo"`
	BaseCommit       string   `parquet:"base_commit"`
	Version          string   `parquet:"version"`
	PassToPass       []string `parquet:"PASS_TO_PASS,list"`
	FailToPass       []string `parquet:"FAIL_TO_PASS,list"`
}

type sweGymTask struct {
	InstanceID       string   `json:"instance_id"`
	Repo             string   `json:"repo"`
	BaseCommit       string   `json:"base_commit"`
	Version          string   `json:"version,omitempty"`
	ProblemStatement string   `json:"problem_statement"`
	FailToPass       []string `json:"fail_to_pass"`
	PassToPass       []string `json:"pass_to_pass"`
	TestPatch        string   `json:"test_patch"`
	Image            string   `json:"image"`
	ImageDigest      string   `json:"image_digest"`
}

type datasetCache struct {
	root   string
	client *http.Client
	mu     sync.Mutex
}

func newDatasetCache(root string) *datasetCache {
	return &datasetCache{root: filepath.Join(root, "coding-traces", "datasets"), client: &http.Client{Timeout: 30 * time.Minute}}
}

func (c *datasetCache) rows(ctx context.Context, dataset string) ([]sweGymRow, datasetSpec, error) {
	spec, ok := datasets[dataset]
	if !ok {
		return nil, datasetSpec{}, fmt.Errorf("unsupported dataset %q", dataset)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		return nil, datasetSpec{}, err
	}
	path := filepath.Join(c.root, spec.Revision+".parquet")
	if err := verifyFile(path, spec.LFS); err != nil {
		if !os.IsNotExist(err) {
			_ = os.Remove(path)
		}
		if err := c.download(ctx, spec, path); err != nil {
			return nil, datasetSpec{}, err
		}
	}
	rows, err := parquet.ReadFile[sweGymRow](path)
	if err != nil {
		return nil, datasetSpec{}, fmt.Errorf("read pinned dataset: %w", err)
	}
	if len(rows) != spec.Count {
		return nil, datasetSpec{}, fmt.Errorf("pinned dataset row count %d, want %d", len(rows), spec.Count)
	}
	return rows, spec, nil
}

func (c *datasetCache) download(ctx context.Context, spec datasetSpec, path string) error {
	url := fmt.Sprintf("https://huggingface.co/datasets/%s/resolve/%s/data/train-00000-of-00001.parquet", spec.Name, spec.Revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("download pinned dataset: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download pinned dataset: HTTP %d", res.StatusCode)
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), res.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != spec.LFS {
		_ = os.Remove(tmp)
		return fmt.Errorf("pinned dataset digest %s, want %s", got, spec.LFS)
	}
	return os.Rename(tmp, path)
}

func verifyFile(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("dataset digest mismatch")
	}
	return nil
}

func filterRows(rows []sweGymRow, taskIDs, repositories []string, limit int) ([]sweGymRow, error) {
	tasks := stringSet(taskIDs)
	repos := stringSet(repositories)
	selected := make([]sweGymRow, 0, len(rows))
	seenTasks := map[string]bool{}
	for _, row := range rows {
		if len(tasks) > 0 && !tasks[row.InstanceID] {
			continue
		}
		if len(repos) > 0 && !repos[row.Repo] {
			continue
		}
		selected = append(selected, row)
		seenTasks[row.InstanceID] = true
	}
	for task := range tasks {
		if !seenTasks[task] {
			return nil, fmt.Errorf("task %s is not in the pinned dataset", task)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].InstanceID < selected[j].InstanceID })
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("filters select no tasks")
	}
	return selected, nil
}

func resolveTasks(ctx context.Context, rows []sweGymRow, imagePrefix string) ([]sweGymTask, error) {
	if imagePrefix == "" {
		imagePrefix = "docker.io/xingyaoww"
	}
	imagePrefix = strings.TrimRight(imagePrefix, "/")
	tasks := make([]sweGymTask, len(rows))
	type result struct {
		index  int
		digest string
		err    error
	}
	results := make(chan result, len(rows))
	sem := make(chan struct{}, 16)
	for i, row := range rows {
		imageID := strings.ToLower(strings.ReplaceAll(row.InstanceID, "__", "_s_"))
		image := imagePrefix + "/sweb.eval.x86_64." + imageID + ":latest"
		tasks[i] = sweGymTask{InstanceID: row.InstanceID, Repo: row.Repo, BaseCommit: row.BaseCommit, Version: row.Version,
			ProblemStatement: row.ProblemStatement, FailToPass: row.FailToPass, PassToPass: row.PassToPass,
			TestPatch: row.TestPatch, Image: image}
		go func(index int, refText string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			ref, err := name.ParseReference(refText)
			if err != nil {
				results <- result{index: index, err: err}
				return
			}
			descriptor, err := remote.Head(ref, remote.WithContext(ctx))
			if err != nil {
				results <- result{index: index, err: err}
				return
			}
			results <- result{index: index, digest: descriptor.Digest.String()}
		}(i, image)
	}
	for range rows {
		resolved := <-results
		if resolved.err != nil {
			return nil, fmt.Errorf("resolve image for %s: %w", tasks[resolved.index].InstanceID, resolved.err)
		}
		tasks[resolved.index].ImageDigest = resolved.digest
		tasks[resolved.index].Image = strings.TrimSuffix(tasks[resolved.index].Image, ":latest") + "@" + resolved.digest
	}
	return tasks, nil
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out[value] = true
		}
	}
	return out
}
