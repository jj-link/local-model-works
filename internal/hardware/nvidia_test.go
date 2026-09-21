package hardware

import (
	"context"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
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

func TestQueryComputeProcessesRejectsOversizedSuccess(t *testing.T) {
	rows, rc := queryComputeProcesses(func(infos []nvml.ProcessInfo) (uint32, nvml.Return) {
		// A successful call is not safe to slice when the driver reports
		// more records than the supplied buffer can contain.
		return uint32(len(infos)) + 1, nvml.SUCCESS
	})
	if rows != nil || rc != nvml.ERROR_INSUFFICIENT_SIZE {
		t.Fatalf("oversized success = (%v, %v), want incomplete result", rows, rc)
	}
}

func TestQueryComputeProcessesGrowsForInsufficientSize(t *testing.T) {
	want := make([]nvml.ProcessInfo, 65)
	for i := range want {
		want[i] = nvml.ProcessInfo{Pid: uint32(i + 1), UsedGpuMemory: uint64(i + 1) * 1024}
	}
	calls := 0
	rows, rc := queryComputeProcesses(func(infos []nvml.ProcessInfo) (uint32, nvml.Return) {
		calls++
		if len(infos) < len(want) && calls <= 2 {
			return uint32(len(want)), nvml.ERROR_INSUFFICIENT_SIZE
		}
		if calls > 2 {
			t.Fatal("enumeration failed to converge")
		}
		copy(infos, want)
		return uint32(len(want)), nvml.SUCCESS
	})
	if rc != nvml.SUCCESS || len(rows) != len(want) {
		t.Fatalf("enumeration = (%v, %v), want %v", rows, rc, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("process %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestQueryComputeProcessesEmptyAndErrors(t *testing.T) {
	for _, rc := range []nvml.Return{nvml.SUCCESS, nvml.ERROR_NOT_SUPPORTED, nvml.ERROR_INSUFFICIENT_SIZE} {
		t.Run(rc.String(), func(t *testing.T) {
			calls := 0
			rows, gotRC := queryComputeProcesses(func([]nvml.ProcessInfo) (uint32, nvml.Return) {
				calls++
				if calls > 1 {
					t.Fatal("retried a result without a larger required buffer")
				}
				return 0, rc
			})
			if len(rows) != 0 || gotRC != rc {
				t.Fatalf("empty/error result = (%v, %v), want (empty, %v)", rows, gotRC, rc)
			}
		})
	}
}
