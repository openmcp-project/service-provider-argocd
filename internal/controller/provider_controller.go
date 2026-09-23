/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider"
	clusteraccess "github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"
	libutils "github.com/openmcp-project/openmcp-operator/lib/utils"

	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
	"github.com/openmcp-project/service-provider-argocd/internal/argocd"
)

// Condition reasons surfaced on the ArgoCD resource status.
const (
	reasonReconciling               = "Reconciling"
	reasonInvalidVersion            = "InvalidVersion"
	reasonInstallFailed             = "InstallFailed"
	reasonUninstalling              = "Uninstalling"
	reasonProvisionerCreationFailed = "ProvisionerCreationFailed"
	reasonReadinessCheckFailed      = "ReadinessCheckFailed"
	reasonFailedToListApplications  = "FailedToListApplications"
	reasonTerminating               = "Terminating"

	// requeueInterval is how long to wait before re-checking asynchronous
	// progress (e.g. namespace teardown or blocked deletion).
	requeueInterval = 10 * time.Second
)

// ArgoCDReconciler reconciles an ArgoCD object by provisioning ArgoCD onto the
// requesting Managed Control Plane.
type ArgoCDReconciler struct {
	// OnboardingCluster is the cluster where this controller watches ArgoCD resources and reacts to their changes.
	OnboardingCluster *clusters.Cluster
	// PlatformCluster is the cluster where this controller is deployed and configured.
	PlatformCluster *clusters.Cluster
	// PodNamespace is the namespace where this controller is deployed in.
	PodNamespace string
}

// CreateOrUpdate provisions (or converges) ArgoCD for the given resource. It is
// invoked on every add or update event.
func (r *ArgoCDReconciler) CreateOrUpdate(ctx context.Context, obj *apiv1alpha1.ArgoCD, pc *apiv1alpha1.ProviderConfig, clusterCtx clusteraccess.ClusterContext) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	serviceprovider.StatusProgressing(obj, reasonReconciling, "Reconcile is in progress")

	version, ok := pc.SelectVersion(obj.Spec.Version)
	if !ok {
		// Invalid user input: report it but do not requeue with an error, since
		// retrying without a spec change would be futile.
		msg := fmt.Sprintf("requested version %q is not offered by the provider configuration", obj.Spec.Version)
		log.Error(fmt.Errorf("rejecting ArgoCD request"), "invalid version requested", "reason", msg)
		argocd.StatusFailed(obj, reasonInvalidVersion, msg)
		return ctrl.Result{}, nil
	}

	provisioner, err := r.newProvisioner(obj, pc, clusterCtx)
	if err != nil {
		log.Error(err, "failed to create provisioner")
		argocd.StatusFailed(obj, reasonProvisionerCreationFailed, err.Error())
		return ctrl.Result{}, err
	}

	if err := provisioner.Install(ctx, version); err != nil {
		log.Error(err, "failed to declare ArgoCD installation")
		argocd.StatusFailed(obj, reasonInstallFailed, err.Error())
		return ctrl.Result{}, err
	}

	ready, message, err := provisioner.Ready(ctx)
	if err != nil {
		log.Error(err, "failed to check readiness")
		argocd.StatusFailed(obj, reasonReadinessCheckFailed, err.Error())
		return ctrl.Result{}, err
	}
	if !ready {
		serviceprovider.StatusProgressing(obj, reasonReconciling, message)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	serviceprovider.StatusReady(obj)
	return ctrl.Result{}, nil
}

// Delete tears down ArgoCD for the given resource. It is invoked on every
// delete event and blocks while user-owned Applications still exist to avoid
// orphaning workloads.
func (r *ArgoCDReconciler) Delete(ctx context.Context, obj *apiv1alpha1.ArgoCD, pc *apiv1alpha1.ProviderConfig, clusterCtx clusteraccess.ClusterContext) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	serviceprovider.StatusTerminatingWithReason(obj, reasonTerminating, "Resource termination initiated; cleanup tasks are in progress")

	provisioner, err := r.newProvisioner(obj, pc, clusterCtx)
	if err != nil {
		log.Error(err, "failed to create provisioner")
		argocd.StatusFailed(obj, reasonProvisionerCreationFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Guard: refuse to delete while the user still has Applications.
	applications, err := provisioner.CountUserApplications(ctx)
	if err != nil {
		log.Error(err, "failed to list ArgoCD Applications")
		argocd.StatusFailed(obj, reasonFailedToListApplications, err.Error())
		return ctrl.Result{}, err
	}
	if applications > 0 {
		serviceprovider.StatusTerminatingWithReason(obj, reasonTerminating, fmt.Sprintf("deletion blocked: %d ArgoCD Application(s) still present", applications))
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if err := provisioner.Uninstall(ctx); err != nil {
		log.Error(err, "failed to uninstall ArgoCD")
		argocd.StatusFailed(obj, reasonUninstalling, err.Error())
		return ctrl.Result{}, err
	}

	uninstalled, err := provisioner.IsUninstalled(ctx)
	if err != nil {
		log.Error(err, "failed to check uninstallation status")
		argocd.StatusFailed(obj, reasonUninstalling, err.Error())
		return ctrl.Result{}, err
	}
	if !uninstalled {
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

// newProvisioner builds a Provisioner for the given reconcile request. The Flux
// resources live in the MCP's tenant namespace on the platform cluster, next to
// the MCP kubeconfig secret that the HelmRelease references.
func (r *ArgoCDReconciler) newProvisioner(obj *apiv1alpha1.ArgoCD, pc *apiv1alpha1.ProviderConfig, clusterCtx clusteraccess.ClusterContext) (*argocd.Provisioner, error) {
	tenantNamespace := clusterCtx.MCPAccessSecretKey.Namespace
	if tenantNamespace == "" {
		// Fall back to a stable, deterministic namespace derived from the request.
		ns, err := libutils.StableMCPNamespace(obj.Name, obj.Namespace)
		if err != nil {
			return nil, fmt.Errorf("determining tenant namespace: %w", err)
		}
		tenantNamespace = ns
	}

	return argocd.NewProvisioner(argocd.ProvisionerConfig{
		PlatformClient:       r.PlatformCluster.Client(),
		MCPClient:            clusterCtx.MCPCluster.Client(),
		TenantNamespace:      tenantNamespace,
		MCPNamespace:         providerNamespace(obj),
		KubeConfigSecretName: clusterCtx.MCPAccessSecretKey.Name,
		PollInterval:         pc.PollInterval(),
	}), nil
}

// providerNamespace resolves the target MCP namespace from the ArgoCD object,
// tolerating a nil config.
func providerNamespace(obj *apiv1alpha1.ArgoCD) string {
	if obj == nil {
		return ""
	}
	return obj.Spec.NamespaceOverride
}
