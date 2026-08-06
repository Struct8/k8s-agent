package main

import (
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// The dynamic client serves STATUS queries, and only those: any kind, native or
// custom, resolved through the RESTMapper below rather than through a typed
// client generated per group.
//
// Metrics do not pass through here at all since 0.4.0 -- a chart is forwarded to
// Prometheus over plain HTTP (see prometheus.go) and needs no Kubernetes
// permission, which is why `metrics.k8s.io` is absent from the ClusterRole.
func buildDynamicClient() (dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(config)
}

// RESTMapper that translates (group, version, kind) into the REST resource by
// asking the cluster itself. It is what allows querying a CRD without a new
// agent release: the new type arrives in the query carrying its apiVersion, and
// the mapping comes out of the discovery API.
//
// Discovery needs no RBAC of its own -- the system:discovery binding already
// grants it to every authenticated user.
//
// The in-memory cache is deliberate (memory.NewMemCacheClient): without it,
// every query would do a full discovery round, which is dozens of requests. The
// price is the cache going stale when a CRD is installed later; what pays that
// is Reset() in resolveGVR, called only on the error path.
//
// meta.RESTMapper is deliberately not the returned type: resolveGVR needs
// Reset(), which only exists on the concrete one.
func buildRESTMapper() (*restmapper.DeferredDiscoveryRESTMapper, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient)), nil
}

// Compile-time check that the mapper returned above serves resolveGVR -- if
// either signature changes, this breaks here and not in production.
var _ restMapper = (*restmapper.DeferredDiscoveryRESTMapper)(nil)
