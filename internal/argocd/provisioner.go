package argocd

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	apiv1alpha1 "github.com/openmcp-project/service-provider-argocd/api/v1alpha1"
)

// Provisioner installs ArgoCD onto a Managed Control Plane by declaring Flux
// resources (OCIRepository + HelmRelease) on the platform cluster. The Flux
// runtime pulls the ArgoCD Helm chart and installs it into the MCP via the
// referenced kubeconfig. The provider therefore never installs anything
// itself; it only expresses the desired state declaratively.
type Provisioner struct {
	// platformClient writes the Flux resources onto the platform cluster.
	platformClient client.Client
	// mcpClient reads workload state (e.g. Applications) from the MCP.
	mcpClient client.Client
	// tenantNamespace is the namespace on the platform cluster that holds the
	// Flux resources and the MCP kubeconfig secret for this instance.
	tenantNamespace string
	// mcpNamespace is the namespace on the MCP into which ArgoCD is installed.
	mcpNamespace string
	// kubeConfigSecretName references the MCP access secret in tenantNamespace.
	kubeConfigSecretName string
	// podNamespace is the namespace the service provider itself runs in, on the
	// platform cluster. It is the source namespace for image pull secrets
	// referenced via values.global.imagePullSecrets.
	podNamespace string
	// pollInterval controls how often Flux reconciles the resources.
	pollInterval time.Duration
	// exposure holds the resolved external-access intent for the ArgoCD server.
	exposure ExposureValues
	// reloader, when non-nil, installs the Stakater Reloader addon alongside
	// ArgoCD. Reloader is best-effort: its failures are logged but never
	// returned as Install errors.
	reloader *apiv1alpha1.ReloaderConfig
}

// ProvisionerConfig groups the inputs required to build a Provisioner.
type ProvisionerConfig struct {
	PlatformClient       client.Client
	MCPClient            client.Client
	TenantNamespace      string
	MCPNamespace         string
	KubeConfigSecretName string
	PodNamespace         string
	PollInterval         time.Duration
	Exposure             ExposureValues
	Reloader             *apiv1alpha1.ReloaderConfig
}

// NewProvisioner constructs a Provisioner, applying the default target
// namespace when none is provided.
func NewProvisioner(cfg ProvisionerConfig) *Provisioner {
	if cfg.MCPNamespace == "" {
		cfg.MCPNamespace = DefaultNamespace
	}
	return &Provisioner{
		platformClient:       cfg.PlatformClient,
		mcpClient:            cfg.MCPClient,
		tenantNamespace:      cfg.TenantNamespace,
		mcpNamespace:         cfg.MCPNamespace,
		kubeConfigSecretName: cfg.KubeConfigSecretName,
		podNamespace:         cfg.PodNamespace,
		pollInterval:         cfg.PollInterval,
		exposure:             cfg.Exposure,
		reloader:             cfg.Reloader,
	}
}

// Install ensures the OCIRepository and HelmRelease exist and reflect the
// requested version. It is idempotent.
func (p *Provisioner) Install(ctx context.Context, version apiv1alpha1.ArgoCDVersion) error {
	if version.ChartURL == nil {
		return fmt.Errorf("version %q is missing a chart URL", version.Version)
	}

	if err := p.reconcileImagePullSecrets(ctx, version.Values); err != nil {
		return fmt.Errorf("reconciling image pull secrets: %w", err)
	}

	if err := p.applyOCIRepository(ctx, version); err != nil {
		return fmt.Errorf("reconciling OCIRepository: %w", err)
	}
	if err := p.applyHelmRelease(ctx, version); err != nil {
		return fmt.Errorf("reconciling HelmRelease: %w", err)
	}

	p.reconcileReloader(ctx)
	return nil
}

// reconcileReloader applies or removes the Reloader addon resources. It is
// intentionally fire-and-forget: any error is logged and swallowed so that
// Reloader health never bubbles up to the ArgoCD CR reconciliation loop.
func (p *Provisioner) reconcileReloader(ctx context.Context) {
	log := logf.FromContext(ctx)
	if p.reloader != nil {
		if err := p.applyReloaderOCIRepository(ctx); err != nil {
			log.Error(err, "failed to reconcile Reloader OCIRepository; addon will not be updated this cycle")
		}
		if err := p.applyReloaderHelmRelease(ctx); err != nil {
			log.Error(err, "failed to reconcile Reloader HelmRelease; addon will not be updated this cycle")
		}
		return
	}
	// Reloader disabled — clean up any resources left from a previous config so
	// they do not run unmanaged on the MCP.
	if err := p.removeReloaderResources(ctx); err != nil {
		log.Error(err, "failed to remove orphaned Reloader resources")
	}
}

// removeReloaderResources deletes the Reloader addon's Flux resources from the
// platform cluster. Missing resources are ignored, so the call is idempotent
// and safe to run whether or not Reloader was ever installed.
func (p *Provisioner) removeReloaderResources(ctx context.Context) error {
	reloaderHR := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: reloaderHelmReleaseName, Namespace: p.tenantNamespace},
	}
	if err := p.platformClient.Delete(ctx, reloaderHR); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting Reloader HelmRelease: %w", err)
	}
	reloaderRepo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{Name: reloaderOCIRepositoryName, Namespace: p.tenantNamespace},
	}
	if err := p.platformClient.Delete(ctx, reloaderRepo); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting Reloader OCIRepository: %w", err)
	}
	return nil
}

// Ready reports whether the HelmRelease has reached its Ready condition, which
// signals that ArgoCD has been successfully installed on the MCP.
func (p *Provisioner) Ready(ctx context.Context) (bool, string, error) {
	hr := &helmv2.HelmRelease{}
	key := client.ObjectKey{Name: helmReleaseName, Namespace: p.tenantNamespace}
	if err := p.platformClient.Get(ctx, key, hr); err != nil {
		return false, "", fmt.Errorf("getting HelmRelease: %w", err)
	}

	if cond := conditions.Get(hr, fluxmeta.ReadyCondition); cond != nil {
		return cond.Status == metav1.ConditionTrue, cond.Message, nil
	}
	return false, "HelmRelease has not reported readiness yet", nil
}

// Uninstall deletes the Flux resources. Deleting the HelmRelease triggers Flux
// to uninstall the ArgoCD release from the MCP.
func (p *Provisioner) Uninstall(ctx context.Context) error {
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: helmReleaseName, Namespace: p.tenantNamespace},
	}
	if err := p.platformClient.Delete(ctx, hr); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting HelmRelease: %w", err)
	}

	repo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{Name: ociRepositoryName, Namespace: p.tenantNamespace},
	}
	if err := p.platformClient.Delete(ctx, repo); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting OCIRepository: %w", err)
	}

	if err := p.removeImagePullSecrets(ctx); err != nil {
		return fmt.Errorf("removing replicated image pull secrets: %w", err)
	}

	return p.removeReloaderResources(ctx)
}

// IsUninstalled reports whether all managed Flux resources have been fully
// removed from the platform cluster. The ArgoCD and Reloader OCIRepositories and
// HelmReleases must all be gone before the ArgoCD CR finalizer is released.
func (p *Provisioner) IsUninstalled(ctx context.Context) (bool, error) {
	hr := &helmv2.HelmRelease{}
	err := p.platformClient.Get(ctx, client.ObjectKey{Name: helmReleaseName, Namespace: p.tenantNamespace}, hr)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("getting HelmRelease: %w", err)
	}
	repo := &sourcev1.OCIRepository{}
	err = p.platformClient.Get(ctx, client.ObjectKey{Name: ociRepositoryName, Namespace: p.tenantNamespace}, repo)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("getting OCIRepository: %w", err)
	}

	reloaderHR := &helmv2.HelmRelease{}
	err = p.platformClient.Get(ctx, client.ObjectKey{Name: reloaderHelmReleaseName, Namespace: p.tenantNamespace}, reloaderHR)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("getting Reloader HelmRelease: %w", err)
	}
	reloaderRepo := &sourcev1.OCIRepository{}
	err = p.platformClient.Get(ctx, client.ObjectKey{Name: reloaderOCIRepositoryName, Namespace: p.tenantNamespace}, reloaderRepo)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("getting Reloader OCIRepository: %w", err)
	}

	return true, nil
}

// CountUserArgoCDResources returns the number of ArgoCD Application and
// ApplicationSet resources still present on the MCP. Missing CRDs are treated
// as zero.
func (p *Provisioner) CountUserArgoCDResources(ctx context.Context) (int, error) {
	applications, err := p.countResourcesByGVK(ctx, applicationListGVK, "ArgoCD Applications")
	if err != nil {
		return 0, err
	}

	applicationSets, err := p.countResourcesByGVK(ctx, applicationSetListGVK, "ArgoCD ApplicationSets")
	if err != nil {
		return 0, err
	}

	appProjects, err := p.countResourcesByGVK(ctx, appProjectListGVK, "ArgoCD AppProjects")
	if err != nil {
		return 0, err
	}

	total := applications + applicationSets + appProjects
	return total, nil
}

// countResourcesByGVK returns the number of MCP resources for the given list
// kind. A missing CRD is treated as zero.
func (p *Provisioner) countResourcesByGVK(ctx context.Context, gvk schema.GroupVersionKind, resourceName string) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	if err := p.mcpClient.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing %s: %w", resourceName, err)
	}
	return len(list.Items), nil
}

// applyOCIRepository creates or updates the OCIRepository that points at the
// ArgoCD Helm chart.
func (p *Provisioner) applyOCIRepository(ctx context.Context, version apiv1alpha1.ArgoCDVersion) error {
	repo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{Name: ociRepositoryName, Namespace: p.tenantNamespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, repo, func() error {
		setManagedByValue(repo, managedByArgoCDValue)
		repo.Spec = sourcev1.OCIRepositorySpec{
			Interval: metav1.Duration{Duration: p.pollInterval},
			URL:      *version.ChartURL,
			Reference: &sourcev1.OCIRepositoryRef{
				Tag: version.ChartVersion,
			},
		}
		if version.ChartPullSecret != "" {
			repo.Spec.SecretRef = &fluxmeta.LocalObjectReference{Name: version.ChartPullSecret}
		}
		return nil
	})
	return err
}

// applyHelmRelease creates or updates the HelmRelease that installs the ArgoCD
// chart onto the MCP via the referenced kubeconfig.
func (p *Provisioner) applyHelmRelease(ctx context.Context, version apiv1alpha1.ArgoCDVersion) error {
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: helmReleaseName, Namespace: p.tenantNamespace},
	}
	values, err := buildValues(version.Values, p.exposure)
	if err != nil {
		return fmt.Errorf("building Helm values: %w", err)
	}

	_, err = controllerutil.CreateOrUpdate(ctx, p.platformClient, hr, func() error {
		setManagedByValue(hr, managedByArgoCDValue)
		hr.Spec = helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: p.pollInterval},
			ChartRef: &helmv2.CrossNamespaceSourceReference{
				Kind:      "OCIRepository",
				Name:      ociRepositoryName,
				Namespace: p.tenantNamespace,
			},
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{
					Name: p.kubeConfigSecretName,
					Key:  kubeConfigSecretKey,
				},
			},
			Install: &helmv2.Install{
				CRDs:            helmv2.Create,
				CreateNamespace: true,
				Remediation:     &helmv2.InstallRemediation{Retries: 3},
			},
			Upgrade: &helmv2.Upgrade{
				CRDs:        helmv2.CreateReplace,
				Remediation: &helmv2.UpgradeRemediation{Retries: 3},
			},
			Uninstall: &helmv2.Uninstall{
				KeepHistory: false,
				Timeout:     &metav1.Duration{Duration: 5 * time.Minute},
			},
			DriftDetection:   &helmv2.DriftDetection{Mode: helmv2.DriftDetectionEnabled},
			Values:           values,
			TargetNamespace:  p.mcpNamespace,
			StorageNamespace: p.mcpNamespace,
		}
		return nil
	})
	return err
}

// applyReloaderOCIRepository creates or updates the OCIRepository for the
// Stakater Reloader Helm chart. It uses managedByReloaderValue so the
// framework's HelmRelease watch does not see this resource.
func (p *Provisioner) applyReloaderOCIRepository(ctx context.Context) error {
	repo := &sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{Name: reloaderOCIRepositoryName, Namespace: p.tenantNamespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, repo, func() error {
		setManagedByValue(repo, managedByReloaderValue)
		repo.Spec = sourcev1.OCIRepositorySpec{
			Interval: metav1.Duration{Duration: p.pollInterval},
			URL:      p.reloader.ChartURL,
			Reference: &sourcev1.OCIRepositoryRef{
				Tag: p.reloader.ChartVersion,
			},
		}
		if p.reloader.ChartPullSecret != "" {
			repo.Spec.SecretRef = &fluxmeta.LocalObjectReference{Name: p.reloader.ChartPullSecret}
		}
		return nil
	})
	return err
}

// applyReloaderHelmRelease creates or updates the HelmRelease that installs
// the Reloader chart onto the MCP in the same namespace as ArgoCD. It uses
// managedByReloaderValue so the framework's HelmRelease watch does not see
// this resource and cannot cascade Reloader failures to the ArgoCD CR.
func (p *Provisioner) applyReloaderHelmRelease(ctx context.Context) error {
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: reloaderHelmReleaseName, Namespace: p.tenantNamespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, hr, func() error {
		setManagedByValue(hr, managedByReloaderValue)
		hr.Spec = helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: p.pollInterval},
			ChartRef: &helmv2.CrossNamespaceSourceReference{
				Kind:      "OCIRepository",
				Name:      reloaderOCIRepositoryName,
				Namespace: p.tenantNamespace,
			},
			KubeConfig: &fluxmeta.KubeConfigReference{
				SecretRef: &fluxmeta.SecretKeyReference{
					Name: p.kubeConfigSecretName,
					Key:  kubeConfigSecretKey,
				},
			},
			Install: &helmv2.Install{
				CreateNamespace: false,
				Remediation:     &helmv2.InstallRemediation{Retries: 3},
			},
			Upgrade: &helmv2.Upgrade{
				Remediation: &helmv2.UpgradeRemediation{Retries: 3},
			},
			Uninstall: &helmv2.Uninstall{
				KeepHistory: false,
				Timeout:     &metav1.Duration{Duration: 5 * time.Minute},
			},
			DriftDetection:   &helmv2.DriftDetection{Mode: helmv2.DriftDetectionEnabled},
			Values:           p.reloader.Values,
			TargetNamespace:  p.mcpNamespace,
			StorageNamespace: p.mcpNamespace,
		}
		return nil
	})
	return err
}

// reconcileImagePullSecrets copies every Secret referenced via
// values.global.imagePullSecrets from the service provider's own namespace
// (podNamespace) into the ArgoCD namespace on the MCP, then removes any
// previously-replicated secret that is no longer referenced.
func (p *Provisioner) reconcileImagePullSecrets(ctx context.Context, values *apiextensionsv1.JSON) error {
	names, err := ImagePullSecretNames(values)
	logf.FromContext(ctx).V(1).Info("reconciling image pull secrets stan1", "names", names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := p.copyImagePullSecret(ctx, name); err != nil {
			return err
		}
	}
	return p.pruneImagePullSecrets(ctx, names)
}

// copyImagePullSecret replicates a single Secret by name from podNamespace on
// the platform cluster to mcpNamespace on the MCP. It is a create-or-update so
// that upstream changes (e.g. credential rotation) are picked up on the next
// reconciliation.
func (p *Provisioner) copyImagePullSecret(ctx context.Context, name string) error {
	source := &corev1.Secret{}
	if err := p.platformClient.Get(ctx, client.ObjectKey{Name: name, Namespace: p.podNamespace}, source); err != nil {
		return fmt.Errorf("getting image pull secret %q from namespace %q: %w", name, p.podNamespace, err)
	}

	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.mcpNamespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, p.mcpClient, target, func() error {
		setManagedByValue(target, imagePullSecretManagedByValue)
		target.Type = source.Type
		target.Data = source.Data
		return nil
	})
	if err != nil {
		return fmt.Errorf("replicating image pull secret %q to namespace %q on the MCP: %w", name, p.mcpNamespace, err)
	}
	return nil
}

// pruneImagePullSecrets deletes replicated image pull Secrets in mcpNamespace
// that are no longer referenced. Only Secrets stamped with
// imagePullSecretManagedByValue are considered, so hand-created secrets are
// never touched. Passing a nil/empty keep list removes all replicated secrets.
func (p *Provisioner) pruneImagePullSecrets(ctx context.Context, keep []string) error {
	keepSet := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		keepSet[name] = struct{}{}
	}

	list := &corev1.SecretList{}
	if err := p.mcpClient.List(ctx, list,
		client.InNamespace(p.mcpNamespace),
		client.MatchingLabels{managedByLabel: imagePullSecretManagedByValue},
	); err != nil {
		return fmt.Errorf("listing replicated image pull secrets in namespace %q: %w", p.mcpNamespace, err)
	}

	for i := range list.Items {
		if _, ok := keepSet[list.Items[i].Name]; ok {
			continue
		}
		if err := p.mcpClient.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting orphaned image pull secret %q: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// removeImagePullSecrets deletes all replicated image pull secrets from the
// MCP's ArgoCD namespace. It is called on Uninstall.
func (p *Provisioner) removeImagePullSecrets(ctx context.Context) error {
	return p.pruneImagePullSecrets(ctx, nil)
}

// setManagedByValue sets the managed-by label on obj to the given value,
// initializing the label map when necessary.
func setManagedByValue(obj client.Object, value string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[managedByLabel] = value
	obj.SetLabels(labels)
}
