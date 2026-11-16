package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// swtpm writes "<domain>:<uuid>" into a 64-byte certificate field.
const LibvirtDomainNameMaxBytes = 27

const libvirtDomainPrefix = "kiln-vm-"

func LibvirtDomainName(machineName, machineID string) string {
	id := shortID(machineID)
	if id == "" {
		id = shortHash(machineName)
	}
	suffix := "-" + id
	name := slugify(machineName)
	if room := LibvirtDomainNameMaxBytes - len(libvirtDomainPrefix) - len(suffix); len(name) > room {
		name = strings.TrimRight(name[:room], "-")
	}
	if name == "" {
		return "kiln-vm" + suffix
	}
	return libvirtDomainPrefix + name + suffix
}

func slugify(input string) string {
	value := strings.ToLower(strings.TrimSpace(input))
	var b strings.Builder
	lastSeparator := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastSeparator = false
			continue
		}
		if !lastSeparator {
			b.WriteRune('-')
			lastSeparator = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortID(value string) string {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}
