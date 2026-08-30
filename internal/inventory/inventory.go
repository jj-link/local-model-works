// Package inventory parses the node inventory as persisted by the agent
// (proto JSON naming) into typed accessors used by fabric validation and
// deployment planning.
package inventory

import "encoding/json"

// Accelerator is one device from the node inventory.
type Accelerator struct {
	Index        int32    `json:"index"`
	Vendor       string   `json:"vendor"`
	Architecture string   `json:"architecture"`
	Name         string   `json:"name"`
	UUID         string   `json:"uuid"`
	MemoryBytes  uint64   `json:"memory_bytes"`
	Features     []string `json:"features"`
}

// Interface is one network interface with its addresses.
type Interface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
}

type RdmaGID struct {
	Index int32  `json:"index"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// RdmaPort is one port of an RDMA device.
type RdmaPort struct {
	Name         string    `json:"name"`
	State        string    `json:"state"`
	LinkRateGbps uint32    `json:"link_rate_gbps"`
	GIDs         []RdmaGID `json:"gids"`
}

type RdmaDevice struct {
	Name              string     `json:"name"`
	Vendor            string     `json:"vendor"`
	NetworkInterfaces []string   `json:"network_interfaces"`
	Ports             []RdmaPort `json:"ports"`
}

type CacheRoot struct {
	Path      string `json:"path"`
	Backend   string `json:"backend,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// Inventory is the parsed node report.
type Inventory struct {
	Hostname     string        `json:"hostname"`
	Accelerators []Accelerator `json:"accelerators"`
	Interfaces   []Interface   `json:"interfaces"`
	RdmaDevices  []RdmaDevice  `json:"rdma_devices"`
	CacheRoots   []CacheRoot   `json:"cache_roots"`
	PeerListen   string        `json:"peer_listen,omitempty"`
}

// Parse decodes a persisted inventory JSON string.
func Parse(raw string) (*Inventory, error) {
	var inv Inventory
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// AcceleratorByUUID finds one accelerator.
func (i *Inventory) AcceleratorByUUID(uuid string) *Accelerator {
	for k := range i.Accelerators {
		if i.Accelerators[k].UUID == uuid {
			return &i.Accelerators[k]
		}
	}
	return nil
}

// AcceleratorByIndex finds one accelerator.
func (i *Inventory) AcceleratorByIndex(index int32) *Accelerator {
	for k := range i.Accelerators {
		if i.Accelerators[k].Index == index {
			return &i.Accelerators[k]
		}
	}
	return nil
}

// HasRdmaDevice reports whether the named device is present.
func (i *Inventory) HasRdmaDevice(name string) bool {
	for k := range i.RdmaDevices {
		if i.RdmaDevices[k].Name == name {
			return true
		}
	}
	return false
}

// RDMADevice returns one named RDMA device.
func (i *Inventory) RDMADevice(name string) *RdmaDevice {
	for index := range i.RdmaDevices {
		if i.RdmaDevices[index].Name == name {
			return &i.RdmaDevices[index]
		}
	}
	return nil
}

// RdmaPort reports the port state of one device.
func (i *Inventory) RdmaPort(device, port string) *RdmaPort {
	for k := range i.RdmaDevices {
		if i.RdmaDevices[k].Name != device {
			continue
		}
		for p := range i.RdmaDevices[k].Ports {
			if i.RdmaDevices[k].Ports[p].Name == port || port == "" {
				return &i.RdmaDevices[k].Ports[p]
			}
		}
	}
	return nil
}

// HasInterface reports whether the named interface is present.
func (i *Inventory) HasInterface(name string) bool {
	for k := range i.Interfaces {
		if i.Interfaces[k].Name == name {
			return true
		}
	}
	return false
}

// InterfaceAddresses returns the addresses of one interface.
func (i *Inventory) InterfaceAddresses(name string) []string {
	for k := range i.Interfaces {
		if i.Interfaces[k].Name == name {
			return i.Interfaces[k].Addresses
		}
	}
	return nil
}
