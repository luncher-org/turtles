//go:build e2e
// +build e2e

/*
Copyright © 2023 - 2024 SUSE LLC

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

package import_gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher/turtles/test/e2e"
	turtlesframework "github.com/rancher/turtles/test/framework"
	"github.com/rancher/turtles/test/testenv"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Test suite flags.
var (
	flagVals *e2e.FlagValues
)

// Test suite global vars.
var (
	// e2eConfig to be used for this test, read from configPath.
	e2eConfig *clusterctl.E2EConfig

	// clusterctlConfigPath to be used for this test, created by generating a clusterctl local repository
	// with the providers specified in the configPath.
	clusterctlConfigPath string

	// hostName is the host name for the Rancher Manager server.
	hostName string

	ctx = context.Background()

	setupClusterResult    *testenv.SetupTestClusterResult
	bootstrapClusterProxy capiframework.ClusterProxy
	gitAddress            string
)

func init() {
	flagVals = &e2e.FlagValues{}
	e2e.InitFlags(flagVals)
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)

	ctrl.SetLogger(klog.Background())

	RunSpecs(t, "rancher-turtles-e2e-import-gitops")
}

var _ = SynchronizedBeforeSuite(
	func() []byte {
		By(fmt.Sprintf("Loading the e2e test configuration from %q", flagVals.ConfigPath))
		Expect(flagVals.ConfigPath).To(BeAnExistingFile(), "Invalid test suite argument. e2e.config should be an existing file.")
		e2eConfig = e2e.LoadE2EConfig(flagVals.ConfigPath)
		e2e.ValidateE2EConfig(e2eConfig)

		artifactsFolder := e2eConfig.GetVariable(e2e.ArtifactsFolderVar)

		preSetupOutput := testenv.PreManagementClusterSetupHook(&testenv.PreRancherInstallHookInput{})

		By(fmt.Sprintf("Creating a clusterctl config into %q", artifactsFolder))
		clusterctlConfigPath = e2e.CreateClusterctlLocalRepository(ctx, e2eConfig, filepath.Join(artifactsFolder, "repository"))

		setupClusterResult = testenv.SetupTestCluster(ctx, testenv.SetupTestClusterInput{
			E2EConfig:             e2eConfig,
			ClusterctlConfigPath:  clusterctlConfigPath,
			Scheme:                e2e.InitScheme(),
			CustomClusterProvider: preSetupOutput.CustomClusterProvider,
		})

		testenv.RancherDeployIngress(ctx, testenv.RancherDeployIngressInput{
			BootstrapClusterProxy:    setupClusterResult.BootstrapClusterProxy,
			CustomIngress:            e2e.NginxIngress,
			DefaultIngressClassPatch: e2e.IngressClassPatch,
		})

		rancherInput := testenv.DeployRancherInput{
			BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
			RancherPatches:        [][]byte{e2e.RancherSettingPatch},
		}

		rancherHookResult := testenv.DeployRancher(ctx, rancherInput)

		if shortTestOnly() {
			chartMuseumDeployInput := testenv.DeployChartMuseumInput{
				BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
			}

			testenv.DeployChartMuseum(ctx, chartMuseumDeployInput)

			rtInput := testenv.DeployRancherTurtlesInput{
				BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
				CAPIProvidersYAML:     e2e.CapiProviders,
				Image:                 "ghcr.io/rancher/turtles-e2e",
				Tag:                   e2eConfig.GetVariable(e2e.TurtlesVersionVar),
				AdditionalValues:      map[string]string{},
			}

			rtInput.AdditionalValues["rancherTurtles.features.addon-provider-fleet.enabled"] = "true"
			rtInput.AdditionalValues["rancherTurtles.features.managementv3-cluster.enabled"] = "false" // disable the default management.cattle.io/v3 controller

			testenv.DeployRancherTurtles(ctx, rtInput)

			By("Waiting for CAAPF deployment to be available")
			capiframework.WaitForDeploymentsAvailable(ctx, capiframework.WaitForDeploymentsAvailableInput{
				Getter: setupClusterResult.BootstrapClusterProxy.GetClient(),
				Deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
					Name:      "caapf-controller-manager",
					Namespace: e2e.RancherTurtlesNamespace,
				}},
			}, e2eConfig.GetIntervals(setupClusterResult.BootstrapClusterProxy.GetName(), "wait-controllers")...)

			By("Setting the CAAPF config to use hostNetwork")
			Expect(turtlesframework.Apply(ctx, setupClusterResult.BootstrapClusterProxy, e2e.AddonProviderFleetHostNetworkPatch)).To(Succeed())
		} else {
			rtInput := testenv.DeployRancherTurtlesInput{
				BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
				CAPIProvidersYAML:     e2e.CapiProviders,
				Image:                 "ghcr.io/rancher/turtles-e2e",
				Tag:                   e2eConfig.GetVariable(e2e.TurtlesVersionVar),
				AdditionalValues: map[string]string{
					"rancherTurtles.features.managementv3-cluster.enabled": "false", // disable the default management.cattle.io/v3 controller
				},
			}

			testenv.DeployRancherTurtles(ctx, rtInput)
		}

		if !shortTestOnly() && !localTestOnly() {
			By("Running full tests, deploying additional infrastructure providers")
			awsCreds := e2eConfig.GetVariable(e2e.CapaEncodedCredentialsVar)
			gcpCreds := e2eConfig.GetVariable(e2e.CapgEncodedCredentialsVar)
			Expect(awsCreds).ToNot(BeEmpty(), "AWS creds required for full test")
			Expect(gcpCreds).ToNot(BeEmpty(), "GCP creds required for full test")

			testenv.CAPIOperatorDeployProvider(ctx, testenv.CAPIOperatorDeployProviderInput{
				BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
				CAPIProvidersSecretsYAML: [][]byte{
					e2e.AWSProviderSecret,
					e2e.AzureIdentitySecret,
					e2e.GCPProviderSecret,
				},
				CAPIProvidersYAML: e2e.FullProviders,
				TemplateData: map[string]string{
					"AWSEncodedCredentials": e2eConfig.GetVariable(e2e.CapaEncodedCredentialsVar),
					"GCPEncodedCredentials": gcpCreds,
				},
				WaitForDeployments: []testenv.NamespaceName{
					{
						Name:      "capa-controller-manager",
						Namespace: "capa-system",
					},
					{
						Name:      "capz-controller-manager",
						Namespace: "capz-system",
					},
					{
						Name:      "capg-controller-manager",
						Namespace: "capg-system",
					},
				},
			})
		} else if Label(e2e.LocalTestLabel).MatchesLabelFilter(GinkgoLabelFilter()) {
			By("Running local vSphere tests, deploying vSphere infrastructure provider")

			testenv.CAPIOperatorDeployProvider(ctx, testenv.CAPIOperatorDeployProviderInput{
				BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
				CAPIProvidersSecretsYAML: [][]byte{
					e2e.VSphereProviderSecret,
				},
				CAPIProvidersYAML: e2e.CapvProvider,
				WaitForDeployments: []testenv.NamespaceName{
					{
						Name:      "capv-controller-manager",
						Namespace: "capv-system",
					},
				},
			})
		}

		giteaInput := testenv.DeployGiteaInput{
			BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
			ValuesFilePath:        "../../data/gitea/values.yaml",
			Values: map[string]string{
				"gitea.admin.username": e2eConfig.GetVariable(e2e.GiteaUserNameVar),
				"gitea.admin.password": e2eConfig.GetVariable(e2e.GiteaUserPasswordVar),
			},
			CustomIngressConfig: e2e.GiteaIngress,
		}

		giteaResult := testenv.DeployGitea(ctx, giteaInput)

		// encode the e2e config into the byte array.
		var configBuf bytes.Buffer
		enc := gob.NewEncoder(&configBuf)
		Expect(enc.Encode(e2eConfig)).To(Succeed())
		configStr := base64.StdEncoding.EncodeToString(configBuf.Bytes())

		return []byte(
			strings.Join([]string{
				setupClusterResult.ClusterName,
				setupClusterResult.KubeconfigPath,
				giteaResult.GitAddress,
				configStr,
				rancherHookResult.HostName,
			}, ","),
		)
	},
	func(sharedData []byte) {
		parts := strings.Split(string(sharedData), ",")
		Expect(parts).To(HaveLen(5))

		clusterName := parts[0]
		kubeconfigPath := parts[1]
		gitAddress = parts[2]

		configBytes, err := base64.StdEncoding.DecodeString(parts[3])
		Expect(err).NotTo(HaveOccurred())
		buf := bytes.NewBuffer(configBytes)
		dec := gob.NewDecoder(buf)
		Expect(dec.Decode(&e2eConfig)).To(Succeed())

		bootstrapClusterProxy = capiframework.NewClusterProxy(string(clusterName), string(kubeconfigPath), e2e.InitScheme(), capiframework.WithMachineLogCollector(capiframework.DockerLogCollector{}))
		Expect(bootstrapClusterProxy).ToNot(BeNil(), "cluster proxy should not be nil")

		hostName = parts[4]
	},
)

var _ = SynchronizedAfterSuite(
	func() {
	},
	func() {
		testenv.UninstallGitea(ctx, testenv.UninstallGiteaInput{
			BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
		})

		testenv.UninstallRancherTurtles(ctx, testenv.UninstallRancherTurtlesInput{
			BootstrapClusterProxy: setupClusterResult.BootstrapClusterProxy,
		})

		testenv.CleanupTestCluster(ctx, testenv.CleanupTestClusterInput{
			SetupTestClusterResult: *setupClusterResult,
		})
	},
)

func shortTestOnly() bool {
	return GinkgoLabelFilter() == e2e.ShortTestLabel
}

func localTestOnly() bool {
	return GinkgoLabelFilter() == e2e.LocalTestLabel
}
