package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CRDs de operador para os quais "existe" é toda a verdade disponível.
//
// O agente NÃO tem caso próprio para nenhum deles, e isso é a decisão -- não um
// esquecimento. Quem sabe se o que está por trás de um Grafana ou de um
// GatewayConfiguration ficou pronto é o operador, e cada um publica isso de um
// jeito; inventar nome de condition aqui produziria "Pending" eterno sobre
// recurso saudável, que é pior do que dizer "existe".
//
// Estes testes fixam esse contrato: se alguém acrescentar um caso no summarize
// para um destes kinds sem trazer a leitura certa do operador, o teste avisa.

func objetoDe(kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"kind":     kind,
		"metadata": map[string]interface{}{"name": "x", "namespace": "infra", "uid": "u-1"},
		// Um status QUALQUER, e ainda assim a resposta é "existe": o agente não
		// tenta interpretar o que não conhece.
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": "False", "reason": "DependenciesNotReady"},
		}},
	}}
}

func TestCrdDeOperadorRespondePresent(t *testing.T) {
	for _, kind := range []string{"Grafana", "Prometheus", "Queue", "GatewayConfiguration", "KongClusterPlugin"} {
		r := summarize(kind, objetoDe(kind))
		if !r.Deployed {
			t.Errorf("%s: existir no cluster deveria contar como implantado", kind)
		}
		if r.Status != "Present" {
			t.Errorf("%s: status = %q, esperado \"Present\"", kind, r.Status)
		}
		if r.UID != "u-1" {
			t.Errorf("%s: o UID não chegou ao resultado", kind)
		}
	}
}

func TestPilhaDePluginsAgregaOsObjetos(t *testing.T) {
	// Um nó de pilha vira um KongClusterPlugin por plugin ligado. Todos
	// presentes = a pilha está aplicada.
	itens := []unstructured.Unstructured{*objetoDe("KongClusterPlugin"), *objetoDe("KongClusterPlugin")}
	r := summarizeGroup("KongClusterPlugin", itens)

	if !r.Deployed {
		t.Errorf("dois plugins presentes vieram como não implantados")
	}
	if r.Outputs["matched"] != 2 {
		t.Errorf("matched = %v, esperado 2", r.Outputs["matched"])
	}
}

func TestPilhaSemNenhumPluginFicaTransparente(t *testing.T) {
	// Nenhum objeto casou: ou a pilha nunca foi aplicada, ou é anterior à label.
	// Nos dois casos a resposta honesta é a mesma que a de recurso ausente.
	r := summarizeGroup("KongClusterPlugin", nil)
	if r.Deployed || r.Status != "" {
		t.Errorf("grupo vazio deveria ficar transparente, veio deployed=%v status=%q", r.Deployed, r.Status)
	}
}
