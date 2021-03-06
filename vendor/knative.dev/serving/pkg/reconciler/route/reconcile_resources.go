/*
Copyright 2018 The Knative Authors

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

package route

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/davecgh/go-spew/spew"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"knative.dev/networking/pkg/apis/networking"
	netv1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/reconciler/route/resources"
	"knative.dev/serving/pkg/reconciler/route/resources/names"
	"knative.dev/serving/pkg/reconciler/route/traffic"
)

func (c *Reconciler) reconcileIngress(
	ctx context.Context, r *v1.Route, tc *traffic.Config,
	tls []netv1alpha1.IngressTLS,
	ingressClass string,
	acmeChallenges ...netv1alpha1.HTTP01Challenge,
) (*netv1alpha1.Ingress, error) {
	recorder := controller.GetEventRecorder(ctx)

	desired, err := resources.MakeIngress(ctx, r, tc, tls, ingressClass, acmeChallenges...)
	if err != nil {
		return nil, err
	}
	// Get the current rollout state as described by the traffic.
	curRO := tc.BuildRollout()

	ingress, err := c.ingressLister.Ingresses(r.Namespace).Get(names.Ingress(r))
	if apierrs.IsNotFound(err) {
		// If there is no exisiting Ingress, then current rollout is _the_ rollout.
		desired.Annotations = kmeta.UnionMaps(desired.Annotations, map[string]string{
			networking.RolloutAnnotationKey: serializeRollout(ctx, curRO),
		})
		ingress, err = c.netclient.NetworkingV1alpha1().Ingresses(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			recorder.Eventf(r, corev1.EventTypeWarning, "CreationFailed", "Failed to create Ingress: %v", err)
			return nil, fmt.Errorf("failed to create Ingress: %w", err)
		}

		recorder.Eventf(r, corev1.EventTypeNormal, "Created", "Created Ingress %q", ingress.GetName())
		return ingress, nil
	} else if err != nil {
		return nil, err
	} else {
		// Ingress exists. We need to compute the rollout spec diff.
		prevRO := deserializeRollout(ctx, ingress.Annotations[networking.RolloutAnnotationKey])
		effectiveRO := curRO.Step(prevRO)
		// Update the annotation.
		desired.Annotations[networking.RolloutAnnotationKey] = serializeRollout(ctx, effectiveRO)
		// TODO(vagababov): apply the Rollout to the ingress spec here.
		if !equality.Semantic.DeepEqual(ingress.Spec, desired.Spec) ||
			!equality.Semantic.DeepEqual(ingress.Annotations, desired.Annotations) ||
			!equality.Semantic.DeepEqual(ingress.Labels, desired.Labels) {
			// It is notable that one reason for differences here may be defaulting.
			// When that is the case, the Update will end up being a nop because the
			// webhook will bring them into alignment and no new reconciliation will occur.
			// Also, compare annotation and label in case ingress.Class or parent route's labels
			// is updated.

			// Don't modify the informers copy
			origin := ingress.DeepCopy()
			origin.Spec = desired.Spec
			origin.Annotations = desired.Annotations
			origin.Labels = desired.Labels

			updated, err := c.netclient.NetworkingV1alpha1().Ingresses(origin.Namespace).Update(
				ctx, origin, metav1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to update Ingress: %w", err)
			}
			return updated, nil
		}
	}

	return ingress, err
}

func (c *Reconciler) deleteServices(ctx context.Context, namespace string, serviceNames sets.String) error {
	for _, serviceName := range serviceNames.List() {
		if err := c.kubeclient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete Service: %w", err)
		}
	}

	return nil
}

func (c *Reconciler) reconcilePlaceholderServices(ctx context.Context, route *v1.Route, targets map[string]traffic.RevisionTargets) ([]*corev1.Service, error) {
	logger := logging.FromContext(ctx)
	recorder := controller.GetEventRecorder(ctx)

	existingServices, err := c.getServices(route)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch existing services: %w", err)
	}
	existingServiceNames := resources.GetNames(existingServices)

	ns := route.Namespace
	names := make(sets.String, len(targets))
	for name := range targets {
		names.Insert(name)
	}

	services := make([]*corev1.Service, 0, names.Len())
	createdServiceNames := make(sets.String, names.Len())
	for _, name := range names.List() {
		desiredService, err := resources.MakeK8sPlaceholderService(ctx, route, name)
		if err != nil {
			return nil, fmt.Errorf("failed to construct placeholder k8s service: %w", err)
		}

		service, err := c.serviceLister.Services(ns).Get(desiredService.Name)
		if apierrs.IsNotFound(err) {
			// Doesn't exist, create it.
			service, err = c.kubeclient.CoreV1().Services(ns).Create(ctx, desiredService, metav1.CreateOptions{})
			if err != nil {
				recorder.Eventf(route, corev1.EventTypeWarning, "CreationFailed",
					"Failed to create placeholder service %q: %v", desiredService.Name, err)
				return nil, fmt.Errorf("failed to create placeholder service: %w", err)
			}
			logger.Info("Created service ", desiredService.Name)
			recorder.Eventf(route, corev1.EventTypeNormal, "Created", "Created placeholder service %q", desiredService.Name)
		} else if err != nil {
			return nil, err
		} else if !metav1.IsControlledBy(service, route) {
			// Surface an error in the route's status, and return an error.
			route.Status.MarkServiceNotOwned(desiredService.Name)
			return nil, fmt.Errorf("route: %q does not own Service: %q", route.Name, desiredService.Name)
		}

		services = append(services, service)
		createdServiceNames.Insert(desiredService.Name)
	}

	// Delete any current services that was no longer desired.
	if err := c.deleteServices(ctx, ns, existingServiceNames.Difference(createdServiceNames)); err != nil {
		return nil, err
	}

	// TODO(mattmoor): This is where we'd look at the state of the Service and
	// reflect any necessary state into the Route.
	return services, nil
}

func (c *Reconciler) updatePlaceholderServices(ctx context.Context, route *v1.Route, services []*corev1.Service, ingress *netv1alpha1.Ingress) error {
	logger := logging.FromContext(ctx)
	ns := route.Namespace

	eg, egCtx := errgroup.WithContext(ctx)
	for _, service := range services {
		service := service
		eg.Go(func() error {
			desiredService, err := resources.MakeK8sService(egCtx, route, service.Name, ingress, resources.IsClusterLocalService(service), service.Spec.ClusterIP)
			if err != nil {
				// Loadbalancer not ready, no need to update.
				logger.Warnw("Failed to update k8s service", zap.Error(err))
				return nil
			}

			// Make sure that the service has the proper specification.
			if !equality.Semantic.DeepEqual(service.Spec, desiredService.Spec) {
				// Don't modify the informers copy.
				existing := service.DeepCopy()
				existing.Spec = desiredService.Spec
				if _, err := c.kubeclient.CoreV1().Services(ns).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// TODO(mattmoor): This is where we'd look at the state of the Service and
	// reflect any necessary state into the Route.
	return eg.Wait()
}

func serializeRollout(ctx context.Context, r *traffic.Rollout) string {
	sr, err := json.Marshal(r)
	if err != nil {
		// This must never happen in the normal course of things.
		logging.FromContext(ctx).Warnw("Error serializing Rollout: "+spew.Sprint(r),
			zap.Error(err))
		return ""
	}
	return string(sr)
}

func deserializeRollout(ctx context.Context, ro string) *traffic.Rollout {
	if ro == "" {
		return nil
	}
	r := &traffic.Rollout{}
	// Failure can happen if users manually tweaked the
	// annotation or there's etcd corruption. Just log, rollouts
	// are not mission critical.
	if err := json.Unmarshal([]byte(ro), r); err != nil {
		logging.FromContext(ctx).Warnw("Error deserializing Rollout: "+ro,
			zap.Error(err))
		return nil
	}
	return r
}
