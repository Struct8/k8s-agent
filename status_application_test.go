package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// O Application do Argo CD é o único tipo do catálogo que já carrega um veredito
// pronto: `status.health` é o julgamento do controller sobre TUDO que aquele
// Application implantou. Existir no apiserver não vale nada aqui -- um
// Application aplicado é um pedido, não um resultado, e um bundle inteiro pode
// estar fora do ar com o objeto intacto.

func application(health, sync, revision string) *unstructured.Unstructured {
	st := map[string]interface{}{}
	if health != "" {
		st["health"] = map[string]interface{}{"status": health}
	}
	if sync != "" || revision != "" {
		s := map[string]interface{}{}
		if sync != "" {
			s["status"] = sync
		}
		if revision != "" {
			s["revision"] = revision
		}
		st["sync"] = s
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]interface{}{"name": "kc", "namespace": "argocd"},
		"status":     st,
	}}
}

func TestApplicationSaudavelEstaImplantado(t *testing.T) {
	r := summarize("Application", application("Healthy", "Synced", "a1b2c3d"))

	if !r.Deployed {
		t.Errorf("Application Healthy veio como não implantado")
	}
	if r.Status != "Healthy" {
		t.Errorf("status = %q, esperado \"Healthy\"", r.Status)
	}
	if r.Outputs["sync"] != "Synced" || r.Outputs["revision"] != "a1b2c3d" {
		t.Errorf("sync e revisão não chegaram em outputs: %v", r.Outputs)
	}
}

func TestApplicationDegradadoNaoEstaImplantado(t *testing.T) {
	// O objeto continua lá, o apply continua limpo, e o que ele implantou está
	// caído. Sem ler health este caso passaria como implantado.
	r := summarize("Application", application("Degraded", "Synced", ""))

	if r.Deployed {
		t.Errorf("Application Degraded veio como implantado")
	}
	if r.Status != "Degraded" {
		t.Errorf("status = %q -- o próprio health do Argo CD nomeia o estado melhor que qualquer rótulo nosso", r.Status)
	}
}

func TestApplicationForaDeSyncContinuaImplantado(t *testing.T) {
	// Deriva NÃO é indisponibilidade. O que está no cluster está saudável e
	// servindo; o que divergiu foi o Git. Apagar o nó por causa disso ensinaria o
	// usuário a ignorar nó apagado.
	r := summarize("Application", application("Healthy", "OutOfSync", ""))

	if !r.Deployed {
		t.Errorf("Healthy com OutOfSync veio como não implantado -- deriva não é queda")
	}
	if r.Outputs["sync"] != "OutOfSync" {
		t.Errorf("a deriva sumiu de outputs: %v", r.Outputs)
	}
}

func TestApplicationSemStatusFicaPendente(t *testing.T) {
	// Recém-criado, antes da primeira reconciliação.
	r := summarize("Application", application("", "", ""))

	if r.Deployed {
		t.Errorf("Application sem status veio como implantado")
	}
	if r.Status != "Pending" {
		t.Errorf("status = %q, esperado \"Pending\"", r.Status)
	}
}

func TestApplicationMissingNaoEstaImplantado(t *testing.T) {
	// `Missing` é o Argo CD dizendo que o que ele deveria ter criado não está
	// lá -- o caso mais próximo de "não implantado" que este tipo tem.
	r := summarize("Application", application("Missing", "OutOfSync", ""))

	if r.Deployed {
		t.Errorf("Application Missing veio como implantado")
	}
	if r.Status != "Missing" {
		t.Errorf("status = %q, esperado \"Missing\"", r.Status)
	}
}

func TestApplicationSuspensoNaoContaComoImplantado(t *testing.T) {
	// `Suspended` é o Argo CD dizendo que a reconciliação está parada. Tratar
	// como saudável esconderia justamente o estado em que nada avança.
	r := summarize("Application", application("Suspended", "Synced", ""))

	if r.Deployed {
		t.Errorf("Application Suspended veio como implantado")
	}
	if r.Status != "Suspended" {
		t.Errorf("status = %q, esperado \"Suspended\"", r.Status)
	}
}
