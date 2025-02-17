/*
Copyright 2024 Angelo Conforti.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	haegressv2 "github.com/angeloxx/cilium-haegress-operator/api/v2"
	haegressip "github.com/angeloxx/cilium-haegress-operator/pkg"
	haegressiputil "github.com/angeloxx/cilium-haegress-operator/util"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sync/atomic"
	"time"
)

// HAEgressGatewayPolicyReconciler reconciles a HAEgressGatewayPolicy object
type HAEgressGatewayPolicyReconciler struct {
	client.Client
	Log                      logr.Logger
	Scheme                   *runtime.Scheme
	Recorder                 record.EventRecorder
	EgressNamespace          string
	LoadBalancerClass        string
	BackgroundCheckerSeconds int
	lastServiceUpdate        atomic.Value
}

//+kubebuilder:rbac:groups=cilium.angeloxx.ch,resources=haegressgatewaypolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cilium.angeloxx.ch,resources=haegressgatewaypolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cilium.angeloxx.ch,resources=haegressgatewaypolicies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HAEgressGatewayPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *HAEgressGatewayPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var haEgressGatewayPolicy haegressv2.HAEgressGatewayPolicy

	// Check if the resource is available, eg. if Reconcile was called due a delete
	if err := r.Get(ctx, req.NamespacedName, &haEgressGatewayPolicy); err != nil {
		if apierrors.IsNotFound(err) {
			// we'll ignore not-found errors, since they can't be fixed by an immediate
			// requeue (we'll need to wait for a new notification), and we can get them
			// on deleted requests.
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch HAEgressGatewayPolicy", "HAEgressGatewayPolicy", req.NamespacedName)
		return ctrl.Result{}, err
	}

	if err := r.UpdateOrCreateCiliumEgressGatewayPolicy(ctx, &haEgressGatewayPolicy); err != nil {
		log.Error(err, "unable to create or update CiliumEgressGatewayPolicy, please check RBAC permissions")
		return ctrl.Result{RequeueAfter: haegressip.HAEgressGatewayPolicyChcekRequeueAfter}, err
	}

	// Check if a service generated by this controller already exists, if not create the service
	if err := r.UpdateOrCreateService(ctx, &haEgressGatewayPolicy); err != nil {
		log.Error(err, "unable to create or update Service, please check RBAC permissions")
		return ctrl.Result{RequeueAfter: haegressip.HAEgressGatewayPolicyChcekRequeueAfter}, err
	}

	return ctrl.Result{}, nil
}

func (r *HAEgressGatewayPolicyReconciler) UpdateOrCreateCiliumEgressGatewayPolicy(ctx context.Context, haEgressGatewayPolicy *haegressv2.HAEgressGatewayPolicy) error {
	log := ctrl.LoggerFrom(ctx)

	// Save the last update date in order to delay the next background check
	r.lastServiceUpdate.Store(time.Now())

	logger := log.WithValues("HAEgressGatewayPolicy", haEgressGatewayPolicy.Name)

	serviceNamespace := r.EgressNamespace
	if haEgressGatewayPolicy.Annotations[haegressip.HAEgressGatewayPolicyNamespace] != "" {
		serviceNamespace = haEgressGatewayPolicy.Annotations[haegressip.HAEgressGatewayPolicyNamespace]
	}

	ciliumEgressGatewayPolicyNew := &ciliumv2.CiliumEgressGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s",
				serviceNamespace,
				haEgressGatewayPolicy.Name),
			Labels:      haEgressGatewayPolicy.Labels,
			Annotations: haEgressGatewayPolicy.Annotations,
		},
		Spec: haEgressGatewayPolicy.Spec,
	}

	// Set HAEgressGatewayPolicy instance as the owner and controller
	if err := controllerutil.SetControllerReference(haEgressGatewayPolicy, ciliumEgressGatewayPolicyNew, r.Scheme); err != nil {
		return err
	}

	ciliumEgressGatewayPolicyExist := &ciliumv2.CiliumEgressGatewayPolicy{}
	err := r.Get(ctx, types.NamespacedName{
		Name: ciliumEgressGatewayPolicyNew.Name,
	}, ciliumEgressGatewayPolicyExist)

	if err != nil && apierrors.IsNotFound(err) {
		logger.Info("Creating a new CiliumEgressGatewayPolicy for HAEgressGatewayPolicy",
			"CiliumEgressGatewayPolicy", ciliumEgressGatewayPolicyNew.Name)
		err = r.Create(ctx, ciliumEgressGatewayPolicyNew)
		r.Recorder.Event(haEgressGatewayPolicy,
			corev1.EventTypeNormal,
			"Created",
			fmt.Sprintf("CiliumEgressGatewayPolicy %q created", ciliumEgressGatewayPolicyNew.Name))
		if err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(haEgressGatewayPolicy, ciliumEgressGatewayPolicyNew, r.Scheme); err != nil {
			return err
		}

		// If service already exists, reconcile
		service := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: haEgressGatewayPolicy.Name, Namespace: serviceNamespace}, service)
		if err == nil {
			// Call the services reconcile function
			_, syncError := haegressiputil.SyncServiceWithCiliumEgressGatewayPolicy(ctx, r.Client, logger, r.Recorder, *service, *ciliumEgressGatewayPolicyNew)
			if syncError != nil {
				return syncError
			}
		}

	} else if err != nil {
		return err
	} else {
		// Update CiliumEgressGatewayPolicy if this policy is manged by the HA
		if !metav1.IsControlledBy(ciliumEgressGatewayPolicyExist, haEgressGatewayPolicy) {
			logger.Error(nil, "CiliumEgressGatewayPolicy already exists and is not controlled by HAEgressGatewayPolicy",
				"CiliumEgressGatewayPolicy", ciliumEgressGatewayPolicyExist.Name)
			r.Recorder.Event(haEgressGatewayPolicy,
				corev1.EventTypeWarning,
				"AlreadyExists",
				fmt.Sprintf("Resource %q already exists and is not managed by HAEgressGatewayPolicy", ciliumEgressGatewayPolicyExist.Name))
			return nil
		} else {
			if !reflect.DeepEqual(ciliumEgressGatewayPolicyExist.Spec.Selectors, ciliumEgressGatewayPolicyNew.Spec.Selectors) {
				ciliumEgressGatewayPolicyExist.Spec.Selectors = ciliumEgressGatewayPolicyNew.Spec.Selectors
				err = r.Update(ctx, ciliumEgressGatewayPolicyExist)
				if err != nil {
					return err
				}
				logger.Info("CiliumEgressGatewayPolicy updated",
					"CiliumEgressGatewayPolicy", ciliumEgressGatewayPolicyExist.Name)
				r.Recorder.Event(haEgressGatewayPolicy, corev1.EventTypeNormal, "Updated",
					fmt.Sprintf("CiliumEgressGatewayPolicy %q updated", ciliumEgressGatewayPolicyExist.Name))
			}
		}
	}
	return nil
}

func (r *HAEgressGatewayPolicyReconciler) UpdateOrCreateService(ctx context.Context, haEgressGatewayPolicy *haegressv2.HAEgressGatewayPolicy) error {
	log := ctrl.LoggerFrom(ctx)

	// Save the last update date in order to delay the next background check
	r.lastServiceUpdate.Store(time.Now())

	serviceNamespace := r.EgressNamespace
	if haEgressGatewayPolicy.Annotations[haegressip.HAEgressGatewayPolicyNamespace] != "" {
		serviceNamespace = haEgressGatewayPolicy.Annotations[haegressip.HAEgressGatewayPolicyNamespace]
	}

	// @TODO: check if target namespace exists

	// Define the service and copy all annotations from the HAEgressGatewayPolicy instance
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        haEgressGatewayPolicy.Name,
			Namespace:   serviceNamespace,
			Labels:      haEgressGatewayPolicy.Labels,
			Annotations: haEgressGatewayPolicy.Annotations,
		},
		Spec: corev1.ServiceSpec{
			LoadBalancerClass: &r.LoadBalancerClass,
			Ports: []corev1.ServicePort{
				{
					Name:     "nope",
					Protocol: corev1.ProtocolTCP,
					Port:     65534,
				},
			},
			Type: corev1.ServiceTypeLoadBalancer,
			// Points nowhere, is a serviceless service used to create the IP object
			Selector: map[string]string{
				haegressip.HAEgressGatewayPolicyNamespace: serviceNamespace,
				haegressip.HAEgressGatewayPolicyName:      haEgressGatewayPolicy.Name,
			},
		},
	}

	if service.Labels == nil {
		service.Labels = make(map[string]string)
	}
	if service.Annotations == nil {
		service.Annotations = make(map[string]string)
	}
	// Avoid L2 announcement by Cilium
	service.Labels[haegressip.KubernetesServiceProxyNameAnnotation] = "kubevip-managed-by-cilium-haegess"
	service.Labels[haegressip.HAEgressGatewayPolicyNamespace] = serviceNamespace
	service.Labels[haegressip.HAEgressGatewayPolicyName] = haEgressGatewayPolicy.Name

	// Set HAEgressGatewayPolicy instance as the owner and controller
	if err := controllerutil.SetControllerReference(haEgressGatewayPolicy, service, r.Scheme); err != nil {
		return err
	}

	// Check if the service already exists, create if not exist, while if exist it will update the service
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("Creating a new Service for HAEgressGatewayPolicy", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		err = r.Create(ctx, service)
		r.Recorder.Event(haEgressGatewayPolicy,
			corev1.EventTypeNormal,
			"Created",
			fmt.Sprintf("Service %s/%s created", service.Namespace, service.Name))
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		// Update service if needed
		if !metav1.IsControlledBy(found, haEgressGatewayPolicy) {
			log.Error(nil, "Service already exists and is not controlled by HAEgressGatewayPolicy",
				"Service.Namespace", found.Namespace, "Service.Name", found.Name)
			// Generate an event to record this issue in haEgressGatewayPolicy
			r.Recorder.Event(haEgressGatewayPolicy, corev1.EventTypeWarning, "AlreadyExists", fmt.Sprintf("Resource %q already exists and is not managed by HAEgressGatewayPolicy", found.Name))

			return nil
		} else {
			if !reflect.DeepEqual(found.Spec.Selector, service.Spec.Selector) {
				log.Info("Updating Service already controlled by HAEgressGatewayPolicy", "Service.Namespace", found.Namespace, "Service.Name", found.Name)
				err = r.Update(ctx, service)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (r *HAEgressGatewayPolicyReconciler) findObjectsForHaegressGatewayPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	ownerRefs := obj.GetOwnerReferences()
	requests := []reconcile.Request{}

	for _, ownerRef := range ownerRefs {
		if ownerRef.Kind == "HAEgressGatewayPolicy" {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      ownerRef.Name,
					Namespace: obj.GetNamespace(),
				},
			})
		}
	}

	return requests
}

func (r *HAEgressGatewayPolicyReconciler) backgroundPeriodicalCheck(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx)
	ticker := time.NewTicker(time.Duration(r.BackgroundCheckerSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Manage concurrency, avoid update if the latest change happened recently, less than
			// half of the background checker period
			if lastUpdate, ok := r.lastServiceUpdate.Load().(time.Time); ok {
				if time.Since(lastUpdate) < (time.Duration(r.BackgroundCheckerSeconds/2) * time.Second) {
					log.Info("Last object update too recent, skipping periodic check",
						"lastUpdate", lastUpdate)
					continue
				}
			} else {
				log.V(1).Info("No previous update recorded, initializing timestamp")
				r.lastServiceUpdate.Store(time.Now())
				continue
			}

			var policies haegressv2.HAEgressGatewayPolicyList
			if err := r.List(ctx, &policies); err != nil {
				log.Error(err, "failed to list HAEgressGatewayPolicies")
				continue
			}

			for _, policy := range policies.Items {
				log.Info("Periodic check of HAEgressGatewayPolicy",
					"Name", policy.Name,
					"Namespace", policy.Namespace)

				if err := r.UpdateOrCreateCiliumEgressGatewayPolicy(ctx, &policy); err != nil {
					log.Error(err, "failed to update CiliumEgressGatewayPolicy")
				}

				if err := r.UpdateOrCreateService(ctx, &policy); err != nil {
					log.Error(err, "failed to update Service")
				}
			}
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *HAEgressGatewayPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.BackgroundCheckerSeconds > 0 {
		ctx := context.Background()
		go func() {
			<-mgr.Elected()
			r.backgroundPeriodicalCheck(ctx)
		}()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&haegressv2.HAEgressGatewayPolicy{}).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForHaegressGatewayPolicy),
			builder.WithPredicates(predicate.Funcs{
				DeleteFunc: func(e event.DeleteEvent) bool {
					return true
				},
				CreateFunc: func(e event.CreateEvent) bool {
					return false
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return false
				},
				GenericFunc: func(e event.GenericEvent) bool {
					return false
				},
			}),
		).
		Watches(
			&ciliumv2.CiliumEgressGatewayPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForHaegressGatewayPolicy),
			builder.WithPredicates(predicate.Funcs{
				DeleteFunc: func(e event.DeleteEvent) bool {
					return true
				},
				CreateFunc: func(e event.CreateEvent) bool {
					return false
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return false
				},
				GenericFunc: func(e event.GenericEvent) bool {
					return false
				},
			}),
		).
		Complete(r)
}
