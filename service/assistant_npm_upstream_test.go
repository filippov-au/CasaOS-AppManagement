package service

import (
	"testing"

	composeTypes "github.com/compose-spec/compose-go/types"
	dockerTypes "github.com/docker/docker/api/types"
)

func TestAssistantNPMPublishedTarget(t *testing.T) {
	for _, tc := range []struct {
		name, savedIP, runtimeIP, published string
		runtimePort                         uint16
		protocol                            string
		gateways                            []string
		want                                string
	}{
		{name: "wildcard uses NPM gateway and host port", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8081, gateways: []string{"172.17.0.1"}, want: "172.17.0.1"},
		{name: "explicit LAN binding", savedIP: "192.168.1.20", runtimeIP: "192.168.1.20", published: "8081", runtimePort: 8081, want: "192.168.1.20"},
		{name: "stable gateway selection", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8081, gateways: []string{"172.19.0.1", "172.17.0.1"}, want: "172.17.0.1"},
		{name: "loopback inaccessible from NPM", runtimeIP: "127.0.0.1", published: "8081", runtimePort: 8081},
		{name: "IPv6 loopback", runtimeIP: "::1", published: "8081", runtimePort: 8081},
		{name: "IPv6 only unsupported", runtimeIP: "::", published: "8081", runtimePort: 8081},
		{name: "ephemeral mapping", runtimeIP: "0.0.0.0", runtimePort: 8081, gateways: []string{"172.17.0.1"}},
		{name: "port drift", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8182, gateways: []string{"172.17.0.1"}},
		{name: "binding drift", savedIP: "192.168.1.20", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8081, gateways: []string{"172.17.0.1"}},
		{name: "UDP is not HTTP", protocol: "udp", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8081, gateways: []string{"172.17.0.1"}},
		{name: "missing gateway", runtimeIP: "0.0.0.0", published: "8081", runtimePort: 8081},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protocol := tc.protocol
			if protocol == "" {
				protocol = "tcp"
			}
			svc := composeTypes.ServiceConfig{Ports: []composeTypes.ServicePortConfig{{Target: 8080, Published: tc.published, HostIP: tc.savedIP, Protocol: protocol}}}
			target := dockerTypes.Container{Ports: []dockerTypes.Port{{PrivatePort: 8080, PublicPort: tc.runtimePort, IP: tc.runtimeIP, Type: protocol}}}
			got, err := npmPublishedTarget(svc, target, 8080, tc.gateways)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted unavailable host route: %+v", got)
				}
			} else if err != nil || got.Host != tc.want || got.Port != 8081 || !got.Published {
				t.Fatalf("got %+v (%v), want %s:8081", got, err, tc.want)
			}
			if _, err := npmPublishedTarget(svc, target, 8182, tc.gateways); err == nil {
				t.Fatal("substituted another container port")
			}
		})
	}
}
