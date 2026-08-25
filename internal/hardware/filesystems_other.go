//go:build !linux

package hardware

// SampleFilesystems is unsupported off-Linux; agents running there report no
// storage telemetry (the workstation test build compiles against this stub).
func SampleFilesystems(paths []string) []FilesystemTelemetry {
	return nil
}
