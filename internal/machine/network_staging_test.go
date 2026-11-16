package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type stagingTAPFixtureLink struct {
	kind      string
	tunType   string
	alias     string
	up        bool
	addresses []string
}

type stagingTAPRunnerFixture struct {
	mu      sync.Mutex
	links   map[string]stagingTAPFixtureLink
	calls   []scriptedNetworkCommandCall
	deleted []string
}

func newStagingTAPRunnerFixture() *stagingTAPRunnerFixture {
	return &stagingTAPRunnerFixture{links: make(map[string]stagingTAPFixtureLink)}
}

func (r *stagingTAPRunnerFixture) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, hadDeadline := ctx.Deadline()
	r.calls = append(r.calls, scriptedNetworkCommandCall{name: name, args: append([]string(nil), args...), hadDeadline: hadDeadline})
	if name != "ip" {
		return nil, fmt.Errorf("unexpected command %s %#v", name, args)
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "-j -d link show dev "):
		linkName := args[len(args)-1]
		link, ok := r.links[linkName]
		if !ok {
			return []byte("Cannot find device"), fakeNetworkCommandExitError{code: 1}
		}
		payload := []ipLinkJSONRecord{{IfName: linkName, IfAlias: link.alias}}
		payload[0].LinkInfo.InfoKind = link.kind
		payload[0].LinkInfo.InfoData.Type = link.tunType
		if link.up {
			payload[0].Flags = []string{"UP"}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal staging TAP link fixture: %w", err)
		}
		return data, nil
	case strings.HasPrefix(joined, "-j -4 addr show dev "):
		linkName := args[len(args)-1]
		link, ok := r.links[linkName]
		if !ok {
			return []byte("Cannot find device"), fakeNetworkCommandExitError{code: 1}
		}
		payload := []ipAddressJSONRecord{{IfName: linkName}}
		for _, address := range link.addresses {
			parts := strings.Split(address, "/")
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid fixture address %q", address)
			}
			ip := parts[0]
			var prefix int
			if _, err := fmt.Sscanf(parts[1], "%d", &prefix); err != nil {
				return nil, fmt.Errorf("parse staging TAP address prefix %q: %w", parts[1], err)
			}
			payload[0].AddrInfo = append(payload[0].AddrInfo, struct {
				Family    string `json:"family"`
				Local     string `json:"local"`
				PrefixLen int    `json:"prefixlen"`
			}{Family: "inet", Local: ip, PrefixLen: prefix})
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal staging TAP address fixture: %w", err)
		}
		return data, nil
	case len(args) == 3 && args[0] == "link" && args[1] == "del":
		linkName := args[2]
		if _, ok := r.links[linkName]; !ok {
			return []byte("Cannot find device"), fakeNetworkCommandExitError{code: 1}
		}
		delete(r.links, linkName)
		r.deleted = append(r.deleted, linkName)
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected ip args %#v", args)
	}
}

func (r *stagingTAPRunnerFixture) deletedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

func TestFreshPublicNetworkCleanupCoversEveryTAPCreationCrashStage(t *testing.T) {
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	stagingName, err := publicNetworkTAPStagingName(*alloc)
	if err != nil {
		t.Fatalf("publicNetworkTAPStagingName: %v", err)
	}
	ownedAlias := publicNetworkTAPAlias(*alloc)
	for _, stage := range []struct {
		name  string
		link  string
		state stagingTAPFixtureLink
	}{
		{name: "before tuntap create"},
		{name: "after tuntap create", link: stagingName, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap"}},
		{name: "after ownership alias", link: stagingName, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: ownedAlias}},
		{name: "after final rename", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: ownedAlias}},
		{name: "after gateway address", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: ownedAlias, addresses: []string{alloc.GatewayIP + "/30"}}},
		{name: "after link up", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: ownedAlias, up: true, addresses: []string{alloc.GatewayIP + "/30"}}},
	} {
		t.Run(stage.name, func(t *testing.T) {
			runner := newStagingTAPRunnerFixture()
			if stage.link != "" {
				runner.links[stage.link] = stage.state
			}
			host := newScriptedExecNetworkHost(runner)
			if err := host.DeleteTAP(*alloc); err != nil {
				t.Fatalf("DeleteTAP: %v", err)
			}
			if len(runner.links) != 0 {
				t.Fatalf("links after recovery cleanup = %+v, want empty", runner.links)
			}
			if stage.link == "" {
				if got := runner.deletedSnapshot(); len(got) != 0 {
					t.Fatalf("deleted links = %v, want none before host mutation", got)
				}
			} else if got := runner.deletedSnapshot(); len(got) != 1 || got[0] != stage.link {
				t.Fatalf("deleted links = %v, want exact crash-stage link %s", got, stage.link)
			}
		})
	}
}

func TestFreshPublicNetworkCleanupRefusesDivergentStagingAndFinalLinks(t *testing.T) {
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	stagingName, err := publicNetworkTAPStagingName(*alloc)
	if err != nil {
		t.Fatalf("publicNetworkTAPStagingName: %v", err)
	}
	for _, test := range []struct {
		name  string
		link  string
		state stagingTAPFixtureLink
	}{
		{name: "staging wrong type", link: stagingName, state: stagingTAPFixtureLink{kind: "bridge"}},
		{name: "staging foreign alias", link: stagingName, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: "foreign"}},
		{name: "staging link up", link: stagingName, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", up: true}},
		{name: "staging address present", link: stagingName, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", addresses: []string{"192.0.2.1/24"}}},
		{name: "final wrong type", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "bridge"}},
		{name: "final alias missing", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap"}},
		{name: "final foreign alias", link: alloc.TAPDevice, state: stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: "foreign"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newStagingTAPRunnerFixture()
			runner.links[test.link] = test.state
			host := newScriptedExecNetworkHost(runner)
			if err := host.DeleteTAP(*alloc); err == nil {
				t.Fatal("DeleteTAP accepted divergent link")
			}
			if got := runner.deletedSnapshot(); len(got) != 0 {
				t.Fatalf("deleted links = %v, want divergent link untouched", got)
			}
			if _, present := runner.links[test.link]; !present {
				t.Fatalf("divergent link %s was removed", test.link)
			}
		})
	}
}

func TestFreshPublicNetworkCleanupRemovesProvenStagingWithoutTouchingForeignFinal(t *testing.T) {
	alloc := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	stagingName, err := publicNetworkTAPStagingName(*alloc)
	if err != nil {
		t.Fatalf("publicNetworkTAPStagingName: %v", err)
	}
	runner := newStagingTAPRunnerFixture()
	runner.links[alloc.TAPDevice] = stagingTAPFixtureLink{kind: "tun", tunType: "tap", alias: "foreign"}
	runner.links[stagingName] = stagingTAPFixtureLink{kind: "tun", tunType: "tap"}
	host := newScriptedExecNetworkHost(runner)

	if err := host.DeleteTAP(*alloc); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("DeleteTAP error = %v, want retained foreign-final collision", err)
	}
	if got := runner.deletedSnapshot(); len(got) != 1 || got[0] != stagingName {
		t.Fatalf("deleted links = %v, want only proven staging link", got)
	}
	if _, present := runner.links[alloc.TAPDevice]; !present {
		t.Fatal("foreign final link was removed")
	}
}

func TestPublicNetworkStagingNameIsBoundedAndOwnerSpecific(t *testing.T) {
	first := mustNetworkAllocation(t, NewNetworkManagerWithDNSDataDir("172.16.0.0/16", 1, nil, ""), 1)
	second := *first
	second.OwnerID = "different-machine-owner"
	firstName, err := publicNetworkTAPStagingName(*first)
	if err != nil {
		t.Fatalf("first staging name: %v", err)
	}
	secondName, err := publicNetworkTAPStagingName(second)
	if err != nil {
		t.Fatalf("second staging name: %v", err)
	}
	if len(firstName) > 15 || len(secondName) > 15 || firstName == secondName {
		t.Fatalf("staging names = %q / %q, want distinct owner-bound names <=15 bytes", firstName, secondName)
	}
}
