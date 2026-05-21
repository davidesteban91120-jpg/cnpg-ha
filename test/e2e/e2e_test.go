//go:build e2e
// +build e2e

/*
Copyright 2026.

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/davidesteban/cnpg-ha/test/utils"
)

// namespace where the project is deployed in
const namespace = "cnpg-ha-system"

// serviceAccountName created for the project
const serviceAccountName = "cnpg-ha-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "cnpg-ha-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "cnpg-ha-metrics-binding"

// haSmokeNamespace hosts the smoke-test HACluster CR and its fake CNPG Cluster fixture.
const haSmokeNamespace = "ha-smoke"

// cnpgClusterCRDFixture is the path (project-root relative — utils.Run cds
// into the project root) of the minimal CNPG Cluster CRD used as a test
// double. cnpg-ha reads/patches the Cluster CR as unstructured JSON, so
// this fixture is enough to exercise the real reconcile path without
// installing the full CloudNativePG operator.
const cnpgClusterCRDFixture = "test/crd/postgresql.cnpg.io_clusters.yaml"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("installing the CNPG Cluster CRD fixture (test double)")
		cmd = exec.Command("kubectl", "apply", "-f", cnpgClusterCRDFixture)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install the CNPG Cluster CRD fixture")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("removing the HA smoke namespace if any")
		cmd = exec.Command("kubectl", "delete", "ns", haSmokeNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("uninstalling the CNPG Cluster CRD fixture")
		cmd = exec.Command("kubectl", "delete", "-f", cnpgClusterCRDFixture, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=cnpg-ha-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	// HACluster lifecycle smoke: apply a real HACluster CR against a fake
	// CNPG Cluster fixture, verify that the controller observes both the
	// primary (locally reachable, ready) and one replica (unreachable —
	// the kubeconfig points nowhere on purpose), and that status flows.
	//
	// This stays in the single-cluster runner: no second KinD, no real
	// CNPG. End-to-end multi-site failover lives under hack/e2e/ and is
	// not run from CI.
	Context("HACluster lifecycle", func() {
		const (
			haName           = "smoke"
			cnpgClusterName  = "pg-prod"
			replicaSiteName  = "site-b"
			replicaSecret    = "site-b-kubeconfig"
			replicaSecretKey = "kubeconfig"
		)

		// Self-contained kubeconfig that DNS-resolves but routes to a
		// closed port. The remote-client cache parses it without errors;
		// the actual Get on the remote API server fails with "connection
		// refused", so site-b is reported as unreachable. This exercises
		// the full reconcile path including remote-client construction
		// and graceful error surfacing without needing a real second
		// cluster.
		const placeholderKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: placeholder
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: placeholder
  context:
    cluster: placeholder
    user: placeholder
current-context: placeholder
users:
- name: placeholder
  user:
    token: placeholder
`

		BeforeAll(func() {
			By("creating the HA smoke namespace")
			cmd := exec.Command("kubectl", "create", "ns", haSmokeNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create HA smoke namespace")

			By("applying a fake CNPG Cluster CR observed as healthy primary")
			// The fixture CRD has no status subresource, so status is set
			// inline on create. cnpg-ha reads status.phase / readyInstances
			// / timelineID through parseCluster — see internal/health.
			fakeCNPG := fmt.Sprintf(`apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  instances: 1
status:
  phase: "Cluster in healthy state"
  readyInstances: 1
  timelineID: 1
`, cnpgClusterName, haSmokeNamespace)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fakeCNPG)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply fake CNPG Cluster")

			By("creating a placeholder kubeconfig Secret for the replica site")
			replicaSecretYAML := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  %s: |
%s`, replicaSecret, haSmokeNamespace, replicaSecretKey, indent(placeholderKubeconfig, "    "))
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(replicaSecretYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create replica kubeconfig Secret")

			By("applying the HACluster CR (Manual mode, one replica)")
			haYAML := fmt.Sprintf(`apiVersion: ha.cnpg.io/v1alpha1
kind: HACluster
metadata:
  name: %s
  namespace: %s
spec:
  primary:
    name: site-a
    clusterRef:
      name: %s
      namespace: %s
  replicas:
    - name: %s
      kubeconfigSecretRef:
        name: %s
        key: %s
      clusterRef:
        name: %s
        namespace: %s
  failover:
    mode: Manual
    healthCheckIntervalSeconds: 10
    failureThreshold: 3
`,
				haName, haSmokeNamespace,
				cnpgClusterName, haSmokeNamespace,
				replicaSiteName, replicaSecret, replicaSecretKey,
				cnpgClusterName, haSmokeNamespace)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(haYAML)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply HACluster CR")
		})

		It("populates status.sites with primary and replica", func() {
			By("waiting for both sites to be reported in status.sites")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", "jsonpath={.status.sites[*].name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "failed to read status.sites")
				g.Expect(out).To(ContainSubstring("site-a"), "primary site missing")
				g.Expect(out).To(ContainSubstring(replicaSiteName), "replica site missing")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("sets status.observedGeneration", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", "jsonpath={.status.observedGeneration}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "observedGeneration not set yet")
				g.Expect(out).NotTo(Equal("0"), "observedGeneration should be >= 1 after a reconcile")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("reports the primary site as reachable and ready", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", `jsonpath={.status.sites[?(@.name=="site-a")].ready}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("true"), "primary site is not reported ready (CNPG fixture not observed)")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("reports the replica site as unreachable (placeholder kubeconfig)", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", fmt.Sprintf(`jsonpath={.status.sites[?(@.name=="%s")].reachable}`, replicaSiteName))
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("false"), "replica site should be unreachable with the placeholder kubeconfig")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("accepts the observed primary as status.currentPrimarySite", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", "jsonpath={.status.currentPrimarySite}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("site-a"), "currentPrimarySite should be set to the observed primary")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})

		It("sets at least one Condition on the HACluster", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "hacluster", haName,
					"-n", haSmokeNamespace,
					"-o", "jsonpath={.status.conditions[*].type}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "no condition set on HACluster")
			}, 90*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})

// indent prefixes every line of s with prefix. Used to embed a multi-line
// kubeconfig literal under a YAML stringData field without breaking the
// surrounding indentation.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
