package hardware

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stddef.h>

// NVML nvmlProcessInfo_t, shared by the v2 and v3 process APIs.
typedef struct {
    unsigned int pid;
    unsigned long long usedGpuMemory;
    unsigned int gpuInstanceId;
    unsigned int computeInstanceId;
} lmw_process_info;

typedef int (*lmw_handle_fn)(unsigned int, void **);
typedef int (*lmw_processes_fn)(void *, unsigned int *, lmw_process_info *);
static lmw_handle_fn lmw_handle;
static lmw_processes_fn lmw_processes;

// Called once after go-nvml has initialized NVML. Keep our dlopen reference
// for the lifetime of these function pointers; NVML Init/Shutdown stays owned
// by go-nvml.
static int lmw_load_process_symbols(void) {
    void *lib = dlopen("libnvidia-ml.so.1", RTLD_LAZY | RTLD_LOCAL);
    if (!lib) return 0;
    lmw_handle = (lmw_handle_fn)dlsym(lib, "nvmlDeviceGetHandleByIndex_v2");
    lmw_processes = (lmw_processes_fn)dlsym(lib, "nvmlDeviceGetComputeRunningProcesses_v3");
    if (!lmw_handle || !lmw_processes) {
        lmw_handle = NULL;
        lmw_processes = NULL;
        dlclose(lib);
        return 0;
    }
    return 1;
}

static int lmw_query_processes(unsigned int index, unsigned int *count, lmw_process_info *infos) {
    void *device = NULL;
    int ret = lmw_handle(index, &device);
    if (ret != 0) return ret;
    return lmw_processes(device, count, infos);
}

enum {
    lmw_process_size = sizeof(lmw_process_info),
    lmw_pid_offset = offsetof(lmw_process_info, pid),
    lmw_memory_offset = offsetof(lmw_process_info, usedGpuMemory),
    lmw_gpu_instance_offset = offsetof(lmw_process_info, gpuInstanceId),
    lmw_compute_instance_offset = offsetof(lmw_process_info, computeInstanceId)
};
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// Fail compilation if a dependency update changes the Go/C ABI agreement.
var (
	_ [unsafe.Sizeof(nvml.ProcessInfo{}) - C.lmw_process_size]byte
	_ [C.lmw_process_size - unsafe.Sizeof(nvml.ProcessInfo{})]byte
	_ [unsafe.Offsetof(nvml.ProcessInfo{}.Pid) - C.lmw_pid_offset]byte
	_ [C.lmw_pid_offset - unsafe.Offsetof(nvml.ProcessInfo{}.Pid)]byte
	_ [unsafe.Offsetof(nvml.ProcessInfo{}.UsedGpuMemory) - C.lmw_memory_offset]byte
	_ [C.lmw_memory_offset - unsafe.Offsetof(nvml.ProcessInfo{}.UsedGpuMemory)]byte
	_ [unsafe.Offsetof(nvml.ProcessInfo{}.GpuInstanceId) - C.lmw_gpu_instance_offset]byte
	_ [C.lmw_gpu_instance_offset - unsafe.Offsetof(nvml.ProcessInfo{}.GpuInstanceId)]byte
	_ [unsafe.Offsetof(nvml.ProcessInfo{}.ComputeInstanceId) - C.lmw_compute_instance_offset]byte
	_ [C.lmw_compute_instance_offset - unsafe.Offsetof(nvml.ProcessInfo{}.ComputeInstanceId)]byte
)

var loadProcessSymbols = sync.OnceValue(func() bool {
	return C.lmw_load_process_symbols() != 0
})

func deviceComputeProcesses(dev nvml.Device) ([]nvml.ProcessInfo, nvml.Return) {
	if !loadProcessSymbols() {
		return nil, nvml.ERROR_FUNCTION_NOT_FOUND
	}
	index, rc := dev.GetIndex()
	if rc != nvml.SUCCESS {
		return nil, rc
	}
	return queryComputeProcesses(func(infos []nvml.ProcessInfo) (uint32, nvml.Return) {
		count := C.uint(len(infos))
		rc := C.lmw_query_processes(C.uint(index), &count, (*C.lmw_process_info)(unsafe.Pointer(&infos[0])))
		return uint32(count), nvml.Return(rc)
	})
}

// Start with room for 64 processes rather than go-nvml's single entry. WSL
// drivers can report SUCCESS with a count exceeding the supplied capacity;
// such a result is incomplete, not a slice we may safely return. Genuine
// INSUFFICIENT_SIZE responses still grow the buffer for busy native hosts.
func queryComputeProcesses(query func([]nvml.ProcessInfo) (uint32, nvml.Return)) ([]nvml.ProcessInfo, nvml.Return) {
	infos := make([]nvml.ProcessInfo, 64)
	for {
		count, rc := query(infos)
		if rc == nvml.SUCCESS {
			if uint64(count) > uint64(len(infos)) {
				return nil, nvml.ERROR_INSUFFICIENT_SIZE
			}
			return infos[:count], rc
		}
		if rc != nvml.ERROR_INSUFFICIENT_SIZE || uint64(count) <= uint64(len(infos)) {
			return nil, rc
		}
		infos = make([]nvml.ProcessInfo, count)
	}
}
