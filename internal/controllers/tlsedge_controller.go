/*
MIT License

Copyright (c) 2022 ngrok, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package controllers

import (
	"context"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	ingressv1alpha1 "github.com/ngrok/kubernetes-ingress-controller/api/v1alpha1"
	"github.com/ngrok/kubernetes-ingress-controller/internal/ngrokapi"
	"github.com/ngrok/ngrok-api-go/v5"
)

// TLSEdgeReconciler reconciles a TLSEdge object
type TLSEdgeReconciler struct {
	client.Client

	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	ipPolicyResolver

	NgrokClientset ngrokapi.Clientset

	controller *baseController[*ingressv1alpha1.TLSEdge]
}

// SetupWithManager sets up the controller with the Manager.
func (r *TLSEdgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.ipPolicyResolver = ipPolicyResolver{client: mgr.GetClient()}

	r.controller = &baseController[*ingressv1alpha1.TLSEdge]{
		Kube:     r.Client,
		Log:      r.Log,
		Recorder: r.Recorder,

		kubeType: "v1alpha1.TLSEdge",
		statusID: func(cr *ingressv1alpha1.TLSEdge) string { return cr.Status.ID },
		create:   r.create,
		update:   r.update,
		delete:   r.delete,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&ingressv1alpha1.TLSEdge{}).
		Watches(
			&source.Kind{Type: &ingressv1alpha1.IPPolicy{}},
			handler.EnqueueRequestsFromMapFunc(r.listTLSEdgesForIPPolicy),
		).
		Complete(r)
}

//+kubebuilder:rbac:groups=ingress.k8s.ngrok.com,resources=tlsedges,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ingress.k8s.ngrok.com,resources=tlsedges/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ingress.k8s.ngrok.com,resources=tlsedges/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.1/pkg/reconcile
func (r *TLSEdgeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.controller.reconcile(ctx, req, new(ingressv1alpha1.TLSEdge))
}

func (r *TLSEdgeReconciler) create(ctx context.Context, edge *ingressv1alpha1.TLSEdge) error {
	if err := r.reconcileTunnelGroupBackend(ctx, edge); err != nil {
		return err
	}

	// Try to find the edge by the backend labels
	resp, err := r.findEdgeByBackendLabels(ctx, edge.Spec.Backend.Labels)
	if err != nil {
		return err
	}

	if resp != nil {
		return r.updateEdgeStatus(ctx, edge, resp)
	}

	// No edge has been created for this edge, create one
	r.Log.Info("Creating new TLSEdge", "namespace", edge.Namespace, "name", edge.Name)
	resp, err = r.NgrokClientset.TLSEdges().Create(ctx, &ngrok.TLSEdgeCreate{
		Hostports:   edge.Spec.Hostports,
		Description: edge.Spec.Description,
		Metadata:    edge.Spec.Metadata,
		Backend: &ngrok.EndpointBackendMutate{
			BackendID: edge.Status.Backend.ID,
		},
	})
	if err != nil {
		return err
	}
	r.Log.Info("Created new TLSEdge", "edge.ID", resp.ID, "name", edge.Name, "namespace", edge.Namespace)

	if err := r.updateEdgeStatus(ctx, edge, resp); err != nil {
		return err
	}

	return r.updateIPRestrictionRouteModule(ctx, edge, resp)
}

func (r *TLSEdgeReconciler) update(ctx context.Context, edge *ingressv1alpha1.TLSEdge) error {
	if err := r.reconcileTunnelGroupBackend(ctx, edge); err != nil {
		return err
	}

	resp, err := r.NgrokClientset.TLSEdges().Get(ctx, edge.Status.ID)
	if err != nil {
		// If we can't find the edge in the ngrok API, it's been deleted, so clear the ID
		// and requeue the edge. When it gets reconciled again, it will be recreated.
		if ngrok.IsNotFound(err) {
			r.Log.Info("TLSEdge not found, clearing ID and requeuing", "edge.ID", edge.Status.ID)
			edge.Status.ID = ""
			//nolint:errcheck
			r.Status().Update(ctx, edge)
		}
		return err
	}

	// If the backend or hostports do not match, update the edge with the desired backend and hostports
	if resp.Backend.Backend.ID != edge.Status.Backend.ID ||
		!slices.Equal(resp.Hostports, edge.Status.Hostports) {
		resp, err = r.NgrokClientset.TLSEdges().Update(ctx, &ngrok.TLSEdgeUpdate{
			ID:          resp.ID,
			Description: pointer.String(edge.Spec.Description),
			Metadata:    pointer.String(edge.Spec.Metadata),
			Hostports:   edge.Spec.Hostports,
			Backend: &ngrok.EndpointBackendMutate{
				BackendID: edge.Status.Backend.ID,
			},
		})
		if err != nil {
			return err
		}
	}

	return r.updateEdgeStatus(ctx, edge, resp)
}

func (r *TLSEdgeReconciler) delete(ctx context.Context, edge *ingressv1alpha1.TLSEdge) error {
	err := r.NgrokClientset.TLSEdges().Delete(ctx, edge.Status.ID)
	if err == nil || ngrok.IsNotFound(err) {
		edge.Status.ID = ""
	}
	return err
}

func (r *TLSEdgeReconciler) reconcileTunnelGroupBackend(ctx context.Context, edge *ingressv1alpha1.TLSEdge) error {
	specBackend := edge.Spec.Backend
	// First make sure the tunnel group backend matches
	if edge.Status.Backend.ID != "" {
		// A backend has already been created for this edge, make sure the labels match
		backend, err := r.NgrokClientset.TunnelGroupBackends().Get(ctx, edge.Status.Backend.ID)
		if err != nil {
			if ngrok.IsNotFound(err) {
				r.Log.Info("TunnelGroupBackend not found, clearing ID and requeuing", "TunnelGroupBackend.ID", edge.Status.Backend.ID)
				edge.Status.Backend.ID = ""
				//nolint:errcheck
				r.Status().Update(ctx, edge)
			}
			return err
		}

		// If the labels don't match, update the backend with the desired labels
		if !maps.Equal(backend.Labels, specBackend.Labels) {
			_, err = r.NgrokClientset.TunnelGroupBackends().Update(ctx, &ngrok.TunnelGroupBackendUpdate{
				ID:          backend.ID,
				Metadata:    pointer.String(specBackend.Metadata),
				Description: pointer.String(specBackend.Description),
				Labels:      specBackend.Labels,
			})
			if err != nil {
				return err
			}
		}
		return nil
	}

	// No backend has been created for this edge, create one
	backend, err := r.NgrokClientset.TunnelGroupBackends().Create(ctx, &ngrok.TunnelGroupBackendCreate{
		Metadata:    edge.Spec.Backend.Metadata,
		Description: edge.Spec.Backend.Description,
		Labels:      edge.Spec.Backend.Labels,
	})
	if err != nil {
		return err
	}
	edge.Status.Backend.ID = backend.ID

	return r.Status().Update(ctx, edge)
}

func (r *TLSEdgeReconciler) findEdgeByBackendLabels(ctx context.Context, backendLabels map[string]string) (*ngrok.TLSEdge, error) {
	r.Log.Info("Searching for existing TLSEdge with backend labels", "labels", backendLabels)
	iter := r.NgrokClientset.TLSEdges().List(&ngrok.Paging{})
	for iter.Next(ctx) {
		edge := iter.Item()
		if edge.Backend == nil {
			continue
		}

		backend, err := r.NgrokClientset.TunnelGroupBackends().Get(ctx, edge.Backend.Backend.ID)
		if err != nil {
			// If we get an error looking up the backend, return the error and
			// hopefully the next reconcile will fix it.
			return nil, err
		}
		if backend == nil {
			continue
		}

		if maps.Equal(backend.Labels, backendLabels) {
			r.Log.Info("Found existing TLSEdge with matching backend labels", "labels", backendLabels, "edge.ID", edge.ID)
			return edge, nil
		}
	}
	return nil, iter.Err()
}

func (r *TLSEdgeReconciler) updateEdgeStatus(ctx context.Context, edge *ingressv1alpha1.TLSEdge, remoteEdge *ngrok.TLSEdge) error {
	edge.Status.ID = remoteEdge.ID
	edge.Status.URI = remoteEdge.URI
	edge.Status.Hostports = remoteEdge.Hostports
	edge.Status.Backend.ID = remoteEdge.Backend.Backend.ID

	return r.Status().Update(ctx, edge)
}

func (r *TLSEdgeReconciler) updateIPRestrictionRouteModule(ctx context.Context, edge *ingressv1alpha1.TLSEdge, remoteEdge *ngrok.TLSEdge) error {
	if edge.Spec.IPRestriction == nil || len(edge.Spec.IPRestriction.IPPolicies) == 0 {
		return r.NgrokClientset.EdgeModules().TLS().IPRestriction().Delete(ctx, edge.Status.ID)
	}
	policyIds, err := r.ipPolicyResolver.resolveIPPolicyNamesorIds(ctx, edge.Namespace, edge.Spec.IPRestriction.IPPolicies)
	if err != nil {
		return err
	}
	r.Log.Info("Resolved IP Policy NamesOrIDs to IDs", "policyIds", policyIds)

	_, err = r.NgrokClientset.EdgeModules().TLS().IPRestriction().Replace(ctx, &ngrok.EdgeIPRestrictionReplace{
		ID: edge.Status.ID,
		Module: ngrok.EndpointIPPolicyMutate{
			IPPolicyIDs: policyIds,
		},
	})
	return err
}

func (r *TLSEdgeReconciler) listTLSEdgesForIPPolicy(obj client.Object) []reconcile.Request {
	r.Log.Info("Listing TLSEdges for ip policy to determine if they need to be reconciled")
	policy, ok := obj.(*ingressv1alpha1.IPPolicy)
	if !ok {
		r.Log.Error(nil, "failed to convert object to IPPolicy", "object", obj)
		return []reconcile.Request{}
	}

	edges := &ingressv1alpha1.TLSEdgeList{}
	if err := r.Client.List(context.Background(), edges); err != nil {
		r.Log.Error(err, "failed to list TLSEdges for ippolicy", "name", policy.Name, "namespace", policy.Namespace)
		return []reconcile.Request{}
	}

	recs := []reconcile.Request{}

	for _, edge := range edges.Items {
		if edge.Spec.IPRestriction == nil {
			continue
		}
		for _, edgePolicyID := range edge.Spec.IPRestriction.IPPolicies {
			if edgePolicyID == policy.Name || edgePolicyID == policy.Status.ID {
				recs = append(recs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      edge.GetName(),
						Namespace: edge.GetNamespace(),
					},
				})
				break
			}
		}
	}

	r.Log.Info("IPPolicy change triggered TLSEdge reconciliation", "count", len(recs), "policy", policy.Name, "namespace", policy.Namespace)
	return recs
}
