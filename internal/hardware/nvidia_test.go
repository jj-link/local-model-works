package hardware

import (
	"context"
	"testing"
)

func TestShutdownNVMLBeforeInit(t *testing.T) {
	// Must not panic and must not prevent a later first initialization.
	ShutdownNVML()
	lib, err := nvmlInit()
	if err != nil {
		return // no NVML driver on this host: clean error, nothing to do
	}
	if lib == nil {
		t.Fatal("nvmlInit succeeded but returned a nil library")
	}
}

func TestFakeDriverValidate(t *testing.T) {
	f := &Fake{Inventory: Inventory{Accelerators: []Accelerator{{
		Index: 0, Vendor: "nvidia", Architecture: "sm_120", MemoryBytes: 32 << 30,
	}}}}

	ok := f.Validate(context.Background(), Requirement{
		Vendor: "nvidia", Architectures: []string{"sm_120"},
		Count: 1, MinMemoryBytes: 16 << 30,
	})
	for _, d := range ok {
		if d.Severity == "error" {
			t.Fatalf("expected no error diagnostics, got %+v", d)
		}
	}

	miss := f.Validate(context.Background(), Requirement{Vendor: "nvidia", Count: 4})
	if len(miss) == 0 || miss[0].Severity != "error" {
		t.Fatalf("expected an error diagnostic for 4 required accelerators, got %+v", miss)
	}
}
