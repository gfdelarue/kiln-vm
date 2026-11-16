package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func (db *DB) CreateVM(vm *models.VMRecord) error {
	vm.Power.Policy = defaultStorePowerPolicy(vm.Power.Policy)
	_, err := db.Exec(`
		INSERT INTO vms (
			id, name, profile, machine_id, image_id, disk_size_mb,
			power_mode, power_idle_timeout_seconds,
			power_last_activity_at, power_last_started_at, power_last_stopped_at, power_stop_reason,
			created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vm.ID, vm.Name, vm.Profile, vm.MachineID, vm.ImageID, vm.DiskSizeMB,
		vm.Power.Policy.Mode, vm.Power.Policy.IdleTimeoutSeconds,
		vm.Power.LastActivityAt, vm.Power.LastStartedAt, vm.Power.LastStoppedAt, vm.Power.StopReason,
		vm.CreatedAt, vm.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert vm: %w", err)
	}
	return nil
}

func (db *DB) GetVM(ref string) (*models.VMRecord, error) {
	vm, err := scanVMRow(db.QueryRow(`
		SELECT id, name, profile, machine_id, image_id, disk_size_mb,
			power_mode, power_idle_timeout_seconds,
			power_last_activity_at, power_last_started_at, power_last_stopped_at, power_stop_reason,
			created_at, updated_at
		FROM vms WHERE id = ? OR name = ?`, ref, ref,
	))
	if err != nil {
		return nil, fmt.Errorf("get vm: %w", err)
	}
	return vm, nil
}

func (db *DB) GetVMByMachineID(machineID string) (*models.VMRecord, error) {
	vm, err := scanVMRow(db.QueryRow(`
		SELECT id, name, profile, machine_id, image_id, disk_size_mb,
			power_mode, power_idle_timeout_seconds,
			power_last_activity_at, power_last_started_at, power_last_stopped_at, power_stop_reason,
			created_at, updated_at
		FROM vms WHERE machine_id = ?`, machineID,
	))
	if err != nil {
		return nil, fmt.Errorf("get vm by machine id: %w", err)
	}
	return vm, nil
}

func (db *DB) ListVMs() ([]models.VMRecord, error) {
	rows, err := db.Query(`
		SELECT id, name, profile, machine_id, image_id, disk_size_mb,
			power_mode, power_idle_timeout_seconds,
			power_last_activity_at, power_last_started_at, power_last_stopped_at, power_stop_reason,
			created_at, updated_at
		FROM vms ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	defer rows.Close()

	var vms []models.VMRecord
	for rows.Next() {
		vm, err := scanVMRows(rows)
		if err != nil {
			return nil, fmt.Errorf("scan vm: %w", err)
		}
		vms = append(vms, *vm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vms: %w", err)
	}
	return vms, nil
}

func (db *DB) ListPowerStartVMs() ([]models.VMRecord, error) {
	rows, err := db.Query(`
		SELECT v.id, v.name, v.profile, v.machine_id, v.image_id, v.disk_size_mb,
			v.power_mode, v.power_idle_timeout_seconds,
			v.power_last_activity_at, v.power_last_started_at, v.power_last_stopped_at, v.power_stop_reason,
			v.created_at, v.updated_at
		FROM vms v LEFT JOIN machines m ON m.id = v.machine_id
		WHERE v.power_mode IN ('always', 'warm')
			OR (v.power_mode = 'manual' AND m.state = ? AND m.error IN (?, ?))
		ORDER BY v.created_at ASC`,
		models.MachineStateStopped, models.MachineStopReasonDaemonShutdown, models.MachineStopReasonDaemonRestarted)
	if err != nil {
		return nil, fmt.Errorf("list daemon-start vms: %w", err)
	}
	defer rows.Close()

	var vms []models.VMRecord
	for rows.Next() {
		vm, err := scanVMRows(rows)
		if err != nil {
			return nil, fmt.Errorf("scan daemon-start vm: %w", err)
		}
		vms = append(vms, *vm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daemon-start vms: %w", err)
	}
	return vms, nil
}

func (db *DB) UpdateVMPowerPolicy(id string, policy models.VMPowerPolicy) error {
	result, err := db.Exec(`
		UPDATE vms
		SET power_mode = ?,
			power_idle_timeout_seconds = ?,
			updated_at = ?
		WHERE id = ?`,
		policy.Mode, policy.IdleTimeoutSeconds, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("update vm power policy: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("update vm power policy: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) RecordVMPowerActivity(id string, activityAt time.Time) error {
	result, err := db.Exec(`
		UPDATE vms
		SET power_last_activity_at = ?, updated_at = ?
		WHERE id = ?`,
		activityAt.UTC(), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("record vm power activity: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("record vm power activity: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) RecordVMPowerStarted(id string, startedAt time.Time) error {
	result, err := db.Exec(`
		UPDATE vms
		SET power_last_started_at = ?,
			power_last_activity_at = ?,
			power_stop_reason = '',
			updated_at = ?
		WHERE id = ?`,
		startedAt.UTC(), startedAt.UTC(), time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("record vm power start: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("record vm power start: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) RecordVMPowerStopped(id string, stoppedAt time.Time, reason string) error {
	result, err := db.Exec(`
		UPDATE vms
		SET power_last_stopped_at = ?,
			power_stop_reason = ?,
			updated_at = ?
		WHERE id = ?`,
		stoppedAt.UTC(), reason, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("record vm power stop: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("record vm power stop: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) DeleteVM(id string) error {
	result, err := db.Exec(`DELETE FROM vms WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vm: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("delete vm: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) ListVMCapabilityConditions(vmID string) ([]models.VMCapabilityCondition, error) {
	rows, err := db.Query(`
		SELECT capability, status, phase, command, detail, remediation, checked_at, updated_at
		FROM vm_capability_conditions
		WHERE vm_id = ?
		ORDER BY capability`, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm capability conditions: %w", err)
	}
	defer rows.Close()

	var conditions []models.VMCapabilityCondition
	for rows.Next() {
		var condition models.VMCapabilityCondition
		var status string
		var checkedAt sql.NullTime
		if err := rows.Scan(
			&condition.Capability,
			&status,
			&condition.Phase,
			&condition.Command,
			&condition.Detail,
			&condition.Remediation,
			&checkedAt,
			&condition.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan vm capability condition: %w", err)
		}
		condition.Status = models.VMCapabilityStatus(status)
		condition.CheckedAt = nullTimePtr(checkedAt)
		conditions = append(conditions, condition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vm capability conditions: %w", err)
	}
	return conditions, nil
}

func (db *DB) UpsertVMCapabilityCondition(vmID string, condition models.VMCapabilityCondition) error {
	_, err := db.Exec(`
		INSERT INTO vm_capability_conditions (
			vm_id, capability, status, phase, command, detail, remediation, checked_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(vm_id, capability) DO UPDATE SET
			status = excluded.status,
			phase = excluded.phase,
			command = excluded.command,
			detail = excluded.detail,
			remediation = excluded.remediation,
			checked_at = excluded.checked_at,
			updated_at = excluded.updated_at`,
		vmID,
		condition.Capability,
		condition.Status,
		condition.Phase,
		condition.Command,
		condition.Detail,
		condition.Remediation,
		condition.CheckedAt,
		condition.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert vm capability condition: %w", err)
	}
	return nil
}

func (db *DB) TouchVM(id string) error {
	_, err := db.Exec(`UPDATE vms SET updated_at = ? WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("touch vm: %w", err)
	}
	return nil
}

func (db *DB) CountVMsByImageID(imageID string) (int, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM vms WHERE image_id = ?`, imageID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count vms by image id: %w", err)
	}
	return count, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanVMRow(row scanner) (*models.VMRecord, error) {
	var vm models.VMRecord
	var mode string
	var lastActivityAt sql.NullTime
	var lastStartedAt sql.NullTime
	var lastStoppedAt sql.NullTime
	if err := row.Scan(
		&vm.ID,
		&vm.Name,
		&vm.Profile,
		&vm.MachineID,
		&vm.ImageID,
		&vm.DiskSizeMB,
		&mode,
		&vm.Power.Policy.IdleTimeoutSeconds,
		&lastActivityAt,
		&lastStartedAt,
		&lastStoppedAt,
		&vm.Power.StopReason,
		&vm.CreatedAt,
		&vm.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan vm: %w", err)
	}
	vm.Power.Policy.Mode = models.VMPowerMode(mode)
	vm.Power.LastActivityAt = nullTimePtr(lastActivityAt)
	vm.Power.LastStartedAt = nullTimePtr(lastStartedAt)
	vm.Power.LastStoppedAt = nullTimePtr(lastStoppedAt)
	return &vm, nil
}

func scanVMRows(rows *sql.Rows) (*models.VMRecord, error) {
	return scanVMRow(rows)
}

func defaultStorePowerPolicy(policy models.VMPowerPolicy) models.VMPowerPolicy {
	if policy.Mode == "" {
		policy.Mode = models.VMPowerModeManual
	}
	return policy
}

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	normalized := value.Time.UTC()
	return &normalized
}

func (db *DB) CreateVMProfileImage(img *models.VMProfileImage) error {
	_, err := db.Exec(`
		INSERT INTO vm_profile_images (id, profile, image_id, fingerprint, rootfs_size_mb, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		img.ID, img.Profile, img.ImageID, img.Fingerprint, img.RootfsSizeMB, img.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert vm profile image: %w", err)
	}
	return nil
}

func (db *DB) GetVMProfileImage(id string) (*models.VMProfileImage, error) {
	img := &models.VMProfileImage{}
	err := db.QueryRow(`
		SELECT id, profile, image_id, fingerprint, rootfs_size_mb, created_at
		FROM vm_profile_images WHERE id = ?`, id,
	).Scan(&img.ID, &img.Profile, &img.ImageID, &img.Fingerprint, &img.RootfsSizeMB, &img.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get vm profile image: %w", err)
	}
	return img, nil
}

func (db *DB) GetVMProfileImageByImageID(imageID string) (*models.VMProfileImage, error) {
	img := &models.VMProfileImage{}
	err := db.QueryRow(`
		SELECT id, profile, image_id, fingerprint, rootfs_size_mb, created_at
		FROM vm_profile_images WHERE image_id = ?`, imageID,
	).Scan(&img.ID, &img.Profile, &img.ImageID, &img.Fingerprint, &img.RootfsSizeMB, &img.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get vm profile image by image id: %w", err)
	}
	return img, nil
}

func (db *DB) FindVMProfileImage(profile, fingerprint string, rootfsSizeMB int64) (*models.VMProfileImage, error) {
	img := &models.VMProfileImage{}
	err := db.QueryRow(`
		SELECT id, profile, image_id, fingerprint, rootfs_size_mb, created_at
		FROM vm_profile_images
		WHERE profile = ? AND fingerprint = ? AND rootfs_size_mb = ?`,
		profile, fingerprint, rootfsSizeMB,
	).Scan(&img.ID, &img.Profile, &img.ImageID, &img.Fingerprint, &img.RootfsSizeMB, &img.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("find vm profile image: %w", err)
	}
	return img, nil
}

func (db *DB) ListVMProfileImages() ([]models.VMProfileImage, error) {
	rows, err := db.Query(`
		SELECT id, profile, image_id, fingerprint, rootfs_size_mb, created_at
		FROM vm_profile_images ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list vm profile images: %w", err)
	}
	defer rows.Close()

	var images []models.VMProfileImage
	for rows.Next() {
		var img models.VMProfileImage
		if err := rows.Scan(&img.ID, &img.Profile, &img.ImageID, &img.Fingerprint, &img.RootfsSizeMB, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan vm profile image: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vm profile images: %w", err)
	}
	return images, nil
}

func (db *DB) DeleteVMProfileImage(id string) error {
	result, err := db.Exec(`DELETE FROM vm_profile_images WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vm profile image: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("delete vm profile image: %w", sql.ErrNoRows)
	}
	return nil
}

func (db *DB) DeleteVMProfileImagesByProfile(profile string) ([]models.VMProfileImage, error) {
	rows, err := db.Query(`
		SELECT id, profile, image_id, fingerprint, rootfs_size_mb, created_at
		FROM vm_profile_images WHERE profile = ?`, profile)
	if err != nil {
		return nil, fmt.Errorf("list vm profile images for profile delete: %w", err)
	}
	defer rows.Close()

	var images []models.VMProfileImage
	for rows.Next() {
		var img models.VMProfileImage
		if err := rows.Scan(&img.ID, &img.Profile, &img.ImageID, &img.Fingerprint, &img.RootfsSizeMB, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan vm profile image for profile delete: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vm profile images for profile delete: %w", err)
	}

	if _, err := db.Exec(`DELETE FROM vm_profile_images WHERE profile = ?`, profile); err != nil {
		return nil, fmt.Errorf("delete vm profile images by profile: %w", err)
	}
	return images, nil
}
