// Package thirdparty exposes the exact third-party notices that kiln-vm
// redistributes inside its own artifacts.
package thirdparty

import _ "embed"

// GoXNetGuestLicensePath is the rootfs-relative location of the notice for the
// copy of kiln-vm-agent installed in a kiln-vm VM.
const GoXNetGuestLicensePath = "usr/share/doc/kiln-vm-agent/third-party/golang-x-net.LICENSE"

//go:embed golang.org/x/net/LICENSE
var goXNetLicense string

// GoXNetLicense returns the exact LICENSE bytes shipped by golang.org/x/net.
func GoXNetLicense() []byte {
	return []byte(goXNetLicense)
}
