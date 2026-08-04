package main

import (
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Um único client dinâmico serve tanto as consultas de STATUS (recursos
// nativos -- Pod/Deployment/DaemonSet/StatefulSet/Job/CronJob) quanto as de
// MÉTRICAS (metrics.k8s.io, servido pelo metrics-server já instalado no
// cluster do cliente -- ver README.md). Evita depender do módulo
// k8s.io/metrics só pra tipar PodMetrics/NodeMetrics: o schema de
// metrics.k8s.io é simples o bastante pra ler via unstructured.
func buildDynamicClient() (dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(config)
}
