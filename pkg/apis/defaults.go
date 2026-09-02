package apis

import (
	"hash/fnv"
)

// Default applies default values to the NetworkConfig.
func (c *NetworkConfig) Default() {
	c.Interface.Default()
	if c.Interface.Type == InterfaceTypeIPVLAN {
		if c.Interface.IPVlan == nil {
			c.Interface.IPVlan = &IPVlanConfig{}
		}
		c.Interface.IPVlan.Default()
	}
	if c.Interface.VRF != nil {
		c.Interface.VRF.Default()
	}
}

// Default applies default values to the InterfaceConfig.
func (c *InterfaceConfig) Default() {
	// Fold the deprecated DHCP field into Addressing when Addressing is unset.
	if c.Addressing == "" && c.DHCP != nil && *c.DHCP {
		c.Addressing = AddressingModeDHCP
	}
}

// Default applies default values to the VRFConfig.
func (c *VRFConfig) Default() {
	if c.Table == nil && c.Name != "" {
		// Derive a deterministic table ID from the VRF name to ensure interfaces
		// joining the same VRF automatically share the same table ID.
		tableID := TableIDForName(c.Name)
		c.Table = &tableID
	}
}

// TableIDForName derives a deterministic DRANET-managed routing table ID from a
// name (e.g. a VRF name or a device identifier). VRF and policy based routing
// share this scheme so their tables come from the same reserved range.
func TableIDForName(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int((h.Sum32() % 1000) + RouteTableOffset)
}

// Default applies default values to the IPVlanConfig.
func (c *IPVlanConfig) Default() {
	if c.Mode == "" {
		c.Mode = IPVlanModeL2
	}
	if c.Flag == "" {
		c.Flag = IPVlanFlagBridge
	}
}
