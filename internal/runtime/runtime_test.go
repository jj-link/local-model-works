package runtime

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/registry"
)

func TestImageRef(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name string
		spec ContainerSpec
		want string
	}{
		{"no digest passes through", ContainerSpec{Image: "ghcr.io/x/y:v1"}, "ghcr.io/x/y:v1"},
		{"digest appended to tag", ContainerSpec{Image: "ghcr.io/x/y:v1", ImageDigest: digestA}, "ghcr.io/x/y:v1@" + digestA},
		{"pinned digest replaces embedded digest", ContainerSpec{Image: "ghcr.io/x/y@" + digestB, ImageDigest: digestA}, "ghcr.io/x/y@" + digestA},
		{"pinned digest matches embedded", ContainerSpec{Image: "ghcr.io/x/y@" + digestA, ImageDigest: digestA}, "ghcr.io/x/y@" + digestA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageRef(&tc.spec); got != tc.want {
				t.Fatalf("imageRef = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCapDropsDefault(t *testing.T) {
	if got := capDrops(nil); len(got) != 1 || got[0] != "ALL" {
		t.Fatalf("capDrops(nil) = %v, want [ALL]", got)
	}
	if got := capDrops([]string{"NET_RAW"}); len(got) != 1 || got[0] != "NET_RAW" {
		t.Fatalf("capDrops passthrough = %v, want [NET_RAW]", got)
	}
}

func TestRegistryAuthEncoding(t *testing.T) {
	enc, err := encodeRegistryAuth(&Auth{Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("encodeRegistryAuth: %v", err)
	}
	cfg, err := registry.DecodeAuthConfig(enc)
	if err != nil {
		t.Fatalf("encoded auth is not a decodable authconfig: %v", err)
	}
	if cfg.Username != "u" || cfg.Password != "p" {
		t.Fatalf("decoded auth = %+v, want u/p", cfg)
	}
}
