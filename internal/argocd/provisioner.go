package argocd

import (
	"context"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
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
	// pollInterval controls how often Flux reconciles the resources.
	pollInterval time.Duration
}

// ProvisionerConfig groups the inputs required to build a Provisioner.
type ProvisionerConfig struct {
	PlatformClient       client.Client
	MCPClient            client.Client
	TenantNamespace      string
	MCPNamespace         string
	KubeConfigSecretName string
	PollInterval         time.Duration
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
		pollInterval:         cfg.PollInterval,
	}
}

// Install ensures the OCIRepository and HelmRelease exist and reflect the
// requested version. It is idempotent.
func (p *Provisioner) Install(ctx context.Context, version apiv1alpha1.ArgoCDVersion) error {
	if version.ChartURL == nil {
		return fmt.Errorf("version %q is missing a chart URL", version.Version)
	}

	if err := p.applyOCIRepository(ctx, version); err != nil {
		return fmt.Errorf("reconciling OCIRepository: %w", err)
	}
	if err := p.applyHelmRelease(ctx, version); err != nil {
		return fmt.Errorf("reconciling HelmRelease: %w", err)
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
	return nil
}

// IsUninstalled reports whether the HelmRelease has been fully removed from the
// platform cluster, signalling that teardown has completed.
func (p *Provisioner) IsUninstalled(ctx context.Context) (bool, error) {
	hr := &helmv2.HelmRelease{}
	key := client.ObjectKey{Name: helmReleaseName, Namespace: p.tenantNamespace}
	err := p.platformClient.Get(ctx, key, hr)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("getting HelmRelease: %w", err)
	default:
		return false, nil
	}
}

// CountUserApplications returns the number of ArgoCD Application resources still
// present on the MCP. It is used as a deletion guard. A missing Application CRD
// is treated as zero.
func (p *Provisioner) CountUserApplications(ctx context.Context) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(applicationListGVK)

	if err := p.mcpClient.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing ArgoCD Applications: %w", err)
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
		setManagedBy(repo)
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
	_, err := controllerutil.CreateOrUpdate(ctx, p.platformClient, hr, func() error {
		setManagedBy(hr)
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
			Values:           version.Values,
			TargetNamespace:  p.mcpNamespace,
			StorageNamespace: p.mcpNamespace,
		}
		return nil
	})
	return err
}

// setManagedBy stamps the provider ownership label onto an object.
func setManagedBy(obj client.Object) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[managedByLabel] = managedByValue
	obj.SetLabels(labels)
}
