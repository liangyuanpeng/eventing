/*
Copyright 2020 The Knative Authors

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

package pingsource

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"

	appsv1listers "k8s.io/client-go/listers/apps/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/controller"
	pkgLogging "knative.dev/pkg/logging"
	"knative.dev/pkg/metrics"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
	"knative.dev/pkg/system"
	"knative.dev/pkg/tracker"

	"knative.dev/eventing/pkg/apis/eventing"
	"knative.dev/eventing/pkg/apis/sources/v1alpha2"
	pingsourcereconciler "knative.dev/eventing/pkg/client/injection/reconciler/sources/v1alpha2/pingsource"
	listers "knative.dev/eventing/pkg/client/listers/sources/v1alpha2"
	"knative.dev/eventing/pkg/logging"
	"knative.dev/eventing/pkg/reconciler/pingsource/resources"
	recresources "knative.dev/eventing/pkg/reconciler/resources"
	"knative.dev/eventing/pkg/utils"
)

const (
	// Name of the corev1.Events emitted from the reconciliation process
	pingSourceDeploymentCreated     = "PingSourceDeploymentCreated"
	pingSourceDeploymentUpdated     = "PingSourceDeploymentUpdated"
	pingSourceDeploymentDeleted     = "PingSourceDeploymentDeleted"
	pingSourceServiceAccountCreated = "PingSourceServiceAccountCreated"
	pingSourceRoleBindingCreated    = "PingSourceRoleBindingCreated"

	component                = "pingsource"
	mtcomponent              = "pingsource-mt-adapter"
	mtadapterName            = "pingsource-mt-adapter"
	stadapterClusterRoleName = "knative-eventing-pingsource-adapter"
)

func newWarningSinkNotFound(sink *duckv1.Destination) pkgreconciler.Event {
	b, _ := json.Marshal(sink)
	return pkgreconciler.NewEvent(corev1.EventTypeWarning, "SinkNotFound", "Sink not found: %s", string(b))
}

func newServiceAccountWarn(err error) pkgreconciler.Event {
	return pkgreconciler.NewEvent(corev1.EventTypeWarning, "PingSourceServiceAccountFailed", "Reconciling PingSource ServiceAccount failed: %s", err)
}

func newRoleBindingWarn(err error) pkgreconciler.Event {
	return pkgreconciler.NewEvent(corev1.EventTypeWarning, "PingSourceRoleBindingFailed", "Reconciling PingSource RoleBinding failed: %s", err)
}

type Reconciler struct {
	kubeClientSet kubernetes.Interface

	receiveAdapterImage   string
	receiveMTAdapterImage string

	// listers index properties about resources
	pingLister           listers.PingSourceLister
	deploymentLister     appsv1listers.DeploymentLister
	serviceAccountLister corev1listers.ServiceAccountLister
	roleBindingLister    rbacv1listers.RoleBindingLister

	// tracking mt adapter deployment changes
	tracker tracker.Interface

	loggingContext context.Context
	sinkResolver   *resolver.URIResolver
	loggingConfig  *pkgLogging.Config
	metricsConfig  *metrics.ExporterOptions

	// Leader election configuration for the mt receive adapter
	leConfig string
}

// Check that our Reconciler implements ReconcileKind
var _ pingsourcereconciler.Interface = (*Reconciler)(nil)

func (r *Reconciler) ReconcileKind(ctx context.Context, source *v1alpha2.PingSource) pkgreconciler.Event {
	// This Source attempts to reconcile three things.
	// 1. Determine the sink's URI.
	//     - Nothing to delete.
	// 2. Create a receive adapter in the form of a Deployment.
	//     - Will be garbage collected by K8s when this PingSource is deleted.
	// 3. Create the EventType that it can emit.
	//     - Will be garbage collected by K8s when this PingSource is deleted.

	dest := source.Spec.Sink.DeepCopy()
	if dest.Ref != nil {
		// To call URIFromDestination(), dest.Ref must have a Namespace. If there is
		// no Namespace defined in dest.Ref, we will use the Namespace of the source
		// as the Namespace of dest.Ref.
		if dest.Ref.Namespace == "" {
			//TODO how does this work with deprecated fields
			dest.Ref.Namespace = source.GetNamespace()
		}
	}

	sinkURI, err := r.sinkResolver.URIFromDestinationV1(*dest, source)
	if err != nil {
		source.Status.MarkNoSink("NotFound", "")
		return newWarningSinkNotFound(dest)
	}
	source.Status.MarkSink(sinkURI)

	// The webhook does not allow for invalid schedules to be posted.
	// TODO: remove MarkSchedule
	source.Status.MarkSchedule()

	scope, ok := source.Annotations[eventing.ScopeAnnotationKey]
	if !ok {
		scope = eventing.ScopeCluster
	}

	if scope == eventing.ScopeCluster {
		// Make sure the global mt receive adapter is running
		d, err := r.reconcileMTReceiveAdapter(ctx, source)
		if err != nil {
			logging.FromContext(ctx).Error("Unable to reconcile the mt receive adapter", zap.Error(err))
			return err
		}
		source.Status.PropagateDeploymentAvailability(d)

		// Tell tracker to reconcile this PingSource whenever the deployment changes
		err = r.tracker.TrackReference(tracker.Reference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Namespace:  d.Namespace,
			Name:       d.Name,
		}, source)

		if err != nil {
			logging.FromContext(ctx).Error("Unable to track the deployment", zap.Error(err))
			return err
		}

	} else {
		if _, err := r.reconcileServiceAccount(ctx, source); err != nil {
			logging.FromContext(ctx).Error("Unable to create the receive adapter service account", zap.Error(err))
			return fmt.Errorf("creating receive adapter service account: %v", err)
		}

		if _, err := r.reconcileRoleBinding(ctx, source); err != nil {
			logging.FromContext(ctx).Error("Unable to create the receive adapter role binding", zap.Error(err))
			return fmt.Errorf("creating receive adapter role binding: %v", err)
		}

		ra, err := r.createReceiveAdapter(ctx, source, sinkURI)
		if err != nil {
			logging.FromContext(ctx).Error("Unable to create the receive adapter", zap.Error(err))
			return fmt.Errorf("creating receive adapter: %v", err)
		}
		source.Status.PropagateDeploymentAvailability(ra)
	}

	source.Status.CloudEventAttributes = []duckv1.CloudEventAttributes{{
		Type:   v1alpha2.PingSourceEventType,
		Source: v1alpha2.PingSourceSource(source.Namespace, source.Name),
	}}

	return nil
}

func (r *Reconciler) reconcileServiceAccount(ctx context.Context, source *v1alpha2.PingSource) (*corev1.ServiceAccount, error) {
	saName := resources.CreateReceiveAdapterName(source.Name, source.UID)
	sa, err := r.serviceAccountLister.ServiceAccounts(source.Namespace).Get(saName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			expected := recresources.MakeServiceAccount(source, saName)
			sa, err := r.kubeClientSet.CoreV1().ServiceAccounts(source.Namespace).Create(expected)
			if err != nil {
				return sa, newServiceAccountWarn(err)
			}
			controller.GetEventRecorder(ctx).Eventf(source, corev1.EventTypeNormal, pingSourceServiceAccountCreated, "PingSource ServiceAccount created")
			return sa, nil
		}

		logging.FromContext(ctx).Error("Unable to get the PingSource ServiceAccount", zap.Error(err))
		source.Status.Annotations["serviceAccount"] = "Failed to get ServiceAccount"
		return nil, newServiceAccountWarn(err)
	}
	return sa, nil
}

func (r *Reconciler) reconcileRoleBinding(ctx context.Context, source *v1alpha2.PingSource) (*rbacv1.RoleBinding, error) {
	rbName := resources.CreateReceiveAdapterName(source.Name, source.UID)

	rb, err := r.roleBindingLister.RoleBindings(source.Namespace).Get(rbName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			expected := resources.MakeRoleBinding(source, rbName, stadapterClusterRoleName)
			rb, err := r.kubeClientSet.RbacV1().RoleBindings(source.Namespace).Create(expected)
			if err != nil {
				return rb, newRoleBindingWarn(err)
			}
			controller.GetEventRecorder(ctx).Eventf(source, corev1.EventTypeNormal, pingSourceRoleBindingCreated, "PingSource RoleBinding created")
			return rb, nil
		}
		logging.FromContext(ctx).Error("Unable to get the PingSource RoleBinding", zap.Error(err))
		source.Status.Annotations["roleBinding"] = "Failed to get PingSource RoleBinding"
		return nil, newRoleBindingWarn(err)
	}
	return rb, nil
}

func (r *Reconciler) createReceiveAdapter(ctx context.Context, src *v1alpha2.PingSource, sinkURI *apis.URL) (*appsv1.Deployment, error) {
	loggingConfig, err := pkgLogging.LoggingConfigToJson(r.loggingConfig)
	if err != nil {
		logging.FromContext(ctx).Error("error while converting logging config to JSON", zap.Any("receiveAdapter", err))
	}

	metricsConfig, err := metrics.MetricsOptionsToJson(r.metricsConfig)
	if err != nil {
		logging.FromContext(ctx).Error("error while converting metrics config to JSON", zap.Any("receiveAdapter", err))
	}

	adapterArgs := resources.Args{
		Image:         r.receiveAdapterImage,
		Source:        src,
		Labels:        resources.Labels(src.Name),
		SinkURI:       sinkURI,
		LoggingConfig: loggingConfig,
		MetricsConfig: metricsConfig,
	}
	expected := resources.MakeReceiveAdapter(&adapterArgs)

	ra, err := r.deploymentLister.Deployments(src.Namespace).Get(expected.Name)
	if apierrors.IsNotFound(err) {
		// Issue #2842: Adapter deployment name uses kmeta.ChildName. If a deployment by the previous name pattern is found, it should
		// be deleted. This might cause temporary downtime.
		if deprecatedName := utils.GenerateFixedName(adapterArgs.Source, fmt.Sprintf("pingsource-%s", adapterArgs.Source.Name)); deprecatedName != expected.Name {
			if err := r.kubeClientSet.AppsV1().Deployments(src.Namespace).Delete(deprecatedName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("error deleting deprecated named deployment: %v", err)
			}
			controller.GetEventRecorder(ctx).Eventf(src, corev1.EventTypeNormal, pingSourceDeploymentDeleted, "Deprecated deployment removed: \"%s/%s\"", src.Namespace, deprecatedName)
		}

		ra, err = r.kubeClientSet.AppsV1().Deployments(src.Namespace).Create(expected)
		msg := "Deployment created"
		if err != nil {
			msg = fmt.Sprintf("Deployment created, error: %v", err)
		}
		controller.GetEventRecorder(ctx).Eventf(src, corev1.EventTypeNormal, pingSourceDeploymentCreated, "%s", msg)
		return ra, err
	} else if err != nil {
		return nil, fmt.Errorf("error getting receive adapter: %v", err)
	} else if !metav1.IsControlledBy(ra, src) {
		return nil, fmt.Errorf("deployment %q is not owned by PingSource %q", ra.Name, src.Name)
	} else if podSpecChanged(ra.Spec.Template.Spec, expected.Spec.Template.Spec) {
		ra.Spec.Template.Spec = expected.Spec.Template.Spec
		if ra, err = r.kubeClientSet.AppsV1().Deployments(src.Namespace).Update(ra); err != nil {
			return ra, err
		}
		controller.GetEventRecorder(ctx).Eventf(src, corev1.EventTypeNormal, pingSourceDeploymentUpdated, "Deployment %q updated", ra.Name)
		return ra, nil
	} else {
		logging.FromContext(ctx).Debug("Reusing existing receive adapter", zap.Any("receiveAdapter", ra))
	}
	return ra, nil
}

func (r *Reconciler) reconcileMTReceiveAdapter(ctx context.Context, source *v1alpha2.PingSource) (*appsv1.Deployment, error) {
	loggingConfig, err := pkgLogging.LoggingConfigToJson(r.loggingConfig)
	if err != nil {
		logging.FromContext(ctx).Error("error while converting logging config to JSON", zap.Any("receiveAdapter", err))
	}

	metricsConfig, err := metrics.MetricsOptionsToJson(r.metricsConfig)
	if err != nil {
		logging.FromContext(ctx).Error("error while converting metrics config to JSON", zap.Any("receiveAdapter", err))
	}

	args := resources.MTArgs{
		ServiceAccountName: mtadapterName,
		MTAdapterName:      mtadapterName,
		Image:              r.receiveMTAdapterImage,
		LoggingConfig:      loggingConfig,
		MetricsConfig:      metricsConfig,
		LeConfig:           r.leConfig,
	}
	expected := resources.MakeMTReceiveAdapter(args)

	d, err := r.deploymentLister.Deployments(system.Namespace()).Get(mtadapterName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			d, err := r.kubeClientSet.AppsV1().Deployments(system.Namespace()).Create(expected)
			if err != nil {
				controller.GetEventRecorder(ctx).Eventf(source, corev1.EventTypeWarning, pingSourceDeploymentCreated, "Cluster-scoped deployment not created (%v)", err)
				return nil, err
			}
			controller.GetEventRecorder(ctx).Event(source, corev1.EventTypeNormal, pingSourceDeploymentCreated, "Cluster-scoped deployment created")
			return d, nil
		}
		return nil, fmt.Errorf("error getting mt adapter deployment %v", err)
	} else if podSpecChanged(d.Spec.Template.Spec, expected.Spec.Template.Spec) {
		d.Spec.Template.Spec = expected.Spec.Template.Spec
		if d, err = r.kubeClientSet.AppsV1().Deployments(system.Namespace()).Update(d); err != nil {
			return d, err
		}
		controller.GetEventRecorder(ctx).Event(source, corev1.EventTypeNormal, pingSourceDeploymentUpdated, "Cluster-scoped deployment updated")
		return d, nil
	} else {
		logging.FromContext(ctx).Debug("Reusing existing cluster-scoped deployment", zap.Any("deployment", d))
	}
	return d, nil
}

func podSpecChanged(oldPodSpec corev1.PodSpec, newPodSpec corev1.PodSpec) bool {
	// We really care about the fields we set and ignore the test.
	return !equality.Semantic.DeepDerivative(newPodSpec, oldPodSpec)
}

// TODO determine how to push the updated logging config to existing data plane Pods.
func (r *Reconciler) UpdateFromLoggingConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	logcfg, err := pkgLogging.NewConfigFromConfigMap(cfg)
	if err != nil {
		logging.FromContext(r.loggingContext).Warn("failed to create logging config from configmap", zap.String("cfg.Name", cfg.Name))
		return
	}
	r.loggingConfig = logcfg
	logging.FromContext(r.loggingContext).Debug("Update from logging ConfigMap", zap.Any("ConfigMap", cfg))
}

// TODO determine how to push the updated metrics config to existing data plane Pods.
func (r *Reconciler) UpdateFromMetricsConfigMap(cfg *corev1.ConfigMap) {
	if cfg != nil {
		delete(cfg.Data, "_example")
	}

	r.metricsConfig = &metrics.ExporterOptions{
		Domain:    metrics.Domain(),
		Component: component,
		ConfigMap: cfg.Data,
	}
	logging.FromContext(r.loggingContext).Debug("Update from metrics ConfigMap", zap.Any("ConfigMap", cfg))
}
