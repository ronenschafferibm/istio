// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	"fmt"
	"strconv"
	"strings"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	"github.com/gogo/protobuf/types"

	"istio.io/istio/pilot/pkg/model"
	istio_route "istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
)

// buildSidecarInboundHTTPRouteConfig builds the route config with a single wildcard virtual host on the inbound path
// TODO: enable websockets, trace decorators
func (configgen *ConfigGeneratorImpl) buildSidecarInboundHTTPRouteConfig(env model.Environment,
	node model.Proxy, instance *model.ServiceInstance) *xdsapi.RouteConfiguration {

	clusterName := model.BuildSubsetKey(model.TrafficDirectionInbound, "",
		instance.Service.Hostname, instance.Endpoint.ServicePort.Port)
	defaultRoute := istio_route.BuildDefaultHTTPRoute(clusterName)

	inboundVHost := route.VirtualHost{
		Name:    fmt.Sprintf("%s|http|%d", model.TrafficDirectionInbound, instance.Endpoint.ServicePort.Port),
		Domains: []string{"*"},
		Routes:  []route.Route{*defaultRoute},
	}

	r := &xdsapi.RouteConfiguration{
		Name:             clusterName,
		VirtualHosts:     []route.VirtualHost{inboundVHost},
		ValidateClusters: &types.BoolValue{Value: false},
	}

	for _, p := range configgen.Plugins {
		in := &plugin.InputParams{
			ListenerType:    plugin.ListenerTypeHTTP,
			Env:             &env,
			Node:            &node,
			ServiceInstance: instance,
			Service:         instance.Service,
		}
		p.OnInboundRouteConfiguration(in, r)
	}

	return r
}

// BuildSidecarOutboundHTTPRouteConfig builds an outbound HTTP Route for sidecar.
// Based on port, will determine all virtual hosts that listen on the port.
func (configgen *ConfigGeneratorImpl) BuildSidecarOutboundHTTPRouteConfig(env model.Environment, node model.Proxy,
	proxyInstances []*model.ServiceInstance, services []*model.Service, routeName string) *xdsapi.RouteConfiguration {

	port := 0
	if routeName != RDSHttpProxy {
		var err error
		port, err = strconv.Atoi(routeName)
		if err != nil {
			return nil
		}
	}

	nameToServiceMap := make(map[model.Hostname]*model.Service)
	for _, svc := range services {
		if port == 0 {
			nameToServiceMap[svc.Hostname] = svc
		} else {
			if svcPort, exists := svc.Ports.GetByPort(port); exists {
				nameToServiceMap[svc.Hostname] = &model.Service{
					Hostname:     svc.Hostname,
					Addresses:    model.BuildAddresses(svc.Addresses...),
					ClusterVIPs:  svc.ClusterVIPs,
					MeshExternal: svc.MeshExternal,
					Ports:        []*model.Port{svcPort},
				}
			}
		}
	}

	// Collect all proxy labels for source match
	var proxyLabels model.LabelsCollection
	for _, w := range proxyInstances {
		proxyLabels = append(proxyLabels, w.Labels)
	}

	// Get list of virtual services bound to the mesh gateway
	meshGateway := map[string]bool{model.IstioMeshGateway: true}
	virtualServices := env.VirtualServices(meshGateway)
	guardedHosts := istio_route.TranslateVirtualHosts(virtualServices, nameToServiceMap, proxyLabels, meshGateway)
	vHostPortMap := make(map[int][]route.VirtualHost)

	for _, guardedHost := range guardedHosts {
		// If none of the routes matched by source, skip this guarded host
		if len(guardedHost.Routes) == 0 {
			continue
		}

		virtualHosts := make([]route.VirtualHost, 0, len(guardedHost.Hosts)+len(guardedHost.Services))
		for _, host := range guardedHost.Hosts {
			virtualHosts = append(virtualHosts, route.VirtualHost{
				Name:    fmt.Sprintf("%s:%d", host, guardedHost.Port),
				Domains: []string{host},
				Routes:  guardedHost.Routes,
			})
		}

		for _, svc := range guardedHost.Services {
			virtualHosts = append(virtualHosts, route.VirtualHost{
				Name:    fmt.Sprintf("%s:%d", svc.Hostname, guardedHost.Port),
				Domains: buildVirtualHostDomains(svc, guardedHost.Port, node),
				Routes:  guardedHost.Routes,
			})
		}

		vHostPortMap[guardedHost.Port] = append(vHostPortMap[guardedHost.Port], virtualHosts...)
	}

	var virtualHosts []route.VirtualHost
	if routeName == RDSHttpProxy {
		virtualHosts = mergeAllVirtualHosts(vHostPortMap)
	} else {
		virtualHosts = vHostPortMap[port]
	}

	util.SortVirtualHosts(virtualHosts)
	out := &xdsapi.RouteConfiguration{
		Name:             fmt.Sprintf("%d", port),
		VirtualHosts:     virtualHosts,
		ValidateClusters: &types.BoolValue{Value: false},
	}

	// call plugins
	for _, p := range configgen.Plugins {
		in := &plugin.InputParams{
			ListenerType: plugin.ListenerTypeHTTP,
			Env:          &env,
			Node:         &node,
		}
		p.OnOutboundRouteConfiguration(in, out)
	}

	return out
}

// buildVirtualHostDomains generates the set of domain matches for a service being accessed from
// a proxy node
func buildVirtualHostDomains(service *model.Service, port int, node model.Proxy) []string {
	domains := []string{service.Hostname.String(), fmt.Sprintf("%s:%d", service.Hostname, port)}
	domains = append(domains, generateAltVirtualHosts(service.Hostname.String(), port, node.Domain)...)

	if len(service.Addresses) > 0 {
		svcAddrs := service.GetServiceAddressesForProxy(&node)
		for _, svcAddr := range svcAddrs {
			// add a vhost match for the IP (if its non CIDR)
			cidr := util.ConvertAddressToCidr(svcAddr)
			if cidr.PrefixLen.Value == 32 {
				domains = append(domains, svcAddr)
				domains = append(domains, fmt.Sprintf("%s:%d", svcAddr, port))
			}
		}
	}
	return domains
}

// Given a service, and a port, this function generates all possible HTTP Host headers.
// For example, a service of the form foo.local.campus.net on port 80, with local domain "local.campus.net"
// could be accessed as http://foo:80 within the .local network, as http://foo.local:80 (by other clients
// in the campus.net domain), as http://foo.local.campus:80, etc.
// NOTE: When a sidecar in remote.campus.net domain is talking to foo.local.campus.net,
// we should only generate foo.local, foo.local.campus, etc (and never just "foo").
//
// - Given foo.local.campus.net on proxy domain local.campus.net, this function generates
// foo:80, foo.local:80, foo.local.campus:80, with and without ports. It will not generate
// foo.local.campus.net (full hostname) since its already added elsewhere.
//
// - Given foo.local.campus.net on proxy domain remote.campus.net, this function generates
// foo.local:80, foo.local.campus:80
//
// - Given foo.local.campus.net on proxy domain "" or proxy domain example.com, this
// function returns nil
func generateAltVirtualHosts(hostname string, port int, proxyDomain string) []string {
	var vhosts []string
	uniqHostname, sharedDNSDomain := getUniqueAndSharedDNSDomain(hostname, proxyDomain)

	// If there is no shared DNS name (e.g., foobar.com service on local.net proxy domain)
	// do not generate any alternate virtual host representations
	if len(sharedDNSDomain) == 0 {
		return nil
	}

	// adds the uniq piece foo, foo:80
	vhosts = append(vhosts, uniqHostname)
	vhosts = append(vhosts, fmt.Sprintf("%s:%d", uniqHostname, port))

	// adds all the other variants (foo.local, foo.local:80)
	for i := len(sharedDNSDomain) - 1; i > 0; i-- {
		if sharedDNSDomain[i] == '.' {
			variant := fmt.Sprintf("%s.%s", uniqHostname, sharedDNSDomain[:i])
			variantWithPort := fmt.Sprintf("%s:%d", variant, port)
			vhosts = append(vhosts, variant)
			vhosts = append(vhosts, variantWithPort)
		}
	}
	return vhosts
}

// mergeAllVirtualHosts across all ports. On routes for ports other than port 80,
// virtual hosts without an explicit port suffix (IP:PORT) should be stripped
func mergeAllVirtualHosts(vHostPortMap map[int][]route.VirtualHost) []route.VirtualHost {
	var virtualHosts []route.VirtualHost
	for p, vhosts := range vHostPortMap {
		if p == 80 {
			virtualHosts = append(virtualHosts, vhosts...)
		} else {
			for _, vhost := range vhosts {
				var newDomains []string
				for _, domain := range vhost.Domains {
					if strings.Contains(domain, ":") {
						newDomains = append(newDomains, domain)
					}
				}
				if len(newDomains) > 0 {
					vhost.Domains = newDomains
					virtualHosts = append(virtualHosts, vhost)
				}
			}
		}
	}
	return virtualHosts
}

// reverseArray returns its argument string array reversed
func reverseArray(r []string) []string {
	for i, j := 0, len(r)-1; i < len(r)/2; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return r
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// getUniqueAndSharedDNSDomain computes the unique and shared DNS suffix from a FQDN service name and
// the proxy's local domain with namespace. This is especially useful in Kubernetes environments, where
// a two services can have same name in different namespaces (e.g., foo.ns1.svc.cluster.local,
// foo.ns2.svc.cluster.local). In this case, if the proxy is in ns2.svc.cluster.local, then while
// generating alt virtual hosts for service foo.ns1 for the sidecars in ns2 namespace, we should generate
// foo.ns1, foo.ns1.svc, foo.ns1.svc.cluster.local and should not generate a virtual host called "foo" for
// foo.ns1 service.
// So given foo.ns1.svc.cluster.local and ns2.svc.cluster.local, this function will return
// foo.ns1, and svc.cluster.local.
// When given foo.ns2.svc.cluster.local and ns2.svc.cluster.local, this function will return
// foo, ns2.svc.cluster.local.
func getUniqueAndSharedDNSDomain(fqdnHostname, proxyDomain string) (string, string) {
	// split them by the dot and reverse the arrays, so that we can
	// start collecting the shared bits of DNS suffix.
	// E.g., foo.ns1.svc.cluster.local -> local,cluster,svc,ns1,foo
	//       ns2.svc.cluster.local -> local,cluster,svc,ns2
	partsFQDN := reverseArray(strings.Split(fqdnHostname, "."))
	partsProxyDomain := reverseArray(strings.Split(proxyDomain, "."))
	var sharedSuffixesInReverse []string // pieces shared between proxy and svc. e.g., local,cluster,svc

	for i := 0; i < min(len(partsFQDN), len(partsProxyDomain)); i++ {
		if partsFQDN[i] == partsProxyDomain[i] {
			sharedSuffixesInReverse = append(sharedSuffixesInReverse, partsFQDN[i])
		} else {
			break
		}
	}

	if len(sharedSuffixesInReverse) == 0 {
		return fqdnHostname, ""
	}

	// get the non shared pieces (ns1, foo) and reverse Array
	uniqHostame := strings.Join(reverseArray(partsFQDN[len(sharedSuffixesInReverse):]), ".")
	sharedSuffixes := strings.Join(reverseArray(sharedSuffixesInReverse), ".")
	return uniqHostame, sharedSuffixes
}
