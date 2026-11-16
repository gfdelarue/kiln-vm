package naming

import "testing"

func TestLibvirtDomainNameFitsTheSwtpmLimit(t *testing.T) {
	cases := map[string]struct{ name, id, want string }{
		"short name":    {"win", "12345678-1234-1234-1234-123456789abc", "kiln-vm-win-12345678"},
		"long name":     {"Very Long Windows VM Name", "12345678-1234-1234-1234-123456789abc", "kiln-vm-very-long-12345678"},
		"symbols only":  {"!!!", "abcdef12-0000-0000-0000-000000000000", "kiln-vm-abcdef12"},
		"cut at a dash": {"build-host-two", "12345678-1234-1234-1234-123456789abc", "kiln-vm-build-host-12345678"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			got := LibvirtDomainName(tc.name, tc.id)
			if got != tc.want {
				t.Fatalf("LibvirtDomainName(%q, %q) = %q, want %q", tc.name, tc.id, got, tc.want)
			}
			if len(got) > LibvirtDomainNameMaxBytes {
				t.Fatalf("%q is %d bytes, want <= %d", got, len(got), LibvirtDomainNameMaxBytes)
			}
		})
	}
}
