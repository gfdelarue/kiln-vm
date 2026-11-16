#!/usr/bin/env bash
# build-rootfs.sh — Build an ext4 rootfs from a Dockerfile
# Usage: ./scripts/build-rootfs.sh <dockerfile-path> <output-path> [size-mb]
set -euo pipefail

DOCKERFILE="${1:?Usage: build-rootfs.sh <dockerfile> <output> [size-mb]}"
OUTPUT="${2:?Usage: build-rootfs.sh <dockerfile> <output> [size-mb]}"
SIZE_MB="${3:-1024}"

TAG="kiln-vm-rootfs-$(date +%s)"
CONTAINER=""
MOUNT_DIR=""

cleanup() {
    [ -n "$CONTAINER" ] && docker rm -f "$CONTAINER" 2>/dev/null || true
    [ -n "$MOUNT_DIR" ] && sudo umount "$MOUNT_DIR" 2>/dev/null || true
    [ -n "$MOUNT_DIR" ] && rm -rf "$MOUNT_DIR" 2>/dev/null || true
    docker rmi "$TAG" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Building Docker image..."
docker build -t "$TAG" -f "$DOCKERFILE" "$(dirname "$DOCKERFILE")"

echo "==> Creating container..."
CONTAINER=$(docker create "$TAG")

echo "==> Exporting filesystem..."
TARBALL="${OUTPUT}.tar"
docker export -o "$TARBALL" "$CONTAINER"

echo "==> Creating ext4 image (${SIZE_MB}MB)..."
dd if=/dev/zero of="$OUTPUT" bs=1M count="$SIZE_MB" status=progress
mkfs.ext4 -F "$OUTPUT"

echo "==> Mounting and extracting..."
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"
sudo tar xf "$TARBALL" -C "$MOUNT_DIR"

echo "==> Setting up SSH directory..."
sudo mkdir -p "$MOUNT_DIR/root/.ssh"
sudo chmod 700 "$MOUNT_DIR/root/.ssh"

echo "==> Cleaning up..."
sudo umount "$MOUNT_DIR"
rm -f "$TARBALL"
rmdir "$MOUNT_DIR"
MOUNT_DIR=""

echo "==> Done: $OUTPUT"
