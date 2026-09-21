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
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	reasonReconciling     = "Reconciling"
	reasonInvalidVersion  = "InvalidVersion"
	reasonInvalidExposure = "InvalidExposure"
	reasonEndpointPending = "EndpointPending"
	reasonInstallFailed   = "InstallFailed"
	reasonUninstalling    = "Uninstalling"
	reasonDeletionBlocked = "UserResourcesPresent"

	conditionDeletionBlocked = "DeletionBlocked"

	// phaseFailed is a terminal phase used for invalid user input that will not
	// resolve without a spec change. Unlike "Progressing", it signals that the
	// controller is not actively working toward readiness.
	phaseFailed = "Failed"

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
	serviceprovider.StatusProgressing(obj, reasonReconciling, "Reconcile in progress")

	version, ok := pc.SelectVersion(obj.Spec.Version)
	if !ok {
		// Invalid user input: report it but do not requeue with an error, since
		// retrying without a spec change would be futile.
		msg := fmt.Sprintf("requested version %q is not offered by the provider configuration", obj.Spec.Version)
		log.Info("rejecting ArgoCD request", "reason", msg)
		statusFailed(obj, reasonInvalidVersion, msg)
		return ctrl.Result{}, nil
	}

	if err := validateExposure(obj, clusterCtx.MCPCluster.APIServerEndpoint()); err != nil {
		// User error: report and stop, do not requeue with an error.
		log.Info("rejecting ArgoCD request", "reason", err.Error())
		statusFailed(obj, reasonInvalidExposure, err.Error())
		return ctrl.Result{}, nil
	}

	if obj.Spec.Exposure != nil {
		if _, ok := pc.ReloaderConfig(); !ok {
			log.Info("exposure requested but Reloader is not enabled in the ProviderConfig; "+
				"argocd-server will not auto-restart when its managed TLS certificate rotates",
				"host", obj.Spec.Exposure.Host)
		}
	}

	provisioner, err := r.newProvisioner(obj, pc, clusterCtx)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := provisioner.Install(ctx, version); err != nil {
		log.Error(err, "failed to declare ArgoCD installation")
		serviceprovider.StatusProgressing(obj, reasonInstallFailed, err.Error())
		return ctrl.Result{}, err
	}

	ready, message, err := provisioner.Ready(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		serviceprovider.StatusProgressing(obj, reasonReconciling, message)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// ArgoCD is installed; resolve external exposure (if requested) before
	// declaring the resource Ready.
	endpoint, endpointReady, err := provisioner.Endpoint(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !endpointReady {
		// Do not publish the endpoint until it is actually reachable, per the
		// status.endpoint contract.
		obj.Status.Endpoint = ""
		serviceprovider.StatusProgressing(obj, reasonEndpointPending, "waiting for LoadBalancer address and managed TLS certificate")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}
	obj.Status.Endpoint = endpoint

	serviceprovider.StatusReady(obj)
	return ctrl.Result{}, nil
}

// Delete tears down ArgoCD for the given resource. It is invoked on every
// delete event and blocks while user-owned Applications still exist to avoid
// orphaning workloads.
func (r *ArgoCDReconciler) Delete(ctx context.Context, obj *apiv1alpha1.ArgoCD, pc *apiv1alpha1.ProviderConfig, clusterCtx clusteraccess.ClusterContext) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	serviceprovider.StatusTerminating(obj)

	provisioner, err := r.newProvisioner(obj, pc, clusterCtx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Guard: refuse to delete while the user still has Applications.
	applications, err := provisioner.CountUserApplications(ctx)
	if err != nil {
		log.Error(err, "failed to list ArgoCD Applications")
		return ctrl.Result{}, err
	}
	if applications > 0 {
		meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
			Type:               conditionDeletionBlocked,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: obj.GetGeneration(),
			Reason:             reasonDeletionBlocked,
			Message:            fmt.Sprintf("deletion blocked: %d ArgoCD Application(s) still present", applications),
		})
		obj.SetObservedGeneration(obj.GetGeneration())
		obj.SetPhase("Terminating")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if err := provisioner.Uninstall(ctx); err != nil {
		log.Error(err, "failed to uninstall ArgoCD")
		serviceprovider.StatusTerminatingWithReason(obj, reasonUninstalling, err.Error())
		return ctrl.Result{}, err
	}

	uninstalled, err := provisioner.IsUninstalled(ctx)
	if err != nil {
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
		Exposure:             resolveExposure(obj, pc, clusterCtx.MCPCluster.APIServerEndpoint()),
		Reloader:             resolveReloader(pc),
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

// statusFailed marks the resource as failed due to invalid user input. It sets
// the Ready condition to False with the given reason/message and a terminal
// "Failed" phase, distinguishing a spec that must be corrected from work that
// is still in progress. The openmcp runtime only ships Progressing/Ready/
// Terminating helpers, so this sets the phase explicitly.
func statusFailed(obj *apiv1alpha1.ArgoCD, reason, message string) {
	serviceprovider.StatusProgressing(obj, reason, message)
	obj.SetPhase(phaseFailed)
}

// hostPattern validates the tenant-supplied exposure host. It accepts a single
// DNS label (for example "argocd") or a dotted, fully-qualified domain name.
// Each label is 1-63 characters, starts and ends with an alphanumeric, and may
// contain hyphens in between. Matching is case-insensitive.
var hostPattern = regexp.MustCompile(`^(?i)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// validateExposure rejects requests whose exposure host is missing, malformed,
// or cannot be completed into a fully-qualified name. It returns nil when
// exposure is not requested. Because this runs before any provisioning, an
// invalid host results in no resources being created on the MCP.
func validateExposure(obj *apiv1alpha1.ArgoCD, mcpAPIServerHost string) error {
	e := obj.Spec.Exposure
	if e == nil {
		return nil
	}
	if e.Host == "" {
		return fmt.Errorf("spec.exposure.host is required when spec.exposure is set")
	}
	if !hostPattern.MatchString(e.Host) {
		return fmt.Errorf("spec.exposure.host %q is not a valid DNS name: each label must be 1-63 characters, start and end with an alphanumeric, and contain only letters, digits or hyphens", e.Host)
	}

	for _, cidr := range e.AllowedIPs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("spec.exposure.allowedIPs entry %q is not a valid CIDR range (for example 203.0.113.0/24 or 203.0.113.5/32): %w", cidr, err)
		}
	}

	fqdn, err := composeHost(e.Host, mcpAPIServerHost)
	if err != nil {
		return err
	}
	if len(fqdn) > 253 {
		return fmt.Errorf("resolved exposure host %q exceeds the maximum DNS name length of 253 characters", fqdn)
	}
	return nil
}

// deriveRootDomain extracts the shoot's root DNS zone from its apiserver URL by
// stripping the leading "api." label.
func deriveRootDomain(serverURL string) (string, error) {
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" {
		return "", fmt.Errorf("empty apiserver URL")
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parsing apiserver URL %q: %w", serverURL, err)
	}
	host := u.Hostname()
	if host == "" {
		host = serverURL
	}
	labels := strings.Split(host, ".")
	if len(labels) < 3 || labels[0] != "api" {
		return "", fmt.Errorf("apiserver host %q is not shaped like \"api.<domain>\"; cannot derive base domain", host)
	}
	return strings.Join(labels[1:], "."), nil
}

// composeHost turns the tenant-provided host into a fully-qualified domain name.
func composeHost(host, mcpAPIServerHost string) (string, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if strings.Contains(host, ".") {
		return host, nil
	}
	baseDomain, err := deriveRootDomain(mcpAPIServerHost)
	if err != nil {
		return "", fmt.Errorf("spec.exposure.host %q is a bare label but the base domain could not be derived from the MCP apiserver host: %w", host, err)
	}
	return host + "." + baseDomain, nil
}

// resolveExposure combines the tenant's exposure request with the platform's
// exposure policy. It returns a disabled value when exposure is not requested.
func resolveExposure(obj *apiv1alpha1.ArgoCD, pc *apiv1alpha1.ProviderConfig, mcpAPIServerHost string) argocd.ExposureValues {
	e := obj.Spec.Exposure
	if e == nil {
		return argocd.ExposureValues{}
	}
	policy := pc.ExposurePolicy()
	_, reloaderEnabled := pc.ReloaderConfig()
	host, err := composeHost(e.Host, mcpAPIServerHost)
	if err != nil {
		host = e.Host
	}
	return argocd.ExposureValues{
		Enabled:         true,
		Host:            host,
		AllowedIPs:      e.AllowedIPs,
		DNSClass:        policy.DNSClass,
		DNSTTL:          policy.DNSTTL,
		CertPurpose:     policy.CertPurpose,
		ReloaderEnabled: reloaderEnabled,
	}
}

// resolveReloader returns the Reloader configuration from the ProviderConfig,
// or nil when Reloader is not enabled.
func resolveReloader(pc *apiv1alpha1.ProviderConfig) *apiv1alpha1.ReloaderConfig {
	cfg, ok := pc.ReloaderConfig()
	if !ok {
		return nil
	}
	return &cfg
}
