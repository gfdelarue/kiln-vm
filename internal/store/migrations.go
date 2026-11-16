package store

import (
	"database/sql"
	"fmt"
)

const schemaVersion = 1

const baselineSchema = `
CREATE TABLE images (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	tag TEXT NOT NULL DEFAULT 'latest',
	boot_mode TEXT NOT NULL DEFAULT 'persistent',
	size_mb INTEGER NOT NULL DEFAULT 0,
	rootfs_path TEXT NOT NULL,
	created_at DATETIME NOT NULL
);

CREATE TABLE machines (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	backend TEXT NOT NULL,
	guest_os TEXT NOT NULL,
	libvirt_json TEXT NOT NULL DEFAULT '{}',
	image_id TEXT NOT NULL,
	network_policy TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	vcpus INTEGER NOT NULL,
	cpu_kind TEXT NOT NULL DEFAULT '',
	cpu_millis INTEGER NOT NULL DEFAULT 0,
	memory_mb INTEGER NOT NULL,
	vsock_cid INTEGER NOT NULL DEFAULT 0,
	ip TEXT NOT NULL DEFAULT '',
	gateway_ip TEXT NOT NULL DEFAULT '',
	tap_device TEXT NOT NULL DEFAULT '',
	network_cidr TEXT NOT NULL DEFAULT '',
	network_index INTEGER NOT NULL DEFAULT 0,
	guest_mac TEXT NOT NULL DEFAULT '',
	netmask TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	diagnostics_json TEXT NOT NULL DEFAULT '[]',
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE INDEX idx_machines_state ON machines(state);

CREATE TABLE tokens (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	hash TEXT NOT NULL UNIQUE,
	created_at DATETIME NOT NULL,
	expires_at DATETIME
);

CREATE TABLE vms (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	profile TEXT NOT NULL,
	machine_id TEXT NOT NULL UNIQUE REFERENCES machines(id) ON DELETE CASCADE,
	image_id TEXT NOT NULL REFERENCES images(id),
	disk_size_mb INTEGER NOT NULL,
	power_mode TEXT NOT NULL DEFAULT 'manual',
	power_idle_timeout_seconds INTEGER NOT NULL DEFAULT 0,
	power_last_activity_at DATETIME,
	power_last_started_at DATETIME,
	power_last_stopped_at DATETIME,
	power_stop_reason TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE TABLE vm_profile_images (
	id TEXT PRIMARY KEY,
	profile TEXT NOT NULL,
	image_id TEXT NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
	fingerprint TEXT NOT NULL,
	rootfs_size_mb INTEGER NOT NULL,
	created_at DATETIME NOT NULL,
	UNIQUE(profile, fingerprint, rootfs_size_mb)
);

CREATE TABLE vm_capability_conditions (
	vm_id TEXT NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
	capability TEXT NOT NULL,
	status TEXT NOT NULL,
	phase TEXT NOT NULL DEFAULT '',
	command TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	remediation TEXT NOT NULL DEFAULT '',
	checked_at DATETIME,
	updated_at DATETIME NOT NULL,
	PRIMARY KEY(vm_id, capability)
);
`

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("database schema version %d is newer than this build supports (%d)", version, schemaVersion)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(baselineSchema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	return nil
}
