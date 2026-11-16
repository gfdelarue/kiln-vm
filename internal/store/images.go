package store

import (
	"fmt"

	"github.com/gfdelarue/kiln-vm/internal/models"
)

func (db *DB) CreateImage(img *models.Image) error {
	if img.BootMode == "" {
		img.BootMode = "persistent"
	}
	_, err := db.Exec(`
		INSERT INTO images (id, name, tag, boot_mode, size_mb, rootfs_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.Name, img.Tag, img.BootMode, img.SizeMB, img.RootfsPath, img.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert image: %w", err)
	}
	return nil
}

func (db *DB) GetImage(id string) (*models.Image, error) {
	img := &models.Image{}
	err := db.QueryRow(`
		SELECT id, name, tag, boot_mode, size_mb, rootfs_path, created_at
		FROM images WHERE id = ?`, id,
	).Scan(&img.ID, &img.Name, &img.Tag, &img.BootMode, &img.SizeMB, &img.RootfsPath, &img.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get image: %w", err)
	}
	return img, nil
}

func (db *DB) ListImages() ([]models.Image, error) {
	rows, err := db.Query(`
		SELECT id, name, tag, boot_mode, size_mb, rootfs_path, created_at
		FROM images ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()

	var images []models.Image
	for rows.Next() {
		var img models.Image
		if err := rows.Scan(&img.ID, &img.Name, &img.Tag, &img.BootMode, &img.SizeMB, &img.RootfsPath, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan image: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate images: %w", err)
	}
	return images, nil
}

func (db *DB) DeleteImage(id string) error {
	_, err := db.Exec(`DELETE FROM images WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}
