package service

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/common"
	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

var assistantNPMStore = &assistant.NPMStore{}

func GetAssistantNPMStore() *assistant.NPMStore { return assistantNPMStore }

type AssistantNPMInstance struct {
	App     string `json:"app"`
	Service string `json:"service"`
	Image   string `json:"image"`
	Ready   bool   `json:"ready"`
}

func npmImage(image string) bool {
	return image == "jc21/nginx-proxy-manager" || strings.HasPrefix(image, "jc21/nginx-proxy-manager:") || strings.HasPrefix(image, "jc21/nginx-proxy-manager@") || strings.HasPrefix(image, "docker.io/jc21/nginx-proxy-manager:")
}
func AssistantNPMDiscover(ctx context.Context) ([]AssistantNPMInstance, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	items, err := assistantContainers(ctx, cli, "")
	if err != nil {
		return nil, errors.New("could not discover NPM containers")
	}
	out := []AssistantNPMInstance{}
	for _, c := range items {
		if npmImage(c.Image) {
			a, s := c.Labels["com.docker.compose.project"], c.Labels["com.docker.compose.service"]
			if assistantName.MatchString(a) && assistantName.MatchString(s) {
				out = append(out, AssistantNPMInstance{a, s, c.Image, c.State == "running"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App+out[i].Service < out[j].App+out[j].Service })
	return out, nil
}
func npmContainer(ctx context.Context, cli client.APIClient, app, service string) (dockerTypes.Container, error) {
	items, err := assistantContainers(ctx, cli, app)
	if err != nil {
		return dockerTypes.Container{}, err
	}
	var found []dockerTypes.Container
	for _, c := range items {
		if c.Labels["com.docker.compose.service"] == service {
			found = append(found, c)
		}
	}
	if len(found) != 1 || found[0].State != "running" {
		return dockerTypes.Container{}, errors.New("select a running, unscaled Compose service")
	}
	return found[0], nil
}
func npmPublished(c dockerTypes.Container, port int) (string, error) {
	for _, p := range c.Ports {
		if p.Type == "tcp" && int(p.PrivatePort) == port && p.PublicPort > 0 {
			host := p.IP
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			return net.JoinHostPort(host, strconv.Itoa(int(p.PublicPort))), nil
		}
	}
	return "", fmt.Errorf("NPM must publish container port %d", port)
}
func npmResolve(ctx context.Context, c assistant.NPMConnection) (*assistant.NPMClient, dockerTypes.Container, error) {
	if !assistantName.MatchString(c.App) || !assistantName.MatchString(c.Service) {
		return nil, dockerTypes.Container{}, errors.New("invalid NPM app or service")
	}
	if _, e := assistantLoad(c.App); e != nil {
		return nil, dockerTypes.Container{}, e
	}
	cli, e := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if e != nil {
		return nil, dockerTypes.Container{}, e
	}
	defer cli.Close()
	target, e := npmContainer(ctx, cli, c.App, c.Service)
	if e != nil {
		return nil, target, e
	}
	if !npmImage(target.Image) {
		return nil, target, errors.New("selected service is not a supported NPM image")
	}
	addr, e := npmPublished(target, 81)
	if e != nil {
		return nil, target, e
	}
	return assistant.NewNPMClient("http://"+addr, c), target, nil
}

type AssistantNPMInventory struct {
	Error        string                     `json:"error,omitempty"`
	Connection   *assistant.NPMConnection   `json:"connection"`
	Certificates []assistant.NPMCertificate `json:"certificates"`
	Suffixes     []string                   `json:"suffixes"`
	AccessLists  []assistant.NPMAccessList  `json:"access_lists"`
	Hosts        []map[string]interface{}   `json:"hosts"`
}

func npmInventory(ctx context.Context, c assistant.NPMConnection) (AssistantNPMInventory, error) {
	out := AssistantNPMInventory{Connection: &c, Hosts: []map[string]interface{}{}, Certificates: []assistant.NPMCertificate{}, Suffixes: []string{}, AccessLists: []assistant.NPMAccessList{}}
	api, _, e := npmResolve(ctx, c)
	if e != nil {
		return out, e
	}
	defer api.Close()
	out.Certificates, e = api.Certificates(ctx)
	if e != nil {
		return out, e
	}
	out.Suffixes = assistant.NPMSuffixes(out.Certificates, time.Now())
	out.AccessLists, e = api.AccessLists(ctx)
	if e != nil {
		return out, e
	}
	hosts, e := api.Hosts(ctx)
	if e != nil {
		return out, e
	}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, map[string]interface{}{"id": h.ID, "domain_names": h.Domains, "certificate_id": h.CertificateID, "access_list_id": h.AccessListID, "enabled": h.Enabled})
	}
	return out, nil
}
func AssistantNPMInspect(ctx context.Context, owner string) (AssistantNPMInventory, error) {
	c, e := assistantNPMStore.Connection(owner)
	if e != nil {
		return AssistantNPMInventory{}, e
	}
	if c == nil {
		return AssistantNPMInventory{}, nil
	}
	return npmInventory(ctx, *c)
}
func AssistantNPMVerify(ctx context.Context, owner string, c assistant.NPMConnection) (AssistantNPMInventory, error) {
	if len(c.Username) > 320 || len(c.Password) > 4096 {
		return AssistantNPMInventory{}, errors.New("NPM credentials exceed allowed size")
	}
	if c.Password == "" {
		old, e := assistantNPMStore.Connection(owner)
		if e != nil {
			return AssistantNPMInventory{}, e
		}
		if old != nil && old.App == c.App && old.Service == c.Service && old.Username == c.Username {
			c.Password = old.Password
		}
	}
	if c.Username == "" || c.Password == "" {
		return AssistantNPMInventory{}, errors.New("enter NPM credentials in the private connection form")
	}
	return npmInventory(ctx, c)
}
func AssistantNPMSave(ctx context.Context, owner string, c assistant.NPMConnection) error {
	unlock, e := LockAppOperation("assistant-npm-owner:" + owner)
	if e != nil {
		return e
	}
	defer unlock()
	inv, e := AssistantNPMVerify(ctx, owner, c)
	if e != nil {
		return e
	}
	c = *inv.Connection
	if c.Suffix == "" && len(inv.Suffixes) == 1 {
		c.Suffix = inv.Suffixes[0]
	}
	if _, e = assistant.SelectNPMCertificate(inv.Certificates, c.Suffix, time.Now()); e != nil {
		return e
	}
	if !npmAccessExists(inv.AccessLists, c.AccessListID) {
		return errors.New("choose public access or an existing NPM Access List")
	}
	return assistantNPMStore.Save(owner, c)
}
func AssistantNPMDisconnect(owner string) error {
	unlock, e := LockAppOperation("assistant-npm-owner:" + owner)
	if e != nil {
		return e
	}
	defer unlock()
	return assistantNPMStore.Disconnect(owner)
}
func npmAccessExists(lists []assistant.NPMAccessList, id int) bool {
	if id == 0 {
		return true
	}
	for _, a := range lists {
		if a.ID == id && id > 0 {
			return true
		}
	}
	return false
}
func assistantNPMTools() []assistant.Tool {
	target := map[string]interface{}{"app": str(), "service": str(), "port": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 65535}, "label": str()}
	return []assistant.Tool{
		tool("npm_inspect", "Inspect the saved Nginx Proxy Manager connection, certificates, eligible wildcard suffixes, proxy hosts and Access Lists. If disconnected, ask the owner to connect NPM in AI settings. Never request credentials in chat.", schema(map[string]interface{}{})),
		tool("publish_app", "Publish a web service using the configured NPM wildcard suffix and access policy. Requires a valid wildcard certificate. Uses HTTP upstream, HTTPS externally, WebSockets and HTTP-to-HTTPS redirection. May connect the app and NPM through a persistent shared Docker network, restarting containers. Verifies TLS and HTTP before updating the CasaOS card. label is one DNS label, not a domain. Inspect first.", schema(target, "app", "service", "port")),
		tool("check_app_url", "Verify a previously published app URL through its associated NPM: wildcard certificate, TLS trust, DNS and HTTP status. Does not follow redirects or open arbitrary URLs.", schema(map[string]interface{}{"app": str(), "label": str()}, "app")),
	}
}
func npmLabel(app, label string) (string, error) {
	if label == "" {
		label = strings.Trim(strings.ReplaceAll(strings.ToLower(app), "_", "-"), "-")
	}
	if !assistant.DNSLabel.MatchString(label) {
		return "", errors.New("hostname must be one DNS label of 1–63 lowercase letters, digits or hyphens")
	}
	return label, nil
}
func npmConnection(ctx context.Context) (string, assistant.NPMConnection, error) {
	owner := assistant.ContextOwner(ctx)
	c, e := assistantNPMStore.Connection(owner)
	if e != nil {
		return owner, assistant.NPMConnection{}, e
	}
	if c == nil {
		return owner, assistant.NPMConnection{}, errors.New("connect Nginx Proxy Manager in AI settings first")
	}
	return owner, *c, nil
}
func npmCurrent(owner string, c assistant.NPMConnection) error {
	v, e := assistantNPMStore.Connection(owner)
	if e != nil {
		return e
	}
	if v == nil || v.Revision != c.Revision {
		return errors.New("NPM connection changed or was disconnected; inspect and propose again")
	}
	return nil
}
func npmAppHash(app *ComposeApp) ([32]byte, error) {
	if len(app.ComposeFiles) != 1 {
		return [32]byte{}, errors.New("publication requires a single CasaOS Compose file")
	}
	b, e := os.ReadFile(app.ComposeFiles[0])
	return sha256.Sum256(b), e
}
func npmServiceIndex(app *ComposeApp, service string) int {
	for i, s := range app.Services {
		if s.Name == service {
			return i
		}
	}
	return -1
}

func npmSavedNetwork(app *ComposeApp, service, name string) (bool, []string) {
	index := npmServiceIndex(app, service)
	if index < 0 {
		return false, nil
	}
	svc := app.Services[index]
	if svc.NetworkMode != "" {
		return false, nil
	}
	if len(svc.Networks) == 0 {
		return name == app.Name+"_default", nil
	}
	for key, attachment := range svc.Networks {
		network := app.Networks[key].Name
		if network == "" {
			network = app.Name + "_" + key
		}
		if network == name {
			if attachment == nil {
				return true, nil
			}
			return true, attachment.Aliases
		}
	}
	return false, nil
}

// Only use a persistent Compose alias that uniquely identifies this container on
// the shared network. Inspect peers rather than trusting service-name uniqueness.
func npmUpstream(ctx context.Context, cli client.APIClient, app *ComposeApp, service string, npm dockerTypes.Container) (string, error) {
	c, e := npmContainer(ctx, cli, app.Name, service)
	if e != nil {
		return "", e
	}
	info, e := cli.ContainerInspect(ctx, c.ID)
	if e != nil {
		return "", e
	}
	index := npmServiceIndex(app, service)
	if index < 0 {
		return "", errors.New("app service not found")
	}
	svc := app.Services[index]
	npmApp, e := assistantLoad(npm.Labels["com.docker.compose.project"])
	if e != nil {
		return "", e
	}
	names := []string{}
	for name := range info.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		endpoint := info.NetworkSettings.Networks[name]
		if endpoint == nil || npm.NetworkSettings == nil || npm.NetworkSettings.Networks[name] == nil {
			continue
		}
		network, e := cli.NetworkInspect(ctx, name, dockerTypes.NetworkInspectOptions{})
		if e != nil {
			return "", e
		}
		if network.Driver != "bridge" || name == "bridge" {
			continue
		}
		appSaved, aliases := npmSavedNetwork(app, service, name)
		npmSaved, _ := npmSavedNetwork(npmApp, npm.Labels["com.docker.compose.service"], name)
		if !appSaved || !npmSaved {
			continue
		}
		// Prefer the explicit app-service alias. A generic service alias (web,
		// app, etc.) can become ambiguous when another app joins this network.
		candidates := []string{}
		for _, alias := range aliases {
			if alias == app.Name+"-"+service {
				candidates = append(candidates, alias)
			}
		}
		candidates = append(candidates, svc.ContainerName)
		runtimeName := strings.TrimPrefix(info.Name, "/")
		if runtimeName == app.Name+"-"+service+"-1" || runtimeName == app.Name+"_"+service+"_1" {
			candidates = append(candidates, runtimeName)
		}
		for _, alias := range candidates {
			if !assistantName.MatchString(alias) {
				continue
			}
			present := false
			for _, a := range endpoint.Aliases {
				if a == alias {
					present = true
				}
			}
			if !present {
				continue
			}
			unique := true
			// Docker DNS is visible across all NPM networks, not just this one.
			for npmNetwork := range npm.NetworkSettings.Networks {
				peers, e := cli.NetworkInspect(ctx, npmNetwork, dockerTypes.NetworkInspectOptions{})
				if e != nil {
					return "", e
				}
				for id := range peers.Containers {
					if id == c.ID {
						continue
					}
					peer, e := cli.ContainerInspect(ctx, id)
					if e != nil {
						return "", e
					}
					if peer.NetworkSettings == nil {
						continue
					}
					if n := peer.NetworkSettings.Networks[npmNetwork]; n != nil {
						for _, a := range n.Aliases {
							if a == alias {
								unique = false
							}
						}
					}
				}
			}
			if unique {
				return alias, nil
			}
		}
	}
	return "", errors.New("no shared Docker network with a unique persistent service alias")
}
func npmNetworkName(app, service string) string {
	sum := sha256.Sum256([]byte(app + "/" + service))
	return fmt.Sprintf("casaos-ai-proxy-%x", sum[:6])
}
func npmNetworkHash(app *ComposeApp, service, network string) ([32]byte, error) {
	target, e := cloneCompose(app)
	if e != nil {
		return [32]byte{}, e
	}
	index := npmServiceIndex(target, service)
	if index < 0 {
		return [32]byte{}, errors.New("service not found")
	}
	if e = assistantSharedNetwork(target, index, network); e != nil {
		return [32]byte{}, e
	}
	data, e := resolvedComposeYAML(target)
	return sha256.Sum256(data), e
}
func npmNetworkPlans(app, npmApp *ComposeApp, service, npmService string) ([]assistant.Prepared, error) {
	if app.Name == npmApp.Name {
		return nil, errors.New("configure unique aliases on the services' shared Compose network first")
	}
	plans := []assistant.Prepared{}
	for _, v := range []struct {
		a *ComposeApp
		s string
	}{{app, service}, {npmApp, npmService}} {
		p, e := assistantConfigurePlan(v.a, assistantArgs{App: v.a.Name, Service: v.s, SharedNetwork: npmNetworkName(npmApp.Name, npmService)})
		if e != nil {
			return nil, e
		}
		plans = append(plans, p)
	}
	return plans, nil
}
func npmFindHost(hosts []assistant.NPMHost, domain string) (*assistant.NPMHost, error) {
	var found *assistant.NPMHost
	for i, h := range hosts {
		for _, d := range h.Domains {
			d = strings.ToLower(d)
			if d == domain || (strings.HasPrefix(d, "*.") && strings.HasSuffix(domain, d[1:])) {
				if found != nil {
					return nil, errors.New("multiple NPM routes conflict with this hostname")
				}
				found = &hosts[i]
				break
			}
		}
	}
	return found, nil
}
func npmValidateHost(h *assistant.NPMHost, r assistant.NPMRoute, app, service string) error {
	if r.Marker != "" && (r.App != app || r.Service != service) {
		return errors.New("hostname belongs to another app or service")
	}
	if h != nil && (h.Advanced != "" || len(h.Locations) > 0) {
		return errors.New("proxy host has custom configuration; review it in NPM before publishing again")
	}
	if h != nil && (r.Marker == "" || h.Marker() != r.Marker || len(h.Domains) != 1 || h.Domains[0] != r.Domain || (r.HostID != 0 && r.HostID != h.ID)) {
		return errors.New("hostname conflicts with a route not owned by this integration")
	}
	return nil
}
func npmSnapshotHost(h *assistant.NPMHost) string { b, _ := stdjson.Marshal(h); return string(b) }

func assistantNPMPlan(ctx context.Context, name string, raw stdjson.RawMessage) (assistant.Prepared, error) {
	if name == "npm_inspect" {
		var a struct{}
		if e := assistantDecode(raw, &a); e != nil {
			return assistant.Prepared{}, e
		}
		return assistant.Prepared{Summary: "Inspect NPM certificates and publication settings", Execute: func(ctx context.Context) (string, error) {
			v, e := AssistantNPMInspect(ctx, assistant.ContextOwner(ctx))
			if v.Connection != nil {
				v.Connection.Username = ""
				v.Connection.Revision = ""
				v.Connection.Password = ""
			}
			return assistantJSON(v), e
		}}, nil
	}
	var a struct {
		App     string `json:"app"`
		Service string `json:"service"`
		Port    int    `json:"port"`
		Label   string `json:"label"`
	}
	if e := assistantDecode(raw, &a); e != nil {
		return assistant.Prepared{}, e
	}
	label, e := npmLabel(a.App, a.Label)
	if e != nil {
		return assistant.Prepared{}, e
	}
	owner, c, e := npmConnection(ctx)
	if e != nil {
		return assistant.Prepared{}, e
	}
	domain := label + "." + c.Suffix
	if name == "check_app_url" {
		if a.Service != "" || a.Port != 0 {
			return assistant.Prepared{}, errors.New("unexpected check arguments")
		}
		return assistant.Prepared{Summary: "Check https://" + domain, Execute: func(ctx context.Context) (string, error) {
			if e := npmCurrent(owner, c); e != nil {
				return "", e
			}
			return npmCheckSaved(ctx, owner, c, a.App, domain)
		}}, nil
	}
	if a.Port < 1 || a.Port > 65535 {
		return assistant.Prepared{}, errors.New("invalid internal web port")
	}
	app, e := assistantLoad(a.App)
	if e != nil {
		return assistant.Prepared{}, e
	}
	idx := npmServiceIndex(app, a.Service)
	if idx < 0 {
		return assistant.Prepared{}, errors.New("app service not found")
	}
	if a.App == c.App && a.Service == c.Service {
		return assistant.Prepared{}, errors.New("publishing the NPM management service is not supported")
	}
	// The internal port must be observed in Compose or the container image/runtime.
	cli, e := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if e != nil {
		return assistant.Prepared{}, e
	}
	defer cli.Close()
	target, e := npmContainer(ctx, cli, a.App, a.Service)
	if e != nil {
		return assistant.Prepared{}, e
	}
	validPort := false
	for _, p := range target.Ports {
		if p.Type == "tcp" && int(p.PrivatePort) == a.Port {
			validPort = true
		}
	}
	for _, p := range app.Services[idx].Ports {
		if int(p.Target) == a.Port && (p.Protocol == "" || p.Protocol == "tcp") {
			validPort = true
		}
	}
	for _, p := range app.Services[idx].Expose {
		if p == strconv.Itoa(a.Port) || p == strconv.Itoa(a.Port)+"/tcp" {
			validPort = true
		}
	}
	if !validPort {
		return assistant.Prepared{}, errors.New("select a web port observed in the service's Compose configuration or container")
	}
	npmApp, e := assistantLoad(c.App)
	if e != nil {
		return assistant.Prepared{}, e
	}
	appHash, e := npmAppHash(app)
	if e != nil {
		return assistant.Prepared{}, e
	}
	npmHash, e := npmAppHash(npmApp)
	if e != nil {
		return assistant.Prepared{}, e
	}
	api, npm, e := npmResolve(ctx, c)
	if e != nil {
		return assistant.Prepared{}, e
	}
	defer api.Close()
	certs, e := api.Certificates(ctx)
	if e != nil {
		return assistant.Prepared{}, e
	}
	cert, e := assistant.SelectNPMCertificate(certs, c.Suffix, time.Now())
	if e != nil {
		return assistant.Prepared{}, e
	}
	lists, e := api.AccessLists(ctx)
	if e != nil {
		return assistant.Prepared{}, e
	}
	if !npmAccessExists(lists, c.AccessListID) {
		return assistant.Prepared{}, errors.New("configured Access List no longer exists")
	}
	hosts, e := api.Hosts(ctx)
	if e != nil {
		return assistant.Prepared{}, e
	}
	existing, e := npmFindHost(hosts, domain)
	if e != nil {
		return assistant.Prepared{}, e
	}
	record, e := assistantNPMStore.Route(owner, c, domain)
	if e != nil {
		return assistant.Prepared{}, e
	}
	if e = npmValidateHost(existing, record, a.App, a.Service); e != nil {
		return assistant.Prepared{}, e
	}
	snapshot := npmSnapshotHost(existing)
	_, networkErr := npmUpstream(ctx, cli, app, a.Service, npm)
	var networkPlans []assistant.Prepared
	afterAppHash, afterNPMHash := appHash, npmHash
	if networkErr != nil {
		networkPlans, e = npmNetworkPlans(app, npmApp, a.Service, c.Service)
		if e != nil {
			return assistant.Prepared{}, e
		}
		network := npmNetworkName(c.App, c.Service)
		afterAppHash, e = npmNetworkHash(app, a.Service, network)
		if e != nil {
			return assistant.Prepared{}, e
		}
		afterNPMHash, e = npmNetworkHash(npmApp, c.Service, network)
		if e != nil {
			return assistant.Prepared{}, e
		}
	}
	access := "public"
	if c.AccessListID > 0 {
		access = fmt.Sprintf("Access List %d", c.AccessListID)
	}
	summary := fmt.Sprintf("Publish %s / %s port %d at https://%s using wildcard certificate %d (%s), %s access, HTTPS redirect and WebSockets. Verify before updating the CasaOS card.", a.App, a.Service, a.Port, domain, cert.ID, cert.Expires, access)
	if len(networkPlans) > 0 {
		summary += " Connect the app and NPM to " + npmNetworkName(c.App, c.Service) + "; both containers may restart. Existing networks are retained."
	}
	return assistant.Prepared{Summary: summary, Execute: func(ctx context.Context) (string, error) {
		unlock, e := LockAppOperation("assistant-npm-owner:" + owner)
		if e != nil {
			return "", e
		}
		defer unlock()
		if e = npmCurrent(owner, c); e != nil {
			return "", e
		}
		currentApp, e := assistantLoad(a.App)
		if e != nil {
			return "", e
		}
		currentNPM, e := assistantLoad(c.App)
		if e != nil {
			return "", e
		}
		ah, e := npmAppHash(currentApp)
		if e != nil {
			return "", e
		}
		nh, e := npmAppHash(currentNPM)
		if e != nil {
			return "", e
		}
		if ah != appHash || nh != npmHash {
			return "", errors.New("app or NPM settings changed; inspect and propose again")
		}
		// Serialize NPM writers across CasaOS users as well as native app operations.
		proxyUnlock, e := LockAppOperation("assistant-npm-instance:" + c.App + "/" + c.Service)
		if e != nil {
			return "", e
		}
		defer proxyUnlock()
		if e = npmRevalidate(ctx, c, cert, domain, snapshot); e != nil {
			return "", e
		}
		for _, p := range networkPlans {
			if _, e = p.Execute(ctx); e != nil {
				return "Shared-network configuration may be partially complete; inspect both services before retrying.", e
			}
		}
		if len(networkPlans) > 0 {
			if e = npmWaitReady(ctx, c); e != nil {
				return "Shared networks configured; NPM has not become ready. Retry publication after checking NPM.", e
			}
		}
		ids := []string{a.App, c.App}
		sort.Strings(ids)
		unlocks := []func(){}
		defer func() {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
		}()
		for i, id := range ids {
			if i > 0 && id == ids[i-1] {
				continue
			}
			u, e := LockAppOperation(id)
			if e != nil {
				return "", e
			}
			unlocks = append(unlocks, u)
		}
		updated, e := assistantLoad(a.App)
		if e != nil {
			return "", e
		}
		actualHash, e := npmAppHash(updated)
		if e != nil {
			return "", e
		}
		if actualHash != afterAppHash {
			return "", errors.New("app settings changed during publication; inspect and retry")
		}
		lockedNPM, e := assistantLoad(c.App)
		if e != nil {
			return "", e
		}
		lockedNPMHash, e := npmAppHash(lockedNPM)
		if e != nil {
			return "", e
		}
		if lockedNPMHash != afterNPMHash {
			return "", errors.New("NPM settings changed during publication; inspect and retry")
		}
		if e = npmRevalidate(ctx, c, cert, domain, snapshot); e != nil {
			return "Shared networks may have been configured; no proxy changes applied.", e
		}
		active, npm, e := npmResolve(ctx, c)
		if e != nil {
			return "", e
		}
		defer active.Close()
		docker, e := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if e != nil {
			return "", e
		}
		defer docker.Close()
		upstream, e := npmUpstream(ctx, docker, updated, a.Service, npm)
		if e != nil {
			return "", e
		}
		if record.Marker == "" {
			record = assistant.NPMRoute{App: a.App, Service: a.Service, Domain: domain, Marker: assistant.ID()}
		}
		record.Port = a.Port
		// Save intent before POST. Its marker lets a retry recover an unknown outcome.
		if e = assistantNPMStore.SaveRoute(owner, c, record); e != nil {
			return "", e
		}
		desired := assistant.NPMHost{Domains: []string{domain}, Scheme: "http", Host: upstream, Port: a.Port, CertificateID: cert.ID, AccessListID: c.AccessListID, SSLForced: true, Websocket: true, Enabled: true, Locations: []stdjson.RawMessage{}, Meta: map[string]interface{}{"casaos_assistant": record.Marker}}
		saved := desired
		if existing == nil {
			e = active.Call(ctx, "POST", "/nginx/proxy-hosts", desired, &saved)
		} else if !existing.SameRoute(desired) || existing.Meta["nginx_online"] == false {
			// Update only our fields; preserve unrelated NPM settings.
			payload := map[string]interface{}{"domain_names": desired.Domains, "forward_scheme": desired.Scheme, "forward_host": desired.Host, "forward_port": desired.Port, "certificate_id": desired.CertificateID, "access_list_id": desired.AccessListID, "ssl_forced": true, "allow_websocket_upgrade": true, "enabled": true, "advanced_config": "", "locations": desired.Locations, "meta": desired.Meta}
			e = active.Call(ctx, "PUT", "/nginx/proxy-hosts/"+strconv.Itoa(existing.ID), payload, &saved)
		} else {
			saved = *existing
		}
		if e != nil {
			return "NPM write outcome may be unknown; the publication intent is saved. Inspect and retry to recover without duplicates.", e
		}
		if saved.ID <= 0 {
			return "NPM may have created the route; inspect before retrying.", errors.New("NPM returned no proxy host ID")
		}
		record.HostID = saved.ID
		if e = assistantNPMStore.SaveRoute(owner, c, record); e != nil {
			return "Proxy host saved; local publication record could not be updated.", e
		}
		var readback assistant.NPMHost
		e = active.Call(ctx, "GET", "/nginx/proxy-hosts/"+strconv.Itoa(saved.ID), nil, &readback)
		if e != nil {
			return "Proxy host saved; verification incomplete.", e
		}
		if !readback.SameRoute(desired) || readback.Marker() != record.Marker || readback.Meta["nginx_online"] == false {
			return "Proxy host saved; configuration verification failed.", errors.New("NPM did not retain the requested settings")
		}
		result, e := npmProbeApp(ctx, npm, updated, domain, c.Suffix)
		// nginx reload is asynchronous even after the API has saved the host.
		for attempt := 0; attempt < 7 && (e != nil || result.HTTPStatus == 502 || result.HTTPStatus == 503); attempt++ {
			select {
			case <-ctx.Done():
				return assistantJSON(result), ctx.Err()
			case <-time.After(time.Second):
			}
			result, e = npmProbeApp(ctx, npm, updated, domain, c.Suffix)
		}
		if e != nil {
			return assistantJSON(result), e
		}
		if !result.Verified {
			return assistantJSON(result), errors.New("proxy host saved but URL verification is incomplete; the CasaOS card was not changed")
		}
		if e = npmSaveCard(updated, domain); e != nil {
			return assistantJSON(result), fmt.Errorf("URL verified but card update failed: %w", e)
		}
		result.CardUpdated = true
		return assistantJSON(result), nil
	}}, nil
}
func npmWaitReady(ctx context.Context, c assistant.NPMConnection) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		api, _, e := npmResolve(waitCtx, c)
		if e == nil {
			_, e = api.Certificates(waitCtx)
			api.Close()
			if e == nil {
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return errors.New("NPM API did not become ready after its container restart")
		case <-time.After(time.Second):
		}
	}
}
func npmRevalidate(ctx context.Context, c assistant.NPMConnection, cert assistant.NPMCertificate, domain, snapshot string) error {
	api, _, e := npmResolve(ctx, c)
	if e != nil {
		return e
	}
	defer api.Close()
	certs, e := api.Certificates(ctx)
	if e != nil {
		return e
	}
	found := false
	for _, v := range certs {
		if v.ID == cert.ID && reflect.DeepEqual(v, cert) && v.Covers(c.Suffix, time.Now()) {
			found = true
		}
	}
	if !found {
		return errors.New("wildcard certificate changed, expired or disappeared; inspect and propose again")
	}
	lists, e := api.AccessLists(ctx)
	if e != nil {
		return e
	}
	if !npmAccessExists(lists, c.AccessListID) {
		return errors.New("Access List disappeared; inspect and propose again")
	}
	hosts, e := api.Hosts(ctx)
	if e != nil {
		return e
	}
	h, e := npmFindHost(hosts, domain)
	if e != nil {
		return e
	}
	if npmSnapshotHost(h) != snapshot {
		return errors.New("proxy host changed since the proposal; inspect and propose again")
	}
	return nil
}

type npmProbeResult struct {
	URL         string `json:"url"`
	Verified    bool   `json:"verified"`
	CardUpdated bool   `json:"card_updated"`
	DNS         string `json:"dns"`
	TLS         string `json:"tls"`
	HTTPStatus  int    `json:"http_status"`
	Status      string `json:"status"`
	Perspective string `json:"perspective"`
}

func npmAppPath(app *ComposeApp) (string, error) {
	info, e := app.StoreInfo(false)
	if e != nil {
		return "", e
	}
	p := "/" + strings.TrimLeft(info.Index, "/")
	u, e := url.Parse(p)
	if e != nil || u.IsAbs() || u.Host != "" || strings.ContainsAny(p, "\\\r\n") {
		return "", errors.New("unsupported application URL path")
	}
	return p, nil
}

var npmProbeApp = npmProbe

func npmProbe(ctx context.Context, npm dockerTypes.Container, app *ComposeApp, domain, suffix string) (npmProbeResult, error) {
	return npmProbeTLS(ctx, npm, app, domain, suffix, nil)
}
func npmProbeTLS(ctx context.Context, npm dockerTypes.Container, app *ComposeApp, domain, suffix string, roots *x509.CertPool) (npmProbeResult, error) {
	r := npmProbeResult{URL: "https://" + domain, DNS: "unchecked", TLS: "unchecked", Perspective: "Checked from CasaOS through local NPM; external internet access is not established."}
	p, e := npmAppPath(app)
	if e != nil {
		return r, e
	}
	r.URL += p
	addr, e := npmPublished(npm, 443)
	if e != nil {
		return r, e
	}
	dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ips, dnsErr := net.DefaultResolver.LookupIPAddr(dnsCtx, domain)
	cancel()
	if dnsErr != nil || len(ips) == 0 {
		r.DNS = "unresolved"
	} else {
		r.DNS = "resolved; destination not verified from external clients"
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: domain, RootCAs: roots}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}}
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer httpClient.CloseIdleConnections()
	req, e := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
	if e != nil {
		return r, e
	}
	res, e := httpClient.Do(req)
	if e != nil {
		r.TLS = "failed"
		r.Status = "TLS or NPM connection failed"
		return r, errors.New("could not verify the HTTPS connection; check certificate validity, trust and NPM readiness")
	}
	defer res.Body.Close()
	wildcard := false
	if res.TLS != nil && len(res.TLS.PeerCertificates) > 0 {
		for _, san := range res.TLS.PeerCertificates[0].DNSNames {
			if strings.ToLower(san) == "*."+suffix {
				wildcard = true
			}
		}
	}
	if !wildcard {
		r.TLS = "missing wildcard"
		return r, errors.New("served TLS certificate does not contain the required wildcard")
	}
	r.TLS = "trusted wildcard; hostname and validity verified"
	r.HTTPStatus = res.StatusCode
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 400:
		r.Status = "responding"
	case res.StatusCode == 401 || res.StatusCode == 403:
		r.Status = "authentication or access policy required"
	default:
		r.Status = "application or upstream error"
	}
	r.Verified = r.DNS != "unresolved" && res.StatusCode >= 200 && (res.StatusCode < 400 || res.StatusCode == 401 || res.StatusCode == 403)
	return r, nil
}
func npmSaveCard(app *ComposeApp, domain string) error {
	if _, e := npmAppPath(app); e != nil {
		return e
	}
	if e := assistantNoSymlink(app.ComposeFiles[0]); e != nil {
		return e
	}
	original, e := os.ReadFile(app.ComposeFiles[0])
	if e != nil {
		return e
	}
	ext, ok := app.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{})
	if !ok {
		return errors.New("missing CasaOS card metadata")
	}
	if ext["hostname"] == domain && ext["scheme"] == "https" && ext["port_map"] == "443" {
		return nil
	}
	ext["hostname"] = domain
	ext["scheme"] = "https"
	ext["port_map"] = "443"
	b, e := resolvedComposeYAML(app)
	if e != nil {
		return e
	}
	if e = atomicPrivateWrite(app.ComposeFiles[0]+".assistant.bak", original); e != nil {
		return e
	}
	if e = atomicPrivateWrite(app.ComposeFiles[0], b); e != nil {
		return e
	}
	return Updates.SettingsChanged(app)
}
func npmCheckSaved(ctx context.Context, owner string, c assistant.NPMConnection, appID, domain string) (string, error) {
	r, e := assistantNPMStore.Route(owner, c, domain)
	if e != nil {
		return "", e
	}
	if r.Marker == "" || r.App != appID {
		return "", errors.New("no saved publication for this app and hostname")
	}
	api, npm, e := npmResolve(ctx, c)
	if e != nil {
		return "", e
	}
	defer api.Close()
	hosts, e := api.Hosts(ctx)
	if e != nil {
		return "", e
	}
	h, e := npmFindHost(hosts, domain)
	if e != nil {
		return "", e
	}
	if h == nil {
		return "", errors.New("saved proxy host no longer exists")
	}
	if e = npmValidateHost(h, r, r.App, r.Service); e != nil {
		return "", e
	}
	certs, e := api.Certificates(ctx)
	if e != nil {
		return "", e
	}
	valid := false
	for _, cert := range certs {
		if cert.ID == h.CertificateID && cert.Covers(c.Suffix, time.Now()) {
			valid = true
		}
	}
	if !valid || !h.Enabled || !h.SSLForced || h.AccessListID != c.AccessListID {
		return "", errors.New("saved route no longer satisfies wildcard HTTPS and access policy")
	}
	app, e := assistantLoad(appID)
	if e != nil {
		return "", e
	}
	docker, e := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if e != nil {
		return "", e
	}
	defer docker.Close()
	upstream, e := npmUpstream(ctx, docker, app, r.Service, npm)
	if e != nil {
		return "", e
	}
	if h.Host != upstream || h.Port != r.Port || h.Scheme != "http" || h.Advanced != "" || len(h.Locations) > 0 {
		return "", errors.New("saved route upstream changed; inspect and republish")
	}
	result, e := npmProbeApp(ctx, npm, app, domain, c.Suffix)
	return assistantJSON(result), e
}

func initAssistantNPM() { assistantNPMStore.Root = filepath.Join(assistantSettingsStore.Root, "npm") }
