//go:generate opencontrolplane-gen
package e2e

import (
	"context"
	"testing"
	"time"

	// opencontrolplane-gen:if SAMPLECODE=true
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	// opencontrolplane-gen:fi
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	// opencontrolplane-gen:if SAMPLECODE=true
	"github.com/openmcp-project/openmcp-testing/pkg/clusterutils"
	// opencontrolplane-gen:fi
	"github.com/openmcp-project/openmcp-testing/pkg/providers"

	// opencontrolplane-gen:replace github.com/openmcp-project/service-provider-template=MODULE
	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
	// opencontrolplane-gen:if SAMPLECODE=true
	openmcpconditions "github.com/openmcp-project/openmcp-testing/pkg/conditions"
	// opencontrolplane-gen:fi
)

func TestServiceProvider(t *testing.T) {
	basicProviderTest := features.New("provider test").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiv1alpha1.AddToScheme(c.Client().Resources().GetScheme())
			config := &apiv1alpha1.ProviderConfig{}
			// opencontrolplane-gen:replace configname=PROVIDER_NAME
			config.SetName("configname")
			if err := c.Client().Resources().Create(ctx, config); err != nil {
				t.Errorf("failed to create ProviderConfig object: %v", err)
			}
			return ctx
		}).
		Setup(providers.CreateMCP("test-controlplane")).
		// opencontrolplane-gen:if SAMPLECODE=true
		Assess("verify provider can be successfully consumed",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				// opencontrolplane-gen:replace Foo=KIND
				api := &apiv1alpha1.Foo{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				if err := config.Client().Resources().Create(ctx, api); err != nil {
					// opencontrolplane-gen:replace Foo=KIND
					t.Errorf("failed to create Foo object: %v", err)
				}
				if err := wait.For(openmcpconditions.Match(api, config, "Ready", corev1.ConditionTrue)); err != nil {
					t.Error(err)
				}
				return ctx
			},
		).
		Assess("verify domain objects can be created",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				mcpConfig, err := clusterutils.MCPConfig(ctx, c, "test-controlplane")
				if err != nil {
					t.Error(err)
					return ctx
				}
				domainObj := &unstructured.Unstructured{}
				domainObj.SetName("test-domain-object")
				domainObj.SetNamespace("default")
				domainObj.SetAPIVersion("example.domain/v1alpha1")
				domainObj.SetKind("Foo")
				if err := mcpConfig.Client().Resources().Create(ctx, domainObj); err != nil {
					t.Errorf("failed to create domain object on controlplane: %v", err)
				}
				return ctx
			},
		).
		Assess("verify service deletion is blocked due to existing domain service object",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				// opencontrolplane-gen:replace Foo=KIND
				api := &apiv1alpha1.Foo{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				if err := config.Client().Resources().Delete(ctx, api); err != nil {
					// opencontrolplane-gen:replace Foo=KIND
					t.Errorf("failed to delete Foo object: %v", err)
				}
				// verify object is stuck in Terminating with UserResourcesPresent reason
				if err := wait.For(func(ctx context.Context) (bool, error) {
					if err := config.Client().Resources().Get(ctx, api.GetName(), api.GetNamespace(), api); err != nil {
						return false, nil
					}
					if c := meta.FindStatusCondition(api.Status.Conditions, "DeletionBlocked"); c != nil && c.Status == metav1.ConditionTrue {
						return true, nil
					}
					return false, nil
				}); err != nil {
					t.Errorf("expected deletion to be blocked with reason UserResourcesPresent: %v", err)
				}
				return ctx
			},
		).
		Assess("delete domain object",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				mcpConfig, err := clusterutils.MCPConfig(ctx, c, "test-controlplane")
				if err != nil {
					t.Error(err)
					return ctx
				}
				domainObj := &unstructured.Unstructured{}
				domainObj.SetName("test-domain-object")
				domainObj.SetNamespace("default")
				domainObj.SetAPIVersion("example.domain/v1alpha1")
				domainObj.SetKind("Foo")
				if err := mcpConfig.Client().Resources().Delete(ctx, domainObj); err != nil {
					t.Errorf("failed to create domain object on controlplane: %v", err)
				}
				return ctx
			},
		).
		Assess("verify service is deleted",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				config := c
				config, err := clusterutils.OnboardingConfig()
				if err != nil {
					t.Error(err)
					return ctx
				}
				apiv1alpha1.AddToScheme(config.Client().Resources().GetScheme())
				// opencontrolplane-gen:replace Foo=KIND
				api := &apiv1alpha1.Foo{}
				api.SetName("test-controlplane")
				api.SetNamespace("default")
				if err := wait.For(conditions.New(config.Client().Resources()).ResourceDeleted(api)); err != nil {
					// opencontrolplane-gen:replace Foo=KIND
					t.Errorf("expected Foo to be deleted after domain object removal, but it still exists: %v", err)
				}
				return ctx
			},
		).
		// opencontrolplane-gen:fi
		// opencontrolplane-gen:if SAMPLECODE=false
		// TODO add assess steps
		// opencontrolplane-gen:fi
		Teardown(providers.DeleteMCP("test-controlplane", wait.WithTimeout(5*time.Minute)))
	testenv.Test(t, basicProviderTest.Feature())
}
