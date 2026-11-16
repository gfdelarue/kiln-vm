package machine

import (
	"fmt"
	"strings"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

// publicAllocationForCleanup returns the immutable allocation recorded for a
// machine. Cleanup diagnostics carry an independent copy so a failed column
// write or a later BridgeCIDR change cannot retarget cleanup.
func publicAllocationForCleanup(machine *models.Machine) (*Allocation, error) {
	if machine == nil {
		return nil, nil
	}
	durable, durablePresent, err := durablePublicAllocationFromMachine(machine)
	if err != nil {
		return nil, err
	}
	var recorded *Allocation
	for _, diagnostic := range machine.Diagnostics {
		if diagnostic.Category != "provider_network" || diagnostic.Phase != "cleanup" || !diagnostic.Retryable || strings.TrimSpace(diagnostic.TAPDevice) == "" {
			continue
		}
		if diagnostic.MachineID != "" && diagnostic.MachineID != machine.ID {
			return nil, fmt.Errorf("network cleanup diagnostic belongs to machine %q, not %q", diagnostic.MachineID, machine.ID)
		}
		candidate, complete, err := allocationFromCleanupDiagnostic(diagnostic)
		if err != nil {
			return nil, err
		}
		if !complete {
			continue
		}
		if strings.TrimSpace(candidate.OwnerID) == "" {
			candidate.OwnerID = strings.TrimSpace(machine.ID)
		}
		if recorded != nil && *recorded != *candidate {
			return nil, fmt.Errorf("network cleanup diagnostics contain conflicting allocations")
		}
		recorded = candidate
	}
	if durablePresent {
		if recorded != nil && !samePublicAllocationIdentity(*recorded, *durable) {
			return nil, fmt.Errorf("network cleanup diagnostic contradicts the recorded allocation for %s", durable.TAPDevice)
		}
		return durable, nil
	}
	if recorded != nil {
		return recorded, nil
	}
	if strings.TrimSpace(machine.TAPDevice) != "" || strings.TrimSpace(machine.IP) != "" || strings.TrimSpace(machine.GatewayIP) != "" {
		return nil, fmt.Errorf("machine %s has network addresses without a recorded allocation", machine.ID)
	}
	return nil, nil
}

func samePublicAllocationIdentity(left, right Allocation) bool {
	left.BaseCIDR = strings.TrimSpace(left.BaseCIDR)
	right.BaseCIDR = strings.TrimSpace(right.BaseCIDR)
	if left.BaseCIDR == "" {
		left.BaseCIDR = right.BaseCIDR
	}
	if right.BaseCIDR == "" {
		right.BaseCIDR = left.BaseCIDR
	}
	left.OwnerID = strings.TrimSpace(left.OwnerID)
	right.OwnerID = strings.TrimSpace(right.OwnerID)
	if left.OwnerID == "" {
		left.OwnerID = right.OwnerID
	}
	if right.OwnerID == "" {
		right.OwnerID = left.OwnerID
	}
	return left == right
}

func setMachinePublicAllocation(machine *models.Machine, alloc *Allocation) {
	if machine == nil || alloc == nil {
		return
	}
	machine.IP = alloc.GuestIP
	machine.GatewayIP = alloc.GatewayIP
	machine.TAPDevice = alloc.TAPDevice
	machine.NetworkCIDR = alloc.BaseCIDR
	machine.NetworkIndex = alloc.NetworkIndex
	machine.GuestMAC = alloc.GuestMAC
	machine.Netmask = alloc.Mask
}

// currentPublicAllocation returns the controller's canonical allocation when a
// durable historical tuple still describes the same TAP, addresses, MAC, and
// index under the current BridgeCIDR. The source CIDR itself is deliberately
// ignored for this comparison: narrowing a range may leave an existing /30
// fully valid, in which case the durable record can be canonicalized without
// needless address churn.
func currentPublicAllocation(alloc *Allocation, bridgeCIDR string) (*Allocation, bool) {
	if alloc == nil {
		return nil, false
	}
	idx, ok := networkIndexFromTAP(strings.TrimSpace(alloc.TAPDevice))
	if !ok || idx != alloc.NetworkIndex {
		return nil, false
	}
	current, err := allocationForIndex(bridgeCIDR, idx)
	if err != nil {
		return nil, false
	}
	current.OwnerID = strings.TrimSpace(alloc.OwnerID)
	candidate := *alloc
	candidate.BaseCIDR = current.BaseCIDR
	candidate.OwnerID = strings.TrimSpace(candidate.OwnerID)
	if candidate != *current {
		return nil, false
	}
	return current, true
}

func durablePublicAllocationFromMachine(machine *models.Machine) (*Allocation, bool, error) {
	if machine == nil {
		return nil, false, nil
	}
	present := strings.TrimSpace(machine.NetworkCIDR) != "" || machine.NetworkIndex != 0 ||
		strings.TrimSpace(machine.GuestMAC) != "" || strings.TrimSpace(machine.Netmask) != ""
	if !present {
		return nil, false, nil
	}
	if strings.TrimSpace(machine.TAPDevice) == "" || strings.TrimSpace(machine.IP) == "" || strings.TrimSpace(machine.GatewayIP) == "" ||
		machine.NetworkIndex == 0 ||
		strings.TrimSpace(machine.GuestMAC) == "" || strings.TrimSpace(machine.Netmask) == "" {
		return nil, false, fmt.Errorf("machine %s has an incomplete durable public network allocation", machine.ID)
	}
	alloc, _, err := validateCleanupAllocation(Allocation{
		TAPDevice:    strings.TrimSpace(machine.TAPDevice),
		GuestIP:      strings.TrimSpace(machine.IP),
		GatewayIP:    strings.TrimSpace(machine.GatewayIP),
		GuestMAC:     strings.TrimSpace(machine.GuestMAC),
		Mask:         strings.TrimSpace(machine.Netmask),
		BaseCIDR:     strings.TrimSpace(machine.NetworkCIDR),
		NetworkIndex: machine.NetworkIndex,
		OwnerID:      strings.TrimSpace(machine.ID),
	})
	if err != nil {
		return nil, false, fmt.Errorf("machine %s durable public network allocation is invalid: %w", machine.ID, err)
	}
	return &alloc, true, nil
}

func allocationFromCleanupDiagnostic(diagnostic models.Diagnostic) (*Allocation, bool, error) {
	newFieldsPresent := strings.TrimSpace(diagnostic.NetworkCIDR) != "" || diagnostic.NetworkIndex != 0 ||
		strings.TrimSpace(diagnostic.GuestIP) != "" || strings.TrimSpace(diagnostic.GatewayIP) != "" ||
		strings.TrimSpace(diagnostic.GuestMAC) != "" || strings.TrimSpace(diagnostic.Netmask) != "" ||
		strings.TrimSpace(diagnostic.TAPAlias) != ""
	if !newFieldsPresent {
		return nil, false, nil
	}
	if strings.TrimSpace(diagnostic.GuestIP) == "" || strings.TrimSpace(diagnostic.GatewayIP) == "" ||
		strings.TrimSpace(diagnostic.GuestMAC) == "" || strings.TrimSpace(diagnostic.Netmask) == "" {
		return nil, false, fmt.Errorf("cleanup diagnostic for TAP %s contains an incomplete immutable allocation", diagnostic.TAPDevice)
	}
	candidate, _, err := validateCleanupAllocation(Allocation{
		TAPDevice:    strings.TrimSpace(diagnostic.TAPDevice),
		GuestIP:      strings.TrimSpace(diagnostic.GuestIP),
		GatewayIP:    strings.TrimSpace(diagnostic.GatewayIP),
		GuestMAC:     strings.TrimSpace(diagnostic.GuestMAC),
		Mask:         strings.TrimSpace(diagnostic.Netmask),
		BaseCIDR:     strings.TrimSpace(diagnostic.NetworkCIDR),
		NetworkIndex: diagnostic.NetworkIndex,
		OwnerID:      strings.TrimSpace(diagnostic.MachineID),
	})
	if err != nil {
		return nil, false, fmt.Errorf("cleanup diagnostic for TAP %s is invalid: %w", diagnostic.TAPDevice, err)
	}
	if alias := strings.TrimSpace(diagnostic.TAPAlias); alias != "" && alias != publicNetworkTAPAlias(candidate) {
		return nil, false, fmt.Errorf("cleanup diagnostic for TAP %s has ownership alias %q, want %q", diagnostic.TAPDevice, alias, publicNetworkTAPAlias(candidate))
	}
	return &candidate, true, nil
}
