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
	// caBundleSecret is the name of the Secret holding the custom CA bundle.
	// It is copied from caBundleSourceNamespace into tenantNamespace so that
	// Flux OCIRepository resources can reference it via CertSecretRef.
	caBundleSecret string
	// caBundleSourceNamespace is the namespace (the service provider's pod
	// namespace) where the CA bundle Secret lives before being synced.
	caBundleSourceNamespace string
	// caCert holds the PEM-encoded CA certificate read from the CA bundle
	// Secret. It is populated by ensureCABundle and consumed by applyHelmRelease
	// to inject the CA into ArgoCD's trusted-certificate configuration.
	caCert string
}

// ProvisionerConfig groups the inputs required to build a Provisioner.
type ProvisionerConfig struct {
	PlatformClient          client.Client
	MCPClient               client.Client
	TenantNamespace         string
	MCPNamespace            string
	KubeConfigSecretName    string
	PodNamespace            string
	PollInterval            time.Duration
	Exposure                ExposureValues
	Reloader                *apiv1alpha1.ReloaderConfig
	CABundleSecret          string
	CABundleSourceNamespace string
}

// NewProvisioner constructs a Provisioner, applying the default target
// namespace when none is provided.
func NewProvisioner(cfg ProvisionerConfig) *Provisioner {
	if cfg.MCPNamespace == "" {
		cfg.MCPNamespace = DefaultNamespace
	}
	return &Provisioner{
		platformClient:          cfg.PlatformClient,
		mcpClient:               cfg.MCPClient,
		tenantNamespace:         cfg.TenantNamespace,
		mcpNamespace:            cfg.MCPNamespace,
		kubeConfigSecretName:    cfg.KubeConfigSecretName,
		podNamespace:            cfg.PodNamespace,
		pollInterval:            cfg.PollInterval,
		exposure:                cfg.Exposure,
		reloader:                cfg.Reloader,
		caBundleSecret:          cfg.CABundleSecret,
		caBundleSourceNamespace: cfg.CABundleSourceNamespace,
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

	// Chart pull secrets must exist in tenantNamespace *before* the
	// OCIRepository resources (ArgoCD's and Reloader's) are applied below,
	// since their spec.secretRef is a same-namespace LocalObjectReference.
	if err := p.reconcileChartPullSecrets(ctx, version); err != nil {
		return fmt.Errorf("reconciling chart pull secrets: %w", err)
	}

	if err := p.reconcileCABundle(ctx); err != nil {
		return fmt.Errorf("reconciling CA bundle: %w", err)
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
// to uninstall the ArgoCD release from the MCP. It does not remove the CA
// bundle secret — that must be done after IsUninstalled returns true, once the
// OCIRepository (which references the secret via certSecretRef) is fully gone.
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

	if err := p.removeChartPullSecrets(ctx); err != nil {
		return fmt.Errorf("removing replicated chart pull secrets: %w", err)
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

// CountUserArgoCDResources returns the number of user-created ArgoCD
// Application, ApplicationSet, and AppProject resources still present on the
// MCP. The "default" AppProject that ArgoCD provisions automatically in its
// install namespace is excluded — it is not a user workload, and counting it
// would keep the deletion guard permanently tripped. Missing CRDs are treated
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

	appProjects, err := p.countUserAppProjects(ctx)
	if err != nil {
		return 0, err
	}

	total := applications + applicationSets + appProjects
	return total, nil
}

// countUserAppProjects counts AppProjects on the MCP, excluding the "default"
// project that ArgoCD auto-creates in its install namespace. A missing CRD is
// treated as zero.
func (p *Provisioner) countUserAppProjects(ctx context.Context) (int, error) {
	list, err := p.listResourcesByGVK(ctx, appProjectListGVK, "ArgoCD AppProjects")
	if err != nil {
		return 0, err
	}

	count := 0
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == defaultAppProjectName && item.GetNamespace() == p.mcpNamespace {
			continue
		}
		count++
	}
	return count, nil
}

// countResourcesByGVK returns the number of MCP resources for the given list
// kind. A missing CRD is treated as zero.
func (p *Provisioner) countResourcesByGVK(ctx context.Context, gvk schema.GroupVersionKind, resourceName string) (int, error) {
	list, err := p.listResourcesByGVK(ctx, gvk, resourceName)
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// listResourcesByGVK lists the MCP resources for the given list kind. A missing
// CRD is treated as an empty list rather than an error.
func (p *Provisioner) listResourcesByGVK(ctx context.Context, gvk schema.GroupVersionKind, resourceName string) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	if err := p.mcpClient.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return list, nil
		}
		return nil, fmt.Errorf("listing %s: %w", resourceName, err)
	}
	return list, nil
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
		if p.caBundleSecret != "" {
			repo.Spec.CertSecretRef = &fluxmeta.LocalObjectReference{Name: p.caBundleSecret}
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
	values, err := buildValues(version.Values, p.exposure, p.caCert)
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
				Timeout:         &metav1.Duration{Duration: 10 * time.Minute},
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
		if p.caBundleSecret != "" {
			repo.Spec.CertSecretRef = &fluxmeta.LocalObjectReference{Name: p.caBundleSecret}
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
	logf.FromContext(ctx).V(1).Info("reconciling image pull secrets", "names", names)
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
//
// Image pull secrets are reconciled before the HelmRelease/OCIRepository are
// applied (see Install), which means on a brand-new MCP the target namespace
// may not exist yet — Flux only creates it once the Helm install actually
// runs. ensureMCPNamespace guards against that chicken-and-egg ordering so the
// very first reconciliation does not fail permanently with "namespace not
// found" before ArgoCD ever gets a chance to be installed.
func (p *Provisioner) copyImagePullSecret(ctx context.Context, name string) error {
	source := &corev1.Secret{}
	if err := p.platformClient.Get(ctx, client.ObjectKey{Name: name, Namespace: p.podNamespace}, source); err != nil {
		return fmt.Errorf("getting image pull secret %q from namespace %q: %w", name, p.podNamespace, err)
	}

	if err := p.ensureMCPNamespace(ctx); err != nil {
		return fmt.Errorf("ensuring namespace %q exists on the MCP: %w", p.mcpNamespace, err)
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

// ensureMCPNamespace creates mcpNamespace on the MCP if it does not already
// exist. It is idempotent and safe to call on every reconciliation.
func (p *Provisioner) ensureMCPNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.mcpNamespace}}
	if err := p.mcpClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
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

// reconcileChartPullSecrets copies the chart pull secrets referenced by the
// ArgoCD version (version.ChartPullSecret) and, when configured, the Reloader
// addon (p.reloader.ChartPullSecret) from the service provider's own
// namespace (podNamespace) into tenantNamespace on the platform cluster. That
// is where the matching Flux OCIRepository lives, and its spec.secretRef is a
// same-namespace LocalObjectReference, so the secret must be present there —
// unlike image pull secrets, which are replicated onto the MCP instead.
// Any previously-replicated chart pull secret no longer referenced is pruned.
func (p *Provisioner) reconcileChartPullSecrets(ctx context.Context, version apiv1alpha1.ArgoCDVersion) error {
	names := chartPullSecretNames(version, p.reloader)
	for _, name := range names {
		if err := p.copyChartPullSecret(ctx, name); err != nil {
			return err
		}
	}
	return p.pruneChartPullSecrets(ctx, names)
}

// chartPullSecretNames collects the distinct, non-empty chart pull secret
// names referenced by the ArgoCD version and, when configured, the Reloader
// addon.
func chartPullSecretNames(version apiv1alpha1.ArgoCDVersion, reloader *apiv1alpha1.ReloaderConfig) []string {
	seen := make(map[string]struct{}, 2)
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	add(version.ChartPullSecret)
	if reloader != nil {
		add(reloader.ChartPullSecret)
	}
	return names
}

// copyChartPullSecret replicates a single Secret by name from podNamespace on
// the platform cluster into tenantNamespace, also on the platform cluster. It
// is a create-or-update so that upstream changes (e.g. credential rotation)
// are picked up on the next reconciliation.
func (p *Provisioner) copyChartPullSecret(ctx context.Context, name string) error {
	source := &corev1.Secret{}
	if err := p.platformClient.Get(ctx, client.ObjectKey{Name: name, Namespace: p.podNamespace}, source); err != nil {
		return fmt.Errorf("getting chart pull secret %q from namespace %q: %w", name, p.podNamespace, err)
	}

	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.tenantNamespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, target, func() error {
		setManagedByValue(target, managedByArgoCDValue)
		if target.Labels == nil {
			target.Labels = map[string]string{}
		}
		target.Labels[chartPullSecretLabel] = "true"
		target.Type = source.Type
		target.Data = source.Data
		return nil
	})
	if err != nil {
		return fmt.Errorf("replicating chart pull secret %q to tenant namespace %q: %w", name, p.tenantNamespace, err)
	}
	return nil
}

// pruneChartPullSecrets deletes replicated chart pull Secrets in
// tenantNamespace that are no longer referenced. Only Secrets stamped with
// chartPullSecretLabel are considered, so hand-created secrets of the same
// name are never touched. Passing a nil/empty keep list removes all
// replicated chart pull secrets.
func (p *Provisioner) pruneChartPullSecrets(ctx context.Context, keep []string) error {
	keepSet := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		keepSet[name] = struct{}{}
	}

	list := &corev1.SecretList{}
	if err := p.platformClient.List(ctx, list,
		client.InNamespace(p.tenantNamespace),
		client.MatchingLabels{chartPullSecretLabel: "true"},
	); err != nil {
		return fmt.Errorf("listing replicated chart pull secrets in namespace %q: %w", p.tenantNamespace, err)
	}

	for i := range list.Items {
		if _, ok := keepSet[list.Items[i].Name]; ok {
			continue
		}
		if err := p.platformClient.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting orphaned chart pull secret %q: %w", list.Items[i].Name, err)
		}
	}
	return nil
}

// removeChartPullSecrets deletes all replicated chart pull secrets from the
// tenant namespace. It is called on Uninstall.
func (p *Provisioner) removeChartPullSecrets(ctx context.Context) error {
	return p.pruneChartPullSecrets(ctx, nil)
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

// reconcileCABundle syncs the CA bundle Secret when caBundleSecret is set, or
// removes any previously synced copy when it has been cleared. It is the single
// CA entry point called from Install.
func (p *Provisioner) reconcileCABundle(ctx context.Context) error {
	if p.caBundleSecret != "" {
		return p.ensureCABundle(ctx)
	}
	return p.RemoveCABundle(ctx)
}

// ensureCABundle reads the CA bundle Secret from the service provider namespace
// and copies it into the tenant namespace so that Flux OCIRepository resources
// can reference it via CertSecretRef (which requires a LocalObjectReference).
// It also populates p.caCert with the raw PEM content for injection into
// ArgoCD Helm values. It is a no-op when no CA bundle is configured.
func (p *Provisioner) ensureCABundle(ctx context.Context) error {
	if p.caBundleSecret == "" {
		return nil
	}

	src := &corev1.Secret{}
	if err := p.platformClient.Get(ctx, client.ObjectKey{
		Name:      p.caBundleSecret,
		Namespace: p.caBundleSourceNamespace,
	}, src); err != nil {
		return fmt.Errorf("reading CA bundle secret %q from namespace %q: %w", p.caBundleSecret, p.caBundleSourceNamespace, err)
	}

	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.caBundleSecret,
			Namespace: p.tenantNamespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, dst, func() error {
		setManagedByValue(dst, managedByArgoCDValue)
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		dst.Labels[caBundleLabel] = "true"
		dst.Data = src.Data
		dst.Type = src.Type
		return nil
	}); err != nil {
		return fmt.Errorf("syncing CA bundle secret to tenant namespace %q: %w", p.tenantNamespace, err)
	}

	p.caCert = string(src.Data["ca.crt"])
	return nil
}

// RemoveCABundle deletes the CA bundle Secret that was synced into the tenant
// namespace. It is a no-op when no CA bundle is configured or the secret no
// longer exists.
//
// During uninstall this must be called only after IsUninstalled returns true,
// so that the OCIRepository (which holds a certSecretRef to the secret) has
// already been fully removed by source-controller.
func (p *Provisioner) RemoveCABundle(ctx context.Context) error {
	if p.caBundleSecret != "" {
		// Fast path: name is known, delete directly.
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.caBundleSecret,
				Namespace: p.tenantNamespace,
			},
		}
		return client.IgnoreNotFound(p.platformClient.Delete(ctx, s))
	}

	// caBundleSecret was cleared from the ProviderConfig after a secret had
	// already been synced. Find the orphaned copy by the label stamped on it at
	// creation time and delete it.
	list := &corev1.SecretList{}
	if err := p.platformClient.List(ctx, list,
		client.InNamespace(p.tenantNamespace),
		client.MatchingLabels{caBundleLabel: "true"},
	); err != nil {
		return fmt.Errorf("listing CA bundle secrets in tenant namespace %q: %w", p.tenantNamespace, err)
	}
	for i := range list.Items {
		if err := client.IgnoreNotFound(p.platformClient.Delete(ctx, &list.Items[i])); err != nil {
			return fmt.Errorf("deleting CA bundle secret %q: %w", list.Items[i].Name, err)
		}
	}
	return nil
}
