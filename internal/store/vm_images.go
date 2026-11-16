package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

const vmImageColumns = `id, name, profile, source, image_id, disk_size_mb, labels_json, resolved_json, created_at`

func (db *DB) CreateVMImage(img *models.VMImage) error {
	labelsJSON, err := encodeLabels(img.Labels)
	if err != nil {
		return err
	}
	resolvedJSON, err := encodeLabels(img.Resolved)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO vm_images (`+vmImageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.Name, img.Profile, img.Source, img.ImageID, img.DiskSizeMB, labelsJSON, resolvedJSON, img.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert vm image: %w", err)
	}
	return nil
}

func (db *DB) GetVMImage(ref string) (*models.VMImage, error) {
	img, err := scanVMImage(db.QueryRow(`SELECT `+vmImageColumns+` FROM vm_images WHERE id = ? OR name = ?`, ref, ref))
	if err != nil {
		return nil, fmt.Errorf("get vm image: %w", err)
	}
	return img, nil
}

func (db *DB) GetVMImageByImageID(imageID string) (*models.VMImage, error) {
	img, err := scanVMImage(db.QueryRow(`SELECT `+vmImageColumns+` FROM vm_images WHERE image_id = ?`, imageID))
	if err != nil {
		return nil, fmt.Errorf("get vm image by image id: %w", err)
	}
	return img, nil
}

func (db *DB) ListVMImages() ([]models.VMImage, error) {
	rows, err := db.Query(`SELECT ` + vmImageColumns + ` FROM vm_images ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list vm images: %w", err)
	}
	defer rows.Close()

	var images []models.VMImage
	for rows.Next() {
		img, err := scanVMImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, *img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vm images: %w", err)
	}
	return images, nil
}

func (db *DB) DeleteVMImage(id string) error {
	result, err := db.Exec(`DELETE FROM vm_images WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vm image: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return fmt.Errorf("delete vm image: %w", sql.ErrNoRows)
	}
	return nil
}

func scanVMImage(row scanner) (*models.VMImage, error) {
	var img models.VMImage
	var source, labelsJSON, resolvedJSON string
	if err := row.Scan(&img.ID, &img.Name, &img.Profile, &source, &img.ImageID, &img.DiskSizeMB, &labelsJSON, &resolvedJSON, &img.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan vm image: %w", err)
	}
	img.Source = models.VMImageSource(source)
	labels, err := decodeLabels(labelsJSON)
	if err != nil {
		return nil, fmt.Errorf("scan vm image labels: %w", err)
	}
	resolved, err := decodeLabels(resolvedJSON)
	if err != nil {
		return nil, fmt.Errorf("scan vm image resolved: %w", err)
	}
	img.Labels = labels
	img.Resolved = resolved
	return &img, nil
}

func encodeLabels(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("encode labels: %w", err)
	}
	return string(data), nil
}

func decodeLabels(data string) (map[string]string, error) {
	if data == "" || data == "{}" {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(data), &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}
