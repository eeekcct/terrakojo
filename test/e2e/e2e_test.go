//go:build e2e
// +build e2e

/*
Copyright 2025 eeekcct.

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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/eeekcct/terrakojo/test/utils"
)

// namespace where the project is deployed in
const namespace = "terrakojo-system"

// serviceAccountName created for the project
const serviceAccountName = "terrakojo-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "terrakojo-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "terrakojo-metrics-binding"
const webhookSecret = "test-secret"
const webhookServiceName = "terrakojo-webhook-server"
const mockGitHubServiceName = "mock-github"

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

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("deploying the mock GitHub API")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/mock-github.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy mock GitHub API")

		By("waiting for the mock GitHub API deployment to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/mock-github", "-n", namespace, "--timeout=2m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Mock GitHub API deployment not ready")

		By("configuring the controller manager to use the mock GitHub API")
		apiURL := fmt.Sprintf("http://%s.%s.svc.cluster.local/api/v3/", mockGitHubServiceName, namespace)
		uploadURL := fmt.Sprintf("http://%s.%s.svc.cluster.local/api/uploads/", mockGitHubServiceName, namespace)
		cmd = exec.Command("kubectl", "set", "env", "deployment/terrakojo-controller-manager", "-n", namespace,
			fmt.Sprintf("GITHUB_API_URL=%s", apiURL),
			fmt.Sprintf("GITHUB_UPLOAD_URL=%s", uploadURL),
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to set GitHub API env vars on controller manager")

		By("waiting for the controller-manager rollout after env updates")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/terrakojo-controller-manager", "-n", namespace, "--timeout=2m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Controller manager rollout after env updates failed")

		By("creating the test repository and webhook secret")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/repository.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply repository and secret manifest")

		By("creating the workflow template")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/manifests/workflowtemplate.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply workflow template manifest")

		By("deploying the webhook server")
		cmd = exec.Command("kubectl", "apply", "-k", "test/e2e/manifests/webhook")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy webhook server")

		By("waiting for the webhook server deployment to be ready")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/terrakojo-webhook-server", "-n", namespace, "--timeout=2m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Webhook server deployment not ready")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("cleaning up the curl pod for webhook")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-webhook", "-n", namespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("removing repository resources")
		cmd = exec.Command("kubectl", "delete", "-f", "test/e2e/manifests/repository.yaml", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("waiting for repository deletion before undeploy")
		cmd = exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace)
		if _, err := utils.Run(cmd); err == nil {
			cmd = exec.Command("kubectl", "wait", "--for=delete", "repository/demo-repo", "-n", namespace, "--timeout=3m")
			_, _ = utils.Run(cmd)
		}

		By("removing workflow template")
		cmd = exec.Command("kubectl", "delete", "workflowtemplate", "demo-workflow", "-n", namespace, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("removing webhook resources")
		cmd = exec.Command("kubectl", "delete", "-k", "test/e2e/manifests/webhook", "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found=true")
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
				// Get the name of the controller-manager pod
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

				// Validate the pod's status
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
				"--clusterrole=terrakojo-metrics-reader",
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

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

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
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
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

		It("should update repository status via GitHub webhook push", func() {
			By("waiting for repository labels to be set")
			verifyRepositoryLabels := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={.metadata.labels.terrakojo\\.io/owner}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("octo"))
			}
			Eventually(verifyRepositoryLabels, 2*time.Minute).Should(Succeed())

			By("sending a GitHub push webhook to the webhook server")
			payload := []byte(`{"ref":"refs/heads/main","after":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef","repository":{"name":"demo","full_name":"octo/demo","owner":{"login":"octo"}}}`)
			signature := signWebhookPayload(webhookSecret, payload)
			output, err := sendWebhookPayload(signature, string(payload))
			Expect(err).NotTo(HaveOccurred(), "Failed to send webhook payload")
			Expect(output).To(Equal("200"), "Unexpected webhook response code")

			By("verifying repository status reflects the pushed commit")
			verifyRepositoryStatus := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={.status.defaultBranchCommits[0].sha}")
				statusOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				if statusOutput == "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
					return
				}

				cmd = exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={.status.synced}")
				syncedOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(syncedOutput).To(Equal("true"))
			}
			Eventually(verifyRepositoryStatus, 2*time.Minute).Should(Succeed())

			By("waiting for a workflow to be created")
			var workflowName string
			verifyWorkflowCreated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "workflows", "-n", namespace,
					"-o", "jsonpath={.items[0].metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				workflowName = output
			}
			Eventually(verifyWorkflowCreated, 2*time.Minute).Should(Succeed())

			By("verifying workflow has a check run assigned")
			verifyWorkflowCheckRun := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "workflow", workflowName, "-n", namespace,
					"-o", "jsonpath={.status.checkRunID}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}
			Eventually(verifyWorkflowCheckRun, 2*time.Minute).Should(Succeed())

			By("verifying check run is completed in mock GitHub")
			verifyCheckRunCompleted := func(g Gomega) {
				snapshot, err := fetchMockGitHubCheckRuns()
				g.Expect(err).NotTo(HaveOccurred())
				found := false
				for _, run := range snapshot.CheckRuns {
					if run.Status == "completed" && run.Conclusion == "success" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue())
			}
			Eventually(verifyCheckRunCompleted, 5*time.Minute).Should(Succeed())
		})

		It("should handle pull request opened and merged events", func() {
			By("sending a GitHub pull request opened webhook")
			prOpenedPayload := []byte(`{"action":"opened","number":3,"pull_request":{"number":3,"state":"open","merged":false,"head":{"ref":"feature/auth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},"repository":{"name":"demo","full_name":"octo/demo","owner":{"login":"octo"}}}`)
			signature := signWebhookPayload(webhookSecret, prOpenedPayload)
			output, err := sendWebhookPayloadWithEvent("pull_request", signature, string(prOpenedPayload))
			Expect(err).NotTo(HaveOccurred(), "Failed to send pull request opened webhook")
			Expect(output).To(Equal("200"), "Unexpected webhook response code")

			By("verifying repository status tracks the PR branch")
			verifyPRBranchListed := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={range .status.branchList[*]}{.ref}:{.sha}{\"\\n\"}{end}")
				listOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				lines := utils.GetNonEmptyLines(listOutput)
				found := false
				for _, line := range lines {
					if line == "feature/auth:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue())
			}
			Eventually(verifyPRBranchListed, 2*time.Minute).Should(Succeed())

			By("sending a GitHub pull request merged webhook")
			prMergedPayload := []byte(`{"action":"closed","number":3,"pull_request":{"number":3,"state":"closed","merged":true,"merge_commit_sha":"cccccccccccccccccccccccccccccccccccccccc","head":{"ref":"feature/auth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},"repository":{"name":"demo","full_name":"octo/demo","owner":{"login":"octo"}}}`)
			signature = signWebhookPayload(webhookSecret, prMergedPayload)
			output, err = sendWebhookPayloadWithEvent("pull_request", signature, string(prMergedPayload))
			Expect(err).NotTo(HaveOccurred(), "Failed to send pull request merged webhook")
			Expect(output).To(Equal("200"), "Unexpected webhook response code")

			By("verifying the PR branch is removed from repository status")
			verifyPRBranchRemoved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={range .status.branchList[*]}{.ref}{\"\\n\"}{end}")
				listOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				lines := utils.GetNonEmptyLines(listOutput)
				for _, line := range lines {
					g.Expect(line).NotTo(Equal("feature/auth"))
				}
			}
			Eventually(verifyPRBranchRemoved, 2*time.Minute).Should(Succeed())

			By("verifying the merge commit is queued on the default branch")
			verifyMergeCommitQueued := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", "demo-repo", "-n", namespace,
					"-o", "jsonpath={range .status.defaultBranchCommits[*]}{.sha}{\"\\n\"}{end}")
				commitsOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				lines := utils.GetNonEmptyLines(commitsOutput)
				found := false
				for _, line := range lines {
					if line == "cccccccccccccccccccccccccccccccccccccccc" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue())
			}
			Eventually(verifyMergeCommitQueued, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
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

func signWebhookPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func sendWebhookPayload(signature, payload string) (string, error) {
	return sendWebhookPayloadWithEvent("push", signature, payload)
}

func sendWebhookPayloadWithEvent(event, signature, payload string) (string, error) {
	args := []string{
		"-s",
		"-o",
		"/dev/null",
		"-w",
		"%{http_code}",
		"-X",
		"POST",
		"-H",
		fmt.Sprintf("X-GitHub-Event: %s", event),
		"-H",
		"X-GitHub-Delivery: e2e-1",
		"-H",
		fmt.Sprintf("X-Hub-Signature-256: sha256=%s", signature),
		"-H",
		"Content-Type: application/json",
		"--data-raw",
		payload,
		fmt.Sprintf("http://%s.%s.svc.cluster.local/webhook", webhookServiceName, namespace),
	}

	name := fmt.Sprintf("curl-webhook-%d", time.Now().UnixNano())
	return runCurlPod(name, args)
}

type checkRunSnapshot struct {
	CheckRuns []checkRunRecord `json:"check_runs"`
}

type checkRunRecord struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func fetchMockGitHubCheckRuns() (*checkRunSnapshot, error) {
	args := []string{
		"-s",
		fmt.Sprintf("http://%s.%s.svc.cluster.local/api/v3/_meta/check-runs", mockGitHubServiceName, namespace),
	}
	name := fmt.Sprintf("curl-github-%d", time.Now().UnixNano())
	output, err := runCurlPod(name, args)
	if err != nil {
		return nil, err
	}
	var snapshot checkRunSnapshot
	if err := json.Unmarshal([]byte(output), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func runCurlPod(name string, args []string) (string, error) {
	overrides := map[string]interface{}{
		"spec": map[string]interface{}{
			"restartPolicy": "Never",
			"securityContext": map[string]interface{}{
				"runAsNonRoot": true,
				"runAsUser":    1000,
				"seccompProfile": map[string]interface{}{
					"type": "RuntimeDefault",
				},
			},
			"containers": []map[string]interface{}{
				{
					"name":    "curl",
					"image":   "curlimages/curl:latest",
					"command": []string{"curl"},
					"args":    args,
					"securityContext": map[string]interface{}{
						"readOnlyRootFilesystem":   true,
						"allowPrivilegeEscalation": false,
						"capabilities": map[string]interface{}{
							"drop": []string{"ALL"},
						},
					},
				},
			},
		},
	}

	rawOverrides, err := json.Marshal(overrides)
	if err != nil {
		return "", err
	}

	cmd := exec.Command("kubectl", "run", name, "--restart=Never", "--namespace", namespace,
		"--image=curlimages/curl:latest",
		"--overrides", string(rawOverrides))
	if _, err := utils.Run(cmd); err != nil {
		return "", err
	}

	verifyCurlUp := func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pods", name,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
	}
	Eventually(verifyCurlUp, 2*time.Minute).Should(Succeed())

	cmd = exec.Command("kubectl", "logs", name, "-n", namespace)
	output, err := utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "pod", name, "-n", namespace)
	_, _ = utils.Run(cmd)

	return output, err
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
