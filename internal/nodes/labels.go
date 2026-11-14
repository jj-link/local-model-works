package nodes

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ServePortLabel pins the host port single-node deployments on a node serve on.
const ServePortLabel = "serve-port"

// ServePort returns the node's serve-port label value. ok is false when the
// label is absent; a present label that is not a port 1-65535 is an error.
func ServePort(labels map[string]string) (port int32, ok bool, err error) {
	value, present := labels[ServePortLabel]
	if !present {
		return 0, false, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > 65535 || strconv.Itoa(parsed) != value {
		return 0, false, fmt.Errorf("label %s must be a port number 1-65535, got %q", ServePortLabel, value)
	}
	return int32(parsed), true, nil
}

// LabelServePort reads ServePort from a stored nodes.labels JSON document.
// Malformed documents and invalid values carry no port.
func LabelServePort(raw string) (int32, bool) {
	var labels map[string]string
	if json.Unmarshal([]byte(raw), &labels) != nil {
		return 0, false
	}
	port, ok, err := ServePort(labels)
	return port, ok && err == nil
}
