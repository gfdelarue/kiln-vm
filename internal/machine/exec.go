package machine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// SSHKeyPair holds generated SSH keys for a VM.
type SSHKeyPair struct {
	PrivateKeyPath string
	PublicKey      string // authorized_keys format
}

// GenerateSSHKeyPair creates an Ed25519 keypair and writes the private key to disk.
func GenerateSSHKeyPair(machineDir string) (*SSHKeyPair, error) {
	privatePEM, publicKey, err := newSSHKey()
	if err != nil {
		return nil, err
	}

	privKeyPath := filepath.Join(machineDir, "id_ed25519")
	if err := os.WriteFile(privKeyPath, privatePEM, 0600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}

	return &SSHKeyPair{
		PrivateKeyPath: privKeyPath,
		PublicKey:      string(ssh.MarshalAuthorizedKey(publicKey)),
	}, nil
}

func newSSHKey() ([]byte, ssh.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("create ssh public key: %w", err)
	}

	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}

	return pem.EncodeToMemory(privBytes), sshPub, nil
}
