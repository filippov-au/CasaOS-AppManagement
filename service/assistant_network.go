package service

import (
	"context"
	"errors"
	"strings"

	composeTypes "github.com/compose-spec/compose-go/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

const assistantNetworkLabel = "io.casaos.assistant.network"

func assistantSharedNetwork(app *ComposeApp, serviceIndex int, name string) error {
	if !assistantName.MatchString(name) || !strings.HasPrefix(name, "casaos-ai-") || len(name) <= len("casaos-ai-") {
		return errors.New("shared_network must be a name beginning with casaos-ai- (up to 63 characters)")
	}
	svc := &app.Services[serviceIndex]
	if svc.NetworkMode != "" {
		return errors.New("shared networks cannot be added while the service has an explicit network_mode; convert it to Compose networks in app settings first")
	}
	if app.Networks == nil {
		app.Networks = composeTypes.Networks{}
	}
	if len(svc.Networks) == 0 {
		// Make the implicit default explicit before adding a second network.
		svc.Networks = map[string]*composeTypes.ServiceNetworkConfig{"default": nil}
		if _, ok := app.Networks["default"]; !ok {
			app.Networks["default"] = composeTypes.NetworkConfig{Name: app.Name + "_default"}
		}
	}
	if existing, ok := app.Networks[name]; ok && (existing.Name != name || !existing.External.External) {
		return errors.New("the Compose file already uses this network key differently; choose a new shared network name")
	}
	app.Networks[name] = composeTypes.NetworkConfig{Name: name, External: composeTypes.External{External: true}}
	if svc.Networks[name] == nil {
		svc.Networks[name] = &composeTypes.ServiceNetworkConfig{}
	}
	alias := app.Name + "-" + svc.Name
	for _, existing := range svc.Networks[name].Aliases {
		if existing == alias {
			return nil
		}
	}
	svc.Networks[name].Aliases = append(svc.Networks[name].Aliases, alias)
	return nil
}

// Only join networks created for this feature. Internal bridge networks allow
// inter-app traffic without becoming a new route to the internet; each service
// retains its pre-existing networks and their routing.
func assistantEnsureNetwork(ctx context.Context, cli client.APIClient, name string) error {
	network, err := cli.NetworkInspect(ctx, name, dockerTypes.NetworkInspectOptions{})
	if errdefs.IsNotFound(err) {
		_, err = cli.NetworkCreate(ctx, name, dockerTypes.NetworkCreate{Driver: "bridge", Internal: true, CheckDuplicate: true, Labels: map[string]string{assistantNetworkLabel: "1"}})
		if err != nil {
			return err
		}
		network, err = cli.NetworkInspect(ctx, name, dockerTypes.NetworkInspectOptions{})
	}
	if err != nil {
		return err
	}
	if network.Name != name || network.Driver != "bridge" || !network.Internal || network.Labels[assistantNetworkLabel] != "1" {
		return errors.New("this network is not an assistant-managed internal bridge; choose a new shared network name")
	}
	return nil
}
