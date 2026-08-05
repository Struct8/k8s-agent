package main

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// Gateway e HTTPRoute são os dois tipos onde "o objeto existe" não é resposta
// nenhuma. Os dois aplicam limpo, sincronizam verde no Argo CD e podem estar
// completamente inertes -- Gateway sem controller que o assuma, HTTPRoute que
// nenhum Gateway reivindicou. O sinal está em `status.conditions`, e é ele que
// estes testes cobrem.

func condOf(tipo, status, reason string) map[string]interface{} {
	return map[string]interface{}{"type": tipo, "status": status, "reason": reason}
}

func gatewayComConditions(conds ...map[string]interface{}) *unstructured.Unstructured {
	lista := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		lista = append(lista, c)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]interface{}{"name": "gapi", "namespace": "infra"},
		"status": map[string]interface{}{
			"conditions": lista,
			"addresses":  []interface{}{map[string]interface{}{"type": "Hostname", "value": "k8s-abc.elb.amazonaws.com"}},
		},
	}}
}

func rotaComParents(parents ...interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]interface{}{"name": "route2-wordpress", "namespace": "infra"},
		"status":     map[string]interface{}{"parents": parents},
	}}
}

func parentComConditions(conds ...map[string]interface{}) interface{} {
	lista := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		lista = append(lista, c)
	}
	return map[string]interface{}{
		"controllerName": "konghq.com/gateway-operator",
		"conditions":     lista,
	}
}

func TestGatewayProgramadoEstaImplantado(t *testing.T) {
	r := summarize("Gateway", gatewayComConditions(
		condOf("Accepted", "True", ""),
		condOf("Programmed", "True", ""),
	))

	if !r.Deployed {
		t.Errorf("Gateway com Accepted e Programmed em True veio como não implantado")
	}
	if r.Status != "Programmed" {
		t.Errorf("status = %q, esperado \"Programmed\"", r.Status)
	}
	// O endereço é a resposta da pergunta seguinte ("por onde eu entro?") e o
	// painel já sabe mostrar outputs.
	if r.Outputs["address"] != "k8s-abc.elb.amazonaws.com" {
		t.Errorf("endereço do Gateway não chegou em outputs: %v", r.Outputs["address"])
	}
}

func TestGatewaySemListenerValidoNaoEstaImplantado(t *testing.T) {
	// Caso real: porta em conflito entre dois listeners, ou certificado que não
	// resolve. O Gateway existe, o apply passou, e a entrada não responde.
	r := summarize("Gateway", gatewayComConditions(
		condOf("Accepted", "True", ""),
		condOf("Programmed", "False", "NoValidListeners"),
	))

	if r.Deployed {
		t.Errorf("Gateway com Programmed=False veio como implantado -- é exatamente o nó verde sobre entrada morta")
	}
	if r.Status != "NoValidListeners" {
		t.Errorf("status = %q -- esperado o `reason` do Kubernetes, que é quem nomeia a causa", r.Status)
	}
}

func TestGatewaySemConditionsFicaPendente(t *testing.T) {
	// Recém-aplicado: o controller ainda não visitou. Chamar isso de falha faria
	// todo apply piscar vermelho por alguns segundos.
	r := summarize("Gateway", gatewayComConditions())
	if r.Deployed {
		t.Errorf("Gateway sem conditions veio como implantado")
	}
	if r.Status != "Pending" {
		t.Errorf("status = %q, esperado \"Pending\"", r.Status)
	}
}

func TestGatewayAceitoMasNaoProgramadoNaoBasta(t *testing.T) {
	// `Accepted` sozinho quer dizer só que um controller assumiu a
	// responsabilidade -- o load balancer por trás pode não existir ainda.
	r := summarize("Gateway", gatewayComConditions(condOf("Accepted", "True", "")))
	if r.Deployed {
		t.Errorf("Accepted sem Programmed veio como implantado")
	}
}

func TestRotaSemParentNaoEstaAnexada(t *testing.T) {
	// O modo de falha mais caro deste tipo: objeto aplicado, apply limpo, Argo CD
	// verde, e nenhum Gateway a reivindicou. Na entrada isso é 404 sobre uma
	// rota que existe.
	r := summarize("HTTPRoute", rotaComParents())

	if r.Deployed {
		t.Errorf("HTTPRoute sem nenhum parent veio como implantada")
	}
	if r.Status != "NotAttached" {
		t.Errorf("status = %q, esperado \"NotAttached\"", r.Status)
	}
}

func TestRotaAceitaComRefsResolvidas(t *testing.T) {
	r := summarize("HTTPRoute", rotaComParents(parentComConditions(
		condOf("Accepted", "True", ""),
		condOf("ResolvedRefs", "True", ""),
	)))

	if !r.Deployed {
		t.Errorf("HTTPRoute aceita e com refs resolvidas veio como não implantada")
	}
	if r.Status != "Accepted" {
		t.Errorf("status = %q, esperado \"Accepted\"", r.Status)
	}
}

func TestRotaComBackendInexistenteNaoEstaImplantada(t *testing.T) {
	// Aceita pelo Gateway, apontando pra um Service que não existe. Aplica
	// limpo, sincroniza verde, devolve 503. Sem ResolvedRefs este caso passaria.
	r := summarize("HTTPRoute", rotaComParents(parentComConditions(
		condOf("Accepted", "True", ""),
		condOf("ResolvedRefs", "False", "BackendNotFound"),
	)))

	if r.Deployed {
		t.Errorf("HTTPRoute com backend inexistente veio como implantada")
	}
	if r.Status != "BackendNotFound" {
		t.Errorf("status = %q, esperado \"BackendNotFound\"", r.Status)
	}
}

func TestRotaOlhaTodosOsParents(t *testing.T) {
	// A mesma rota pode ser aceita por um Gateway e recusada por outro. Parar no
	// primeiro daria verde pra quem entra pelo segundo.
	r := summarize("HTTPRoute", rotaComParents(
		parentComConditions(condOf("Accepted", "True", ""), condOf("ResolvedRefs", "True", "")),
		parentComConditions(condOf("Accepted", "False", "NotAllowedByListeners"), condOf("ResolvedRefs", "True", "")),
	))

	if r.Deployed {
		t.Errorf("segundo parent recusado e o resultado veio implantado -- a varredura parou no primeiro")
	}
	if r.Status != "NotAllowedByListeners" {
		t.Errorf("status = %q, esperado o reason do parent que falhou", r.Status)
	}
}

// --- Agregação por seletor -------------------------------------------------
//
// Um nó Route vira N HTTPRoutes. Estes testes fixam como os N viram um.

func rotaNomeada(nome string, deployed bool) unstructured.Unstructured {
	c := condOf("ResolvedRefs", "False", "BackendNotFound")
	if deployed {
		c = condOf("ResolvedRefs", "True", "")
	}
	obj := rotaComParents(parentComConditions(condOf("Accepted", "True", ""), c))
	obj.SetName(nome)
	return *obj
}

func TestGrupoVazioFicaTransparente(t *testing.T) {
	// Nenhum objeto casou com o seletor. A resposta honesta é a mesma de
	// NotFound: não existe no cluster nada que este nó represente. Status vazio
	// é o que o frontend desenha como transparente -- não como erro.
	r := summarizeGroup("HTTPRoute", nil)
	if r.Deployed {
		t.Errorf("grupo vazio veio como implantado")
	}
	if r.Status != "" {
		t.Errorf("status = %q, esperado vazio (mesmo desenho de NotFound)", r.Status)
	}
	if r.Outputs["matched"] != 0 {
		t.Errorf("matched = %v, esperado 0", r.Outputs["matched"])
	}
}

func TestGrupoDeUmRepassaOStatusIndividual(t *testing.T) {
	// Caso comum: uma rota, um destino. "BackendNotFound" diz mais que "0/1".
	r := summarizeGroup("HTTPRoute", []unstructured.Unstructured{rotaNomeada("route2-wordpress", false)})
	if r.Deployed {
		t.Errorf("grupo de um objeto quebrado veio como implantado")
	}
	if r.Status != "BackendNotFound" {
		t.Errorf("status = %q, esperado o status do objeto único", r.Status)
	}
	if r.Outputs["matched"] != 1 {
		t.Errorf("matched = %v, esperado 1", r.Outputs["matched"])
	}
}

func TestGrupoExigeTodosImplantados(t *testing.T) {
	// Três destinos, um sem backend. Quem entra por aquele caminho recebe 503, e
	// um nó aceso mentiria exatamente pra essa pessoa.
	itens := []unstructured.Unstructured{
		rotaNomeada("route2-wordpress", true),
		rotaNomeada("route2-syncope", false),
		rotaNomeada("route2-agent", true),
	}
	r := summarizeGroup("HTTPRoute", itens)

	if r.Deployed {
		t.Errorf("dois de três prontos veio como implantado")
	}
	if r.Status != "2/3" {
		t.Errorf("status = %q, esperado \"2/3\"", r.Status)
	}

	// Qual das três falhou é a pergunta seguinte, e o painel já mostra outputs.
	objetos, ok := r.Outputs["objects"].([]interface{})
	if !ok || len(objetos) != 3 {
		t.Fatalf("outputs[objects] não trouxe os três objetos: %v", r.Outputs["objects"])
	}
	quebrado := objetos[1].(map[string]interface{})
	if quebrado["name"] != "route2-syncope" || quebrado["deployed"] != false {
		t.Errorf("o objeto quebrado não está identificado em outputs: %v", quebrado)
	}
}

func TestGrupoTodoProntoEstaImplantado(t *testing.T) {
	itens := []unstructured.Unstructured{
		rotaNomeada("route2-a", true),
		rotaNomeada("route2-b", true),
	}
	r := summarizeGroup("HTTPRoute", itens)
	if !r.Deployed {
		t.Errorf("dois de dois prontos veio como não implantado")
	}
	if r.Status != "2/2" {
		t.Errorf("status = %q, esperado \"2/2\"", r.Status)
	}
}

// --- O caminho completo, do pedido até o resultado --------------------------

func mapperComGatewayAPI() *fakeMapper {
	gvGw := schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
	real := meta.NewDefaultRESTMapper([]schema.GroupVersion{gvGw})
	real.Add(gvGw.WithKind("HTTPRoute"), meta.RESTScopeNamespace)
	real.Add(gvGw.WithKind("Gateway"), meta.RESTScopeNamespace)
	return &fakeMapper{real: real}
}

func TestResolveStatusPorSeletorListaEAgrega(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

	minha1 := rotaNomeada("route2-wordpress", true)
	minha1.SetLabels(map[string]string{"struct8.io/origin": "route2"})
	minha1.SetNamespace("infra")
	minha2 := rotaNomeada("route2-syncope", false)
	minha2.SetLabels(map[string]string{"struct8.io/origin": "route2"})
	minha2.SetNamespace("infra")
	// De outro nó Route, na mesma namespace: não pode entrar na conta.
	alheia := rotaNomeada("outra-x", false)
	alheia.SetLabels(map[string]string{"struct8.io/origin": "outra"})
	alheia.SetNamespace("infra")

	esquema := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		esquema,
		map[schema.GroupVersionResource]string{gvr: "HTTPRouteList"},
		&minha1, &minha2, &alheia,
	)

	r := resolveStatus(context.Background(), client, mapperComGatewayAPI(), statusResourceQuery{
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       "HTTPRoute",
		Namespace:  "infra",
		List:       true,
		Selector:   "struct8.io/origin=route2",
	})

	if r.Outputs["matched"] != 2 {
		t.Fatalf("matched = %v, esperado 2 -- o seletor pegou objeto de outro nó ou perdeu um dos meus", r.Outputs["matched"])
	}
	if r.Deployed {
		t.Errorf("uma das duas está quebrada e o resultado veio implantado")
	}
	if r.Status != "1/2" {
		t.Errorf("status = %q, esperado \"1/2\"", r.Status)
	}
}

func TestResolveStatusSemNomeESemPedidoDeListaNaoInventa(t *testing.T) {
	// Consulta malformada tem que virar erro visível, não "não implantado" --
	// que é indistinguível de um recurso que realmente não subiu. E, agora que
	// existe lista sem seletor, também não pode virar uma varredura acidental do
	// namespace inteiro: por isso `list` é explícito.
	esquema := runtime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		esquema,
		map[schema.GroupVersionResource]string{gvr: "HTTPRouteList"},
	)

	r := resolveStatus(context.Background(), client, mapperComGatewayAPI(), statusResourceQuery{
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       "HTTPRoute",
		Namespace:  "infra",
	})

	if r.Status != "error" {
		t.Errorf("status = %q, esperado \"error\"", r.Status)
	}
}

func TestResolveStatusListaNamespaceInteiraSemSeletor(t *testing.T) {
	// É assim que se pergunta por um operador instalado por Helm: o nome do
	// Deployment sai dos templates de um chart de terceiros, mas a namespace é
	// criada pelo próprio release e só tem aquilo dentro.
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	gvApps := schema.GroupVersion{Group: "apps", Version: "v1"}
	real := meta.NewDefaultRESTMapper([]schema.GroupVersion{gvApps})
	real.Add(gvApps.WithKind("Deployment"), meta.RESTScopeNamespace)
	mapper := &fakeMapper{real: real}

	// Nome que ninguém do nosso lado teria como prever.
	op := deploymentPronto("kong-operator-controller-manager", "kong-operator-system", 1, 1)
	// Segundo workload na mesma namespace, ainda subindo: o release não está
	// pronto, e responder "implantado" por causa do primeiro seria mentira.
	aux := deploymentPronto("kong-operator-webhook", "kong-operator-system", 2, 1)
	// De outra namespace: não pode entrar na conta.
	alheio := deploymentPronto("outro", "default", 1, 1)

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "DeploymentList"},
		op, aux, alheio,
	)

	r := resolveStatus(context.Background(), client, mapper, statusResourceQuery{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Namespace:  "kong-operator-system",
		List:       true,
	})

	if r.Outputs["matched"] != 2 {
		t.Fatalf("matched = %v, esperado 2 -- a lista pegou fora da namespace ou perdeu um", r.Outputs["matched"])
	}
	if r.Deployed {
		t.Errorf("um dos dois workloads não está pronto e o operador veio como implantado")
	}
	if r.Status != "1/2" {
		t.Errorf("status = %q, esperado \"1/2\"", r.Status)
	}
}

func deploymentPronto(nome, ns string, desejadas, prontas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": nome, "namespace": ns},
		"spec":       map[string]interface{}{"replicas": desejadas},
		"status":     map[string]interface{}{"readyReplicas": prontas},
	}}
}
