package e2e

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/openmcp-project/openmcp-testing/pkg/providers"
	"github.com/openmcp-project/openmcp-testing/pkg/setup"
	"github.com/openmcp-project/openmcp-testing/pkg/setup/extensions"
	"github.com/openmcp-project/openmcp-testing/pkg/setup/extensions/fluxcd"
)

var testenv env.Environment

func TestMain(m *testing.M) {
	initLogging()
	version := mustVersion()
	openmcp := setup.OpenMCPSetup{
		Namespace: "openmcp-system",
		Operator: setup.OpenMCPOperatorSetup{
			Name: "openmcp-operator",
			// renovate: datasource=docker depName=ghcr.io/openmcp-project/images/openmcp-operator
			Image:        "ghcr.io/openmcp-project/images/openmcp-operator:v1.4.4",
			Environment:  "debug",
			PlatformName: "platform",
			// The operator Deployment's own readiness wait (inside
			// openmcp-testing's installOpenMCPOperator) uses this WaitOpts
			// field specifically — it is not covered by the top-level
			// OpenMCPSetup.WaitOpts below. Without it, klient/wait's 5-minute
			// default fires before a cold image pull / cluster warm-up on a
			// busy host completes, even though nothing is actually stuck.
			WaitOpts: []wait.Option{wait.WithTimeout(15 * time.Minute)},
		},
		ClusterProviders: []providers.ClusterProviderSetup{
			{
				Name: "kind",
				// renovate: datasource=docker depName=ghcr.io/openmcp-project/images/cluster-provider-kind
				Image:    "ghcr.io/openmcp-project/images/cluster-provider-kind:v0.7.4",
				WaitOpts: []wait.Option{wait.WithTimeout(15 * time.Minute)},
			},
		},
		ServiceProviders: []providers.ServiceProviderSetup{
			{
				Name:               "argocd",
				Image:              fmt.Sprintf("ghcr.io/openmcp-project/images/service-provider-argocd:%s", version),
				LoadImageToCluster: true,
				// Same reasoning as Operator.WaitOpts above: InstallServiceProvider
				// waits on this field specifically for the ServiceProvider to go
				// Ready, and otherwise falls back to the 5-minute default.
				WaitOpts: []wait.Option{wait.WithTimeout(15 * time.Minute)},
			},
		},
		Extensions: []extensions.Extension{
			&fluxcd.FluxCD{},
		},
		// Each kind-based Cluster (platform, onboarding, and later the MCP) only
		// reports status.phase=Ready once cluster-provider-kind has deployed
		// MetalLB into it AND all of its pods report ready, on top of the kind
		// cluster itself becoming reachable. On a host that is also busy
		// building the provider image and/or running several nested kind
		// clusters concurrently, that can comfortably take longer than the
		// klient/wait package's 5-minute default, causing ClustersReady (called
		// from verifyEnvironment during Bootstrap) to fail with "context
		// deadline exceeded" even though nothing is actually stuck — it just
		// needs more time. Give it generous headroom here.
		WaitOpts: []wait.Option{wait.WithTimeout(15 * time.Minute)},
	}
	testenv = env.NewWithConfig(envconf.New().WithNamespace(openmcp.Namespace))
	openmcp.Bootstrap(testenv)
	os.Exit(testenv.Run(m))
}

func mustVersion() string {
	cmd := exec.Command("../../hack/common/get-version.sh")
	version, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(version))
}

func initLogging() {
	klog.InitFlags(nil)
	if err := flag.Set("v", "2"); err != nil {
		panic(err)
	}
	flag.Parse()
}
