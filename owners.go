package main

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Atribuição de uso de Pod ao WORKLOAD dono dele.
//
// WHY THIS EXISTS: a chart asks for "Deployment#checkout-api"; metrics-server
// only speaks in Pods. Without this translation the agent recorded
// "Pod#checkout-api-7d9f8b6c4-x2klm" and the query never matched -- it returned
// HTTP 200 with an empty series, which renders as a blank chart and reports an
// error nowhere.
//
// POR QUE PRECISA LISTAR PODS: o objeto de metrics.k8s.io carrega só nome e
// namespace, nunca ownerReferences. O dono só existe no Pod de verdade, daí a
// segunda listagem -- e o ReplicaSet no meio (Pod -> ReplicaSet -> Deployment)
// exige uma terceira.
//
// POR QUE TEM CACHE: essas listas são caras (um Pod traz spec e status
// inteiros; um ReplicaSet traz o template do Pod, e o histórico padrão guarda
// 10 revisões por Deployment). Como ownerReferences é imutável pela vida do
// objeto, o cache é sempre válido: em regime estável nenhuma das duas listagens
// acontece, e só um Pod novo (rollout, escala) provoca uma releitura.
var (
	podGVR        = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	replicaSetGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
)

// objectKey identifica um objeto namespaced.
type objectKey struct {
	Namespace string
	Name      string
}

// workloadRef é o par kind/name que vira `blob3` no Analytics Engine.
type workloadRef struct {
	Kind string
	Name string
}

type ownerResolver struct {
	client dynamic.Interface

	// Dono CONTROLADOR do Pod, como está no objeto -- pode ser um ReplicaSet,
	// que ainda precisa do segundo hop. Guardado cru de propósito: assim uma
	// releitura de ReplicaSet não obriga a reler Pods também.
	podOwner map[objectKey]workloadRef

	// Dono controlador do ReplicaSet (normalmente o Deployment).
	rsOwner map[objectKey]workloadRef
}

func newOwnerResolver(client dynamic.Interface) *ownerResolver {
	return &ownerResolver{
		client:   client,
		podOwner: map[objectKey]workloadRef{},
		rsOwner:  map[objectKey]workloadRef{},
	}
}

// controllerOwner devolve o dono marcado como controlador.
//
// Só o que tem `controller: true` vale. Um objeto pode ter várias
// ownerReferences e apenas uma é o controlador; pegar a primeira da lista
// atribuiria o uso a quem só tem referência incidental.
func controllerOwner(obj *unstructured.Unstructured) (workloadRef, bool) {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return workloadRef{Kind: ref.Kind, Name: ref.Name}, true
		}
	}
	return workloadRef{}, false
}

// ResourceVersion "0" faz o apiserver responder do cache de watch em vez de ler
// o etcd com quórum. A defasagem que isso pode trazer não importa aqui: se um
// Pod acabou de nascer e ainda não aparece, ele entra no ciclo seguinte.
func listAll(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource) (*unstructured.UnstructuredList, error) {
	return client.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
}

// refreshPods reconstrói o mapa inteiro, não só o que faltou. É isso que dá o
// descarte de Pod morto de graça -- sem isso o mapa cresceria para sempre num
// cluster que faz rollout com frequência.
func (r *ownerResolver) refreshPods(ctx context.Context) error {
	list, err := listAll(ctx, r.client, podGVR)
	if err != nil {
		return err
	}
	fresh := make(map[objectKey]workloadRef, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		key := objectKey{Namespace: item.GetNamespace(), Name: item.GetName()}
		if owner, ok := controllerOwner(item); ok {
			fresh[key] = owner
		} else {
			// Pod avulso (kubectl run, um Pod escrito à mão). O dono é ele mesmo.
			fresh[key] = workloadRef{Kind: "Pod", Name: item.GetName()}
		}
	}
	r.podOwner = fresh
	return nil
}

func (r *ownerResolver) refreshReplicaSets(ctx context.Context) error {
	list, err := listAll(ctx, r.client, replicaSetGVR)
	if err != nil {
		return err
	}
	fresh := make(map[objectKey]workloadRef, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		key := objectKey{Namespace: item.GetNamespace(), Name: item.GetName()}
		if owner, ok := controllerOwner(item); ok {
			fresh[key] = owner
		} else {
			// ReplicaSet sem Deployment acima (criado à mão, ou por um Operator
			// que não se declara controlador). Ele próprio é o workload.
			fresh[key] = workloadRef{Kind: "ReplicaSet", Name: item.GetName()}
		}
	}
	r.rsOwner = fresh
	return nil
}

// Resolve devolve o workload de cada Pod pedido, e a lista dos que não deu para
// resolver.
//
// Um Pod não resolvido é DEVOLVIDO como não resolvido, nunca chutado para
// "Pod#<nome>": chutar produziria uma série com nome de Pod que ninguém
// consulta, indistinguível de dado bom. Quem chama conta e registra.
func (r *ownerResolver) Resolve(ctx context.Context, keys []objectKey) (map[objectKey]workloadRef, []objectKey, error) {
	// Uma releitura por ciclo, no máximo, mesmo com vários Pods novos.
	needsPods := false
	for _, key := range keys {
		if _, ok := r.podOwner[key]; !ok {
			needsPods = true
			break
		}
	}
	if needsPods {
		if err := r.refreshPods(ctx); err != nil {
			return nil, nil, err
		}
	}

	resolved := make(map[objectKey]workloadRef, len(keys))
	var unresolved []objectKey
	var pendingRS []objectKey

	for _, key := range keys {
		owner, ok := r.podOwner[key]
		if !ok {
			// Pod que existia na leitura de métricas e não existe mais na de
			// Pods: morreu no meio do ciclo. O uso dele não é de ninguém.
			unresolved = append(unresolved, key)
			continue
		}
		if owner.Kind != "ReplicaSet" {
			// StatefulSet, DaemonSet, Job, um CRD de Operator, ou o próprio Pod.
			// Um hop só.
			//
			// Job deliberately does NOT climb to its CronJob: that would cost a
			// listing of jobs every cycle for a kind nothing queries.
			resolved[key] = owner
			continue
		}
		rsKey := objectKey{Namespace: key.Namespace, Name: owner.Name}
		if _, ok := r.rsOwner[rsKey]; !ok {
			pendingRS = append(pendingRS, key)
			continue
		}
		resolved[key] = r.rsOwner[rsKey]
	}

	if len(pendingRS) > 0 {
		if err := r.refreshReplicaSets(ctx); err != nil {
			return nil, nil, err
		}
		for _, key := range pendingRS {
			rsName := r.podOwner[key].Name
			rsKey := objectKey{Namespace: key.Namespace, Name: rsName}
			if owner, ok := r.rsOwner[rsKey]; ok {
				resolved[key] = owner
				continue
			}
			// ReplicaSet apagado entre uma listagem e outra. O ReplicaSet ainda
			// é uma resposta verdadeira -- melhor que descartar a amostra.
			resolved[key] = workloadRef{Kind: "ReplicaSet", Name: rsName}
		}
	}

	return resolved, unresolved, nil
}
