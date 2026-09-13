package service

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"

	composeTypes "github.com/compose-spec/compose-go/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type npmRouteTarget struct {
	Host      string
	Port      int
	Published bool
}

// A host route must agree with both saved Compose and the running container.
// Do not persist ephemeral host ports or substitute another exposed web port.
func npmPublishedTarget(svc composeTypes.ServiceConfig, target dockerTypes.Container, port int, gateways []string) (npmRouteTarget, error) {
	candidates := []npmRouteTarget{}
	for _, saved := range svc.Ports {
		if int(saved.Target) != port || (saved.Protocol != "" && saved.Protocol != "tcp") {
			continue
		}
		published, err := strconv.Atoi(saved.Published)
		if err != nil || published < 1 || published > 65535 {
			continue
		}
		for _, runtime := range target.Ports {
			if runtime.Type != "tcp" || int(runtime.PrivatePort) != port || int(runtime.PublicPort) != published {
				continue
			}
			binding := runtime.IP
			if binding == "" {
				binding = "0.0.0.0"
			}
			if saved.HostIP != "" && saved.HostIP != binding {
				continue
			}
			ip := net.ParseIP(binding)
			// IPv4 host routes only; loopback belongs to NPM's own namespace.
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			if ip.IsUnspecified() {
				for _, gateway := range gateways {
					candidates = append(candidates, npmRouteTarget{Host: gateway, Port: published, Published: true})
				}
			} else {
				candidates = append(candidates, npmRouteTarget{Host: ip.String(), Port: published, Published: true})
			}
		}
	}
	if len(candidates) == 0 {
		return npmRouteTarget{}, errors.New("no stable, non-loopback published IPv4 TCP port for this service; publish the selected container port on a host address reachable from NPM")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Port != candidates[j].Port {
			return candidates[i].Port < candidates[j].Port
		}
		return candidates[i].Host < candidates[j].Host
	})
	return candidates[0], nil
}

// Prefer an existing persistent container alias, then an existing published
// host port. Neither path changes networking or recreates NPM.
func npmRouteUpstream(ctx context.Context, cli client.APIClient, app *ComposeApp, service string, port int, npm dockerTypes.Container) (npmRouteTarget, error) {
	if alias, err := npmUpstream(ctx, cli, app, service, npm); err == nil {
		return npmRouteTarget{Host: alias, Port: port}, nil
	}
	npmApp, err := assistantLoad(npm.Labels["com.docker.compose.project"])
	if err != nil {
		return npmRouteTarget{}, err
	}
	npmService := npm.Labels["com.docker.compose.service"]
	npmIndex := npmServiceIndex(npmApp, npmService)
	index := npmServiceIndex(app, service)
	if npmIndex < 0 || index < 0 {
		return npmRouteTarget{}, errors.New("app or NPM service not found")
	}
	mode := npmApp.Services[npmIndex].NetworkMode
	if mode != "" && mode != "bridge" {
		return npmRouteTarget{}, errors.New("automatic host-port routing requires NPM on a Docker bridge network")
	}
	gateways := []string{}
	if npm.NetworkSettings != nil {
		for name, endpoint := range npm.NetworkSettings.Networks {
			if endpoint == nil {
				continue
			}
			persistent, _ := npmSavedNetwork(npmApp, npmService, name)
			if !persistent && !(mode == "bridge" && name == "bridge") {
				continue
			}
			network, err := cli.NetworkInspect(ctx, name, dockerTypes.NetworkInspectOptions{})
			if err != nil {
				return npmRouteTarget{}, err
			}
			if network.Driver != "bridge" || network.Internal {
				continue
			}
			for _, config := range network.IPAM.Config {
				ip := net.ParseIP(config.Gateway)
				if ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() {
					gateways = append(gateways, ip.String())
				}
			}
		}
	}
	if len(gateways) == 0 {
		return npmRouteTarget{}, errors.New("NPM has no persistent IPv4 bridge gateway for host-port routing")
	}
	target, err := npmContainer(ctx, cli, app.Name, service)
	if err != nil {
		return npmRouteTarget{}, err
	}
	return npmPublishedTarget(app.Services[index], target, port, gateways)
}
