package config

import (
	"reflect"
	"testing"
)

func TestServerCloneDeepCopiesMutableFields(t *testing.T) {
	original := Server{
		Auth: Auth{
			Users:             map[string]string{"alice": "hash"},
			PasswordUnchanged: []string{"alice"},
		},
		Access: AccessControl{
			AdmissionCIDRs: []string{"192.0.2.0/24"},
			TargetRules:    []string{"deny example.com:443"},
		},
		DNS: DNS{
			Servers: []string{"192.0.2.53:53"},
			DoH: &DoH{
				URL:          "https://legacy.example/dns-query",
				URLs:         []string{"https://dns.example/dns-query"},
				BootstrapIPs: []string{"192.0.2.54"},
				Headers:      map[string]string{"X-Test": "original"},
			},
		},
		UDP: UDP{
			AdvertiseMap: map[string]string{"192.0.2.1": "198.51.100.1"},
			RelayPorts:   []int{12000, 12007},
		},
	}

	clone := original.Clone()
	if !reflect.DeepEqual(clone, original) {
		t.Fatalf("clone differs from original:\nclone=%+v\noriginal=%+v", clone, original)
	}
	if clone.DNS.DoH == original.DNS.DoH {
		t.Fatal("Clone retained the DoH pointer")
	}

	clone.Auth.Users["alice"] = "changed"
	clone.Auth.PasswordUnchanged[0] = "changed"
	clone.Access.AdmissionCIDRs[0] = "changed"
	clone.Access.TargetRules[0] = "changed"
	clone.DNS.Servers[0] = "changed"
	clone.DNS.DoH.URL = "changed"
	clone.DNS.DoH.URLs[0] = "changed"
	clone.DNS.DoH.BootstrapIPs[0] = "changed"
	clone.DNS.DoH.Headers["X-Test"] = "changed"
	clone.UDP.AdvertiseMap["192.0.2.1"] = "changed"
	clone.UDP.RelayPorts[0] = 13000

	if original.Auth.Users["alice"] != "hash" || original.Auth.PasswordUnchanged[0] != "alice" {
		t.Fatalf("Clone shares authentication state: %+v", original.Auth)
	}
	if original.Access.AdmissionCIDRs[0] != "192.0.2.0/24" || original.Access.TargetRules[0] != "deny example.com:443" {
		t.Fatalf("Clone shares access-control state: %+v", original.Access)
	}
	if original.DNS.Servers[0] != "192.0.2.53:53" || original.DNS.DoH.URL != "https://legacy.example/dns-query" ||
		original.DNS.DoH.URLs[0] != "https://dns.example/dns-query" || original.DNS.DoH.BootstrapIPs[0] != "192.0.2.54" ||
		original.DNS.DoH.Headers["X-Test"] != "original" {
		t.Fatalf("Clone shares DNS/DoH state: %+v", original.DNS)
	}
	if original.UDP.AdvertiseMap["192.0.2.1"] != "198.51.100.1" {
		t.Fatalf("Clone shares UDP advertise map: %+v", original.UDP.AdvertiseMap)
	}
	if original.UDP.RelayPorts[0] != 12000 {
		t.Fatalf("Clone shares UDP relay ports: %+v", original.UDP.RelayPorts)
	}
}

func TestServerClonePreservesNilAndAllocatedEmptyContainers(t *testing.T) {
	original := Server{
		Auth:   Auth{Users: map[string]string{}, PasswordUnchanged: []string{}},
		Access: AccessControl{AdmissionCIDRs: []string{}, TargetRules: []string{}},
		DNS: DNS{Servers: []string{}, DoH: &DoH{
			URLs: []string{}, BootstrapIPs: []string{}, Headers: map[string]string{},
		}},
		UDP: UDP{AdvertiseMap: map[string]string{}, RelayPorts: []int{}},
	}
	clone := original.Clone()

	allocated := map[string]any{
		"auth.users":              clone.Auth.Users,
		"auth.password_unchanged": clone.Auth.PasswordUnchanged,
		"access.admission_cidrs":  clone.Access.AdmissionCIDRs,
		"access.target_rules":     clone.Access.TargetRules,
		"dns.servers":             clone.DNS.Servers,
		"dns.doh.urls":            clone.DNS.DoH.URLs,
		"dns.doh.bootstrap_ips":   clone.DNS.DoH.BootstrapIPs,
		"dns.doh.headers":         clone.DNS.DoH.Headers,
		"udp.advertise_map":       clone.UDP.AdvertiseMap,
		"udp.relay_ports":         clone.UDP.RelayPorts,
	}
	for name, value := range allocated {
		if reflect.ValueOf(value).IsNil() {
			t.Errorf("Clone changed allocated empty %s to nil", name)
		}
	}

	nilClone := (Server{}).Clone()
	if nilClone.Auth.Users != nil || nilClone.Auth.PasswordUnchanged != nil ||
		nilClone.Access.AdmissionCIDRs != nil || nilClone.Access.TargetRules != nil ||
		nilClone.DNS.Servers != nil || nilClone.DNS.DoH != nil || nilClone.UDP.AdvertiseMap != nil || nilClone.UDP.RelayPorts != nil {
		t.Fatalf("Clone allocated nil containers: %+v", nilClone)
	}
}
