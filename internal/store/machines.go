package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const machineColumns = `id, name, backend, guest_os, libvirt_json, image_id, network_policy, state, vcpus, cpu_kind, cpu_millis, memory_mb, vsock_cid, ip, gateway_ip, tap_device, network_cidr, network_index, guest_mac, netmask, error, diagnostics_json, created_at, updated_at`

func (db *DB) CreateMachine(m *models.Machine) error {
	_, err := db.Exec(`INSERT INTO machines (`+machineColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Name, m.Backend, m.GuestOS, libvirtJSON(m.Libvirt), m.ImageID, m.NetworkPolicy, m.State, m.VCPUs, m.CPU.Kind, m.CPU.Millis, m.MemoryMB, m.VSockCID,
		m.IP, m.GatewayIP, m.TAPDevice, m.NetworkCIDR, m.NetworkIndex, m.GuestMAC, m.Netmask, m.Error, diagnosticsJSON(m.Diagnostics), m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert machine: %w", err)
	}
	return nil
}

func (db *DB) GetMachine(id string) (*models.Machine, error) {
	m, err := scanMachine(db.QueryRow(`SELECT `+machineColumns+` FROM machines WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get machine: %w", err)
	}
	return m, nil
}

func (db *DB) ListMachines() ([]models.Machine, error) {
	rows, err := db.Query(`SELECT ` + machineColumns + ` FROM machines ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer rows.Close()

	var machines []models.Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		machines = append(machines, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate machines: %w", err)
	}
	return machines, nil
}

func scanMachine(row scanner) (*models.Machine, error) {
	m := &models.Machine{}
	var diagnostics, libvirtSpec string
	if err := row.Scan(&m.ID, &m.Name, &m.Backend, &m.GuestOS, &libvirtSpec, &m.ImageID, &m.NetworkPolicy, &m.State, &m.VCPUs, &m.CPU.Kind, &m.CPU.Millis, &m.MemoryMB, &m.VSockCID,
		&m.IP, &m.GatewayIP, &m.TAPDevice, &m.NetworkCIDR, &m.NetworkIndex, &m.GuestMAC, &m.Netmask, &m.Error, &diagnostics, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan machine: %w", err)
	}
	m.CPU.VCPUs = m.VCPUs
	if libvirtSpec != "" && libvirtSpec != "{}" {
		m.Libvirt = &models.LibvirtDomainSpec{}
		if err := json.Unmarshal([]byte(libvirtSpec), m.Libvirt); err != nil {
			return nil, fmt.Errorf("decode machine libvirt spec: %w", err)
		}
	}
	if diagnostics != "" {
		if err := json.Unmarshal([]byte(diagnostics), &m.Diagnostics); err != nil {
			return nil, fmt.Errorf("decode machine diagnostics: %w", err)
		}
	}
	return m, nil
}

func (db *DB) UpdateMachineState(id string, state models.MachineState, errMsg string) error {
	return db.UpdateMachineStateWithDiagnostics(id, state, errMsg, nil)
}

func (db *DB) UpdateMachineStateWithDiagnostics(id string, state models.MachineState, errMsg string, diagnostics []models.Diagnostic) error {
	_, err := db.Exec(`
		UPDATE machines SET state = ?, error = ?, diagnostics_json = ?, updated_at = ? WHERE id = ?`,
		state, errMsg, diagnosticsJSON(diagnostics), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update machine state: %w", err)
	}
	return nil
}

func diagnosticsJSON(diagnostics []models.Diagnostic) string {
	if diagnostics == nil {
		diagnostics = []models.Diagnostic{}
	}
	data, err := json.Marshal(diagnostics)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func libvirtJSON(spec *models.LibvirtDomainSpec) string {
	if spec == nil {
		return "{}"
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (db *DB) UpdateMachineNetwork(id, ip, gatewayIP, tapDevice string) error {
	_, err := db.Exec(`
		UPDATE machines SET ip = ?, gateway_ip = ?, tap_device = ?, updated_at = ? WHERE id = ?`,
		ip, gatewayIP, tapDevice, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update machine network: %w", err)
	}
	return nil
}

// UpdateMachineNetworkIntent records the complete allocation and lifecycle
// state before any host network changes are made.
func (db *DB) UpdateMachineNetworkIntent(m *models.Machine) error {
	_, err := db.Exec(`
		UPDATE machines
		SET ip = ?, gateway_ip = ?, tap_device = ?, network_cidr = ?, network_index = ?,
			guest_mac = ?, netmask = ?, state = ?, error = ?, diagnostics_json = ?, updated_at = ?
		WHERE id = ?`,
		m.IP, m.GatewayIP, m.TAPDevice, m.NetworkCIDR, m.NetworkIndex,
		m.GuestMAC, m.Netmask, m.State, m.Error, diagnosticsJSON(m.Diagnostics), time.Now().UTC(), m.ID,
	)
	if err != nil {
		return fmt.Errorf("update machine network intent: %w", err)
	}
	return nil
}

func (db *DB) DeleteMachine(id string) error {
	_, err := db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	return nil
}

func (db *DB) UpdateMachineVSockCID(id string, cid uint32) error {
	_, err := db.Exec(`
		UPDATE machines SET vsock_cid = ?, updated_at = ? WHERE id = ?`,
		cid, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update machine vsock cid: %w", err)
	}
	return nil
}

func (db *DB) MaxAllocatedNetworkIndex() (uint32, error) {
	var idx uint32
	if err := db.QueryRow(`SELECT COALESCE(MAX(network_index), 0) FROM machines`).Scan(&idx); err != nil {
		return 0, fmt.Errorf("max network index: %w", err)
	}
	return idx, nil
}

func (db *DB) MaxAllocatedVSockCID() (uint32, error) {
	var cid uint32
	if err := db.QueryRow(`SELECT COALESCE(MAX(vsock_cid), 0) FROM machines`).Scan(&cid); err != nil {
		return 0, fmt.Errorf("max vsock cid: %w", err)
	}
	return cid, nil
}
