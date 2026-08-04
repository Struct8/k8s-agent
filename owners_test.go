package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// Cada caso aqui é um ramo de Resolve que, errado, produz série com chave que o
// gráfico nunca consulta -- e o sintoma é sempre o mesmo: HTTP 200 com série
// vazia, sem erro em lugar nenhum. Por isso o teste afirma o par kind/name
// exato, não só "resolveu alguma coisa".

func ptrBool(b bool) *bool { return &b }

func obj(apiVersion, kind, namespace, name string, owners ...metav1.OwnerReference) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	if len(owners) > 0 {
		u.SetOwnerReferences(owners)
	}
	return u
}

func controllerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name, Controller: ptrBool(true)}
}

func newResolverWith(objects ...runtime.Object) *ownerResolver {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		podGVR:        "PodList",
		replicaSetGVR: "ReplicaSetList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)
	return newOwnerResolver(client)
}

func TestResolveOwnerChains(t *testing.T) {
	objects := []runtime.Object{
		// Pod -> ReplicaSet -> Deployment: a cadeia de dois hops, que é o caso
		// esmagadoramente mais comum e o que estava quebrado.
		obj("v1", "Pod", "app", "checkout-7d9f8b6c4-x2klm", controllerRef("ReplicaSet", "checkout-7d9f8b6c4")),
		obj("apps/v1", "ReplicaSet", "app", "checkout-7d9f8b6c4", controllerRef("Deployment", "checkout")),

		// StatefulSet: um hop só, sem ReplicaSet no meio.
		obj("v1", "Pod", "app", "postgres-0", controllerRef("StatefulSet", "postgres")),

		// Pod avulso, sem dono nenhum.
		obj("v1", "Pod", "app", "debug-shell"),

		// ReplicaSet sem Deployment acima: o próprio ReplicaSet é o workload.
		obj("v1", "Pod", "app", "legacy-rs-abcde", controllerRef("ReplicaSet", "legacy-rs")),
		obj("apps/v1", "ReplicaSet", "app", "legacy-rs"),

		// Só referência NÃO-controladora: não é dono para efeito de atribuição.
		obj("v1", "Pod", "app", "adopted", metav1.OwnerReference{Kind: "Deployment", Name: "nao-controla"}),
	}

	resolver := newResolverWith(objects...)

	keys := []objectKey{
		{Namespace: "app", Name: "checkout-7d9f8b6c4-x2klm"},
		{Namespace: "app", Name: "postgres-0"},
		{Namespace: "app", Name: "debug-shell"},
		{Namespace: "app", Name: "legacy-rs-abcde"},
		{Namespace: "app", Name: "adopted"},
		// Existe em metrics.k8s.io e não existe mais como Pod: morreu no meio
		// do ciclo, o uso dele não é de ninguém.
		{Namespace: "app", Name: "ja-morreu"},
	}

	resolved, unresolved, err := resolver.Resolve(context.Background(), keys)
	if err != nil {
		t.Fatalf("Resolve devolveu erro: %v", err)
	}

	want := map[objectKey]workloadRef{
		{Namespace: "app", Name: "checkout-7d9f8b6c4-x2klm"}: {Kind: "Deployment", Name: "checkout"},
		{Namespace: "app", Name: "postgres-0"}:               {Kind: "StatefulSet", Name: "postgres"},
		{Namespace: "app", Name: "debug-shell"}:              {Kind: "Pod", Name: "debug-shell"},
		{Namespace: "app", Name: "legacy-rs-abcde"}:          {Kind: "ReplicaSet", Name: "legacy-rs"},
		{Namespace: "app", Name: "adopted"}:                  {Kind: "Pod", Name: "adopted"},
	}

	for key, expected := range want {
		got, ok := resolved[key]
		if !ok {
			t.Errorf("%s: não resolvido, esperava %s#%s", key.Name, expected.Kind, expected.Name)
			continue
		}
		if got != expected {
			t.Errorf("%s: resolveu %s#%s, esperava %s#%s", key.Name, got.Kind, got.Name, expected.Kind, expected.Name)
		}
	}

	if len(unresolved) != 1 || unresolved[0].Name != "ja-morreu" {
		t.Errorf("esperava exatamente 'ja-morreu' como não resolvido, veio %v", unresolved)
	}
	if _, ok := resolved[objectKey{Namespace: "app", Name: "ja-morreu"}]; ok {
		t.Error("Pod inexistente não pode receber workload chutado -- viraria série indistinguível de dado bom")
	}
}

// O cache só vale se sobreviver entre ciclos: é ele que mantém o custo em
// regime estável igual ao de antes da mudança (uma listagem por ciclo).
func TestResolveNaoRelistaSemPodNovo(t *testing.T) {
	resolver := newResolverWith(
		obj("v1", "Pod", "app", "web-1", controllerRef("StatefulSet", "web")),
	)
	keys := []objectKey{{Namespace: "app", Name: "web-1"}}

	if _, _, err := resolver.Resolve(context.Background(), keys); err != nil {
		t.Fatalf("primeira resolução falhou: %v", err)
	}

	// Esvaziar o cluster: se a segunda chamada listasse de novo, o Pod sumiria
	// do mapa e voltaria como não resolvido.
	resolver.client = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podGVR: "PodList", replicaSetGVR: "ReplicaSetList"},
	)

	resolved, unresolved, err := resolver.Resolve(context.Background(), keys)
	if err != nil {
		t.Fatalf("segunda resolução falhou: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("houve releitura sem Pod novo: %v", unresolved)
	}
	if got := resolved[keys[0]]; got.Kind != "StatefulSet" || got.Name != "web" {
		t.Errorf("cache devolveu %s#%s", got.Kind, got.Name)
	}
}

func TestSanitizePointsEspelhaOWorker(t *testing.T) {
	long := strings.Repeat("a", 253)
	// Caractere diferente do válido de propósito: se o código truncasse em vez
	// de descartar, sobraria um nome de 253 "b" -- distinguível do de 253 "a".
	tooLong := strings.Repeat("b", 254)

	points := []metricPoint{
		{Metric: "cpu", Namespace: "app", Kind: "Deployment", Name: "checkout", Value: 1},
		// 253 é o limite de nome de objeto no Kubernetes: tem que PASSAR. Era
		// isto que o limite antigo de 64 barrava, derrubando o cluster inteiro.
		{Metric: "cpu", Namespace: "app", Kind: "Deployment", Name: long, Value: 1},
		{Metric: "cpu", Namespace: "app", Kind: "Deployment", Name: tooLong, Value: 1},
		// namespace é label RFC-1123: teto 63, não 253.
		{Metric: "cpu", Namespace: strings.Repeat("n", 64), Kind: "Deployment", Name: "x", Value: 1},
		{Metric: "cpu", Namespace: "app", Kind: "Deployment", Name: "tem espaco", Value: 1},
	}

	kept, dropped := sanitizePoints(points)

	if len(kept) != 2 {
		t.Errorf("esperava 2 pontos mantidos, veio %d", len(kept))
	}
	if len(dropped) != 3 {
		t.Errorf("esperava 3 pontos descartados, veio %d: %v", len(dropped), dropped)
	}
	if len(kept) > 1 && kept[1].Name != long {
		t.Error("nome de 253 caracteres foi descartado -- é um nome válido no Kubernetes")
	}
	// Descartar, nunca truncar: nome cortado não casa com o que o gráfico
	// consulta e gravaria série que parece boa e nunca é lida.
	for _, p := range kept {
		if strings.HasPrefix(p.Name, "b") {
			t.Errorf("ponto foi truncado em vez de descartado: %q", p.Name)
		}
	}
}
