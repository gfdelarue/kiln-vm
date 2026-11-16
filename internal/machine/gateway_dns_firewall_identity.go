package machine

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	gatewayDNSFirewallIdentityVersion = "kiln-vm-gateway-dns-firewall:v2"
	gatewayDNSFirewallChainPrefix     = "KVD_"
	gatewayDNSFirewallOwnerIDBytes    = 16
	// Linux iptables user-chain names are limited to 28 visible characters.
	// KVD_ plus 24 hexadecimal owner characters is exactly 28 and retains 96
	// bits of collision resistance while the full 128-bit identity is carried
	// by every ownership comment and the durable identity file.
	gatewayDNSFirewallChainIDChars = 24
)

type gatewayDNSFirewallIdentity struct {
	OwnerID string
	Chain   string
}

func newGatewayDNSFirewallIdentity(ownerID string) (gatewayDNSFirewallIdentity, error) {
	if len(ownerID) != gatewayDNSFirewallOwnerIDBytes*2 || ownerID != strings.ToLower(ownerID) {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("owner identity must be %d lowercase hexadecimal characters", gatewayDNSFirewallOwnerIDBytes*2)
	}
	decoded, err := hex.DecodeString(ownerID)
	if err != nil || len(decoded) != gatewayDNSFirewallOwnerIDBytes {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("owner identity is not valid hexadecimal")
	}
	return gatewayDNSFirewallIdentity{
		OwnerID: ownerID,
		Chain:   gatewayDNSFirewallChainPrefix + ownerID[:gatewayDNSFirewallChainIDChars],
	}, nil
}

func randomGatewayDNSFirewallIdentity() (gatewayDNSFirewallIdentity, error) {
	raw := make([]byte, gatewayDNSFirewallOwnerIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("generate gateway DNS firewall owner identity: %w", err)
	}
	return newGatewayDNSFirewallIdentity(hex.EncodeToString(raw))
}

func gatewayDNSFirewallIdentityContents(identity gatewayDNSFirewallIdentity) string {
	return fmt.Sprintf("%s\nowner-id=%s\n", gatewayDNSFirewallIdentityVersion, identity.OwnerID)
}

func parseGatewayDNSFirewallIdentity(path string, data []byte) (gatewayDNSFirewallIdentity, error) {
	prefix := gatewayDNSFirewallIdentityVersion + "\nowner-id="
	text := string(data)
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, "\n") || strings.Count(text, "\n") != 2 {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("%w: ownership identity %s has unexpected contents", errGatewayDNSFirewallOwnershipCollision, path)
	}
	identity, err := newGatewayDNSFirewallIdentity(strings.TrimSuffix(strings.TrimPrefix(text, prefix), "\n"))
	if err != nil {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("%w: ownership identity %s is invalid: %v", errGatewayDNSFirewallOwnershipCollision, path, err)
	}
	return identity, nil
}

func readGatewayDNSFirewallIdentity(path string) (gatewayDNSFirewallIdentity, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return gatewayDNSFirewallIdentity{}, false, nil
	}
	if err != nil {
		return gatewayDNSFirewallIdentity{}, false, fmt.Errorf("inspect gateway DNS firewall ownership identity %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return gatewayDNSFirewallIdentity{}, false, fmt.Errorf(
			"%w: ownership identity %s must be a regular file without group/world write permission",
			errGatewayDNSFirewallOwnershipCollision,
			path,
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, false, fmt.Errorf("read gateway DNS firewall ownership identity %s: %w", path, err)
	}
	identity, err := parseGatewayDNSFirewallIdentity(path, data)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, false, err
	}
	return identity, true, nil
}

// loadOrCreateGatewayDNSFirewallIdentity creates ownership evidence before any
// iptables mutation. The no-replace hard-link publish makes concurrent creators
// converge on exactly one durable identity; a crash before chain creation is
// safe because the resulting chain name is private and collision-resistant.
func loadOrCreateGatewayDNSFirewallIdentity(path string) (gatewayDNSFirewallIdentity, error) {
	if identity, present, err := readGatewayDNSFirewallIdentity(path); err != nil {
		return gatewayDNSFirewallIdentity{}, err
	} else if present {
		return identity, nil
	}

	identity, err := randomGatewayDNSFirewallIdentity()
	if err != nil {
		return gatewayDNSFirewallIdentity{}, err
	}
	if err := persistGatewayDNSFirewallIdentity(path, identity); err != nil {
		return gatewayDNSFirewallIdentity{}, err
	}
	winner, present, err := readGatewayDNSFirewallIdentity(path)
	if err != nil {
		return gatewayDNSFirewallIdentity{}, err
	}
	if !present {
		return gatewayDNSFirewallIdentity{}, fmt.Errorf("gateway DNS firewall ownership identity disappeared after creation")
	}
	return winner, nil
}

func persistGatewayDNSFirewallIdentity(path string, identity gatewayDNSFirewallIdentity) error {
	if _, err := newGatewayDNSFirewallIdentity(identity.OwnerID); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("create gateway DNS firewall ownership directory %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".gateway-dns-firewall.owner-*")
	if err != nil {
		return fmt.Errorf("create temporary gateway DNS firewall ownership identity in %s: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0640); err != nil {
		return fmt.Errorf("set gateway DNS firewall ownership identity permissions: %w", err)
	}
	if _, err := temporary.WriteString(gatewayDNSFirewallIdentityContents(identity)); err != nil {
		return fmt.Errorf("write gateway DNS firewall ownership identity: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync gateway DNS firewall ownership identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close gateway DNS firewall ownership identity: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish gateway DNS firewall ownership identity %s: %w", path, err)
		}
		winner, present, readErr := readGatewayDNSFirewallIdentity(path)
		if readErr != nil {
			return readErr
		}
		if !present {
			return fmt.Errorf("%w: ownership identity collision at %s", errGatewayDNSFirewallOwnershipCollision, path)
		}
		if winner != identity {
			// A concurrent valid creator won. This is not corruption; callers load
			// and use the winner instead of ever adopting its chain implicitly.
			return nil
		}
		return nil
	}
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary gateway DNS firewall ownership identity %s: %w", temporaryPath, err)
	}
	return syncGatewayDNSFirewallDirectory(directory)
}

func syncGatewayDNSFirewallDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open gateway DNS firewall ownership directory %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync gateway DNS firewall ownership directory %s: %w", path, err)
	}
	return nil
}
