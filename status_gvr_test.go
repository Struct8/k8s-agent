package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeMapper embrulha o DefaultRESTMapper pra poder contar Reset() e simular
// um CRD que só aparece depois da invalidação do cache.
type fakeMapper struct {
	real *meta.DefaultRESTMapper
	// Quando > 0, RESTMapping devolve NoMatch e decrementa a cada Reset --
	// reproduz "o CRD foi instalado depois que o agente subiu".
	hiddenUntil int
	resets       int
}

func (m *fakeMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.hiddenUntil > 0 {
		return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return m.real.RESTMapping(gk, versions...)
}

func (m *fakeMapper) Reset() {
	m.resets++
	if m.hiddenUntil > 0 {
		m.hiddenUntil--
	}
}

// Dois grupos declarando o MESMO Kind. É a razão de o apiVersion viajar junto
// na consulta: com kind sozinho, escolher entre os dois seria chute, e o
// errado devolveria HTTP 200 com o status de outro objeto.
func mapperComKindDuplicado() *fakeMapper {
	gvNosso := schema.GroupVersion{Group: "acme.example.com", Version: "v1"}
	gvOutro := schema.GroupVersion{Group: "outro.example.com", Version: "v1"}
	gvCore := schema.GroupVersion{Group: "", Version: "v1"}

	real := meta.NewDefaultRESTMapper([]schema.GroupVersion{gvNosso, gvOutro, gvCore})
	real.Add(gvNosso.WithKind("Widget"), meta.RESTScopeNamespace)
	real.Add(gvOutro.WithKind("Widget"), meta.RESTScopeNamespace)
	real.Add(gvCore.WithKind("Namespace"), meta.RESTScopeRoot)

	return &fakeMapper{real: real}
}

func TestResolveGVRDesambiguaPeloApiVersion(t *testing.T) {
	m := mapperComKindDuplicado()

	gvr, namespaced, err := resolveGVR("acme.example.com/v1", "Widget", m)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if gvr.Group != "acme.example.com" {
		t.Errorf("resolveu pro grupo errado: %q (o objeto consultado seria de outro operador)", gvr.Group)
	}
	if !namespaced {
		t.Errorf("Widget é namespaced, veio cluster-scoped")
	}

	// O outro grupo, mesmo Kind, tem que resolver pro OUTRO recurso.
	gvrOutro, _, err := resolveGVR("outro.example.com/v1", "Widget", m)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if gvrOutro.Group == gvr.Group {
		t.Errorf("os dois apiVersion resolveram pro mesmo grupo %q -- a desambiguação não está acontecendo", gvr.Group)
	}
}

func TestResolveGVRDevolveEscopoDoCluster(t *testing.T) {
	// Namespace é cluster-scoped. O frontend resolve namespace subindo a
	// hierarquia do diagrama e pode mandar um preenchido; quem decide é isto.
	_, namespaced, err := resolveGVR("v1", "Namespace", mapperComKindDuplicado())
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if namespaced {
		t.Errorf("Namespace veio como namespaced -- a consulta iria pro caminho errado da API")
	}
}

func TestResolveGVRSemApiVersionUsaMapaLegado(t *testing.T) {
	// Worker antigo ainda no ar: consulta chega sem apiVersion.
	gvr, namespaced, err := resolveGVR("", "Deployment", mapperComKindDuplicado())
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if gvr.Group != "apps" || gvr.Resource != "deployments" {
		t.Errorf("caminho legado resolveu errado: %+v", gvr)
	}
	if !namespaced {
		t.Errorf("os oito kinds do mapa legado são namespaced")
	}
}

func TestResolveGVRSemApiVersionEForaDoMapaFalha(t *testing.T) {
	// Tem que falhar, não adivinhar: quem chama transforma isso em
	// "unknown_kind", que é uma resposta honesta.
	if _, _, err := resolveGVR("", "HTTPRoute", mapperComKindDuplicado()); err == nil {
		t.Fatalf("kind fora do mapa legado e sem apiVersion deveria falhar")
	}
}

func TestResolveGVRReconsultaDepoisDeCrdNovo(t *testing.T) {
	// CRD instalado depois que o agente subiu -- todo sync do Argo CD pode
	// fazer isso. Sem o Reset, o kind responderia "não existe" até alguém
	// reiniciar o Pod.
	m := mapperComKindDuplicado()
	m.hiddenUntil = 1

	gvr, _, err := resolveGVR("acme.example.com/v1", "Widget", m)
	if err != nil {
		t.Fatalf("não resolveu depois do Reset: %v", err)
	}
	if m.resets != 1 {
		t.Errorf("Reset chamado %d vez(es), esperado 1", m.resets)
	}
	if gvr.Resource == "" {
		t.Errorf("resolveu pra recurso vazio")
	}
}

func TestResolveGVRNaoInsisteQuandoOKindNaoExiste(t *testing.T) {
	// Ausência de verdade: uma tentativa de Reset e para. Sem esse teto, um
	// kind inexistente custaria uma rodada completa de discovery por consulta,
	// e a lista de status tem dezenas delas.
	m := mapperComKindDuplicado()
	m.hiddenUntil = 99

	if _, _, err := resolveGVR("acme.example.com/v1", "Widget", m); err == nil {
		t.Fatalf("kind ausente deveria falhar")
	}
	if m.resets != 1 {
		t.Errorf("Reset chamado %d vez(es), esperado exatamente 1", m.resets)
	}
}
