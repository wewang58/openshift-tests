package util

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	authorizationapi "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kapiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubernetes/pkg/quota/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	appsv1 "github.com/openshift/api/apps/v1"
	buildv1 "github.com/openshift/api/build/v1"
	configv1 "github.com/openshift/api/config/v1"
	imagev1 "github.com/openshift/api/image/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	appsv1clienttyped "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	buildv1clienttyped "github.com/openshift/client-go/build/clientset/versioned/typed/build/v1"
	imagev1typedclient "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	"github.com/openshift/library-go/pkg/apps/appsutil"
	"github.com/openshift/library-go/pkg/build/naming"
	"github.com/openshift/library-go/pkg/git"
	"github.com/openshift/library-go/pkg/image/imageutil"

	"github.com/openshift/origin/test/extended/testdata"
)

// WaitForInternalRegistryHostname waits for the internal registry hostname to be made available to the cluster.
func WaitForInternalRegistryHostname(oc *CLI) (string, error) {
	e2e.Logf("Waiting up to 2 minutes for the internal registry hostname to be published")
	var registryHostname string
	foundOCMLogs := false
	isOCMProgressing := true
	podLogs := map[string]string{}
	err := wait.Poll(2*time.Second, 2*time.Minute, func() (bool, error) {
		imageConfig, err := oc.AsAdmin().AdminConfigClient().ConfigV1().Images().Get("cluster", metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				e2e.Logf("Image config object not found")
				return false, nil
			}
			e2e.Logf("Error accessing image config object: %#v", err)
			return false, err
		}
		if imageConfig == nil {
			e2e.Logf("Image config object nil")
			return false, nil
		}
		registryHostname = imageConfig.Status.InternalRegistryHostname
		if len(registryHostname) == 0 {
			e2e.Logf("Internal Registry Hostname is not set in image config object")
			return false, nil
		}

		// verify that the OCM config's internal registry hostname matches
		// the image config's internal registry hostname
		ocm, err := oc.AdminOperatorClient().OperatorV1().OpenShiftControllerManagers().Get("cluster", metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		observedConfig := map[string]interface{}{}
		err = json.Unmarshal(ocm.Spec.ObservedConfig.Raw, &observedConfig)
		if err != nil {
			return false, nil
		}
		internalRegistryHostnamePath := []string{"dockerPullSecret", "internalRegistryHostname"}
		currentRegistryHostname, _, err := unstructured.NestedString(observedConfig, internalRegistryHostnamePath...)
		if err != nil {
			e2e.Logf("error procesing observed config %#v", err)
			return false, nil
		}
		if currentRegistryHostname != registryHostname {
			e2e.Logf("OCM observed config hostname %s does not match image config hostname %s", currentRegistryHostname, registryHostname)
			return false, nil
		}
		// check pod logs for messages around image config's internal registry hostname has been observed and
		// and that the build controller was started after that observation
		pods, err := oc.AdminKubeClient().CoreV1().Pods("openshift-controller-manager").List(metav1.ListOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		for _, pod := range pods.Items {
			req := oc.AdminKubeClient().CoreV1().Pods("openshift-controller-manager").GetLogs(pod.Name, &corev1.PodLogOptions{})
			readCloser, err := req.Stream()
			if err == nil {
				b, err := ioutil.ReadAll(readCloser)
				if err == nil {
					podLog := string(b)
					podLogs[pod.Name] = podLog
					scanner := bufio.NewScanner(strings.NewReader(podLog))
					firstLog := false
					for scanner.Scan() {
						line := scanner.Text()
						if strings.Contains(line, "docker_registry_service.go") && strings.Contains(line, registryHostname) {
							firstLog = true
							continue
						}
						if firstLog && strings.Contains(line, "build_controller.go") && strings.Contains(line, "Starting build controller") {
							e2e.Logf("the OCM pod logs indicate the build controller was started after the internal registry hostname has been set in the OCM config")
							foundOCMLogs = true
							break
						}
					}
				}
			} else {
				e2e.Logf("error getting pod logs: %#v", err)
			}
		}
		if !foundOCMLogs {
			e2e.Logf("did not find the sequence in the OCM pod logs around the build controller getting started after the internal registry hostname has been set in the OCM config")
			return false, nil
		}

		if !isOCMProgressing {
			return true, nil
		}
		// now cycle through the OCM operator conditions and make sure the Progressing condition is done
		for _, condition := range ocm.Status.Conditions {
			if condition.Type != operatorv1.OperatorStatusTypeProgressing {
				continue
			}
			if condition.Status != operatorv1.ConditionFalse {
				e2e.Logf("OCM rollout still progressing or in error: %v", condition.Status)
				return false, nil
			}
			e2e.Logf("OCM rollout progressing status reports complete")
			isOCMProgressing = true
			return true, nil
		}
		e2e.Logf("OCM operator progressing condition not present yet")
		return false, nil
	})

	if !foundOCMLogs {
		e2e.Logf("dumping OCM pod logs since we never found the internal registry hostname and start build controller sequence")
		for podName, podLog := range podLogs {
			e2e.Logf("pod %s logs:\n%s", podName, podLog)
		}
	}
	if err == wait.ErrWaitTimeout {
		return "", fmt.Errorf("Timed out waiting for internal registry hostname to be published")
	}
	if err != nil {
		return "", err
	}
	return registryHostname, nil
}

// WaitForOpenShiftNamespaceImageStreams waits for the standard set of imagestreams to be imported
func WaitForOpenShiftNamespaceImageStreams(oc *CLI) error {
	// First wait for the internal registry hostname to be published
	registryHostname, err := WaitForInternalRegistryHostname(oc)
	if err != nil {
		return err
	}
	langs := []string{"ruby", "nodejs", "perl", "php", "python", "mysql", "postgresql", "mongodb", "jenkins"}
	scan := func() bool {
		// check the samples operator to see about imagestream import status
		samplesOperatorConfig, err := oc.AdminConfigClient().ConfigV1().ClusterOperators().Get("openshift-samples", metav1.GetOptions{})
		if err != nil {
			e2e.Logf("Samples Operator ClusterOperator Error: %#v", err)
			return false
		}
		for _, condition := range samplesOperatorConfig.Status.Conditions {
			switch {
			case condition.Type == configv1.OperatorDegraded && condition.Status == configv1.ConditionTrue:
				// if degraded, bail ... unexpected results can ensue
				e2e.Logf("SamplesOperator degraded!!!")
				return false
			case condition.Type == configv1.OperatorProgressing:
				if condition.Status == configv1.ConditionTrue {
					// updates still in progress ... not "ready"
					e2e.Logf("SamplesOperator still in progress")
					return false
				}
				// if reason field set, that means an image import error occurred
				if len(condition.Reason) > 0 {
					e2e.Logf("SamplesOperator detected error during imagestream import: %s with details %s", condition.Reason, condition.Message)
					return false
				}
			case condition.Type == configv1.OperatorAvailable && condition.Status == configv1.ConditionFalse:
				e2e.Logf("SamplesOperator not available")
				return false
			default:
				e2e.Logf("SamplesOperator at steady state")
			}
		}
		for _, lang := range langs {
			e2e.Logf("Checking language %v \n", lang)
			is, err := oc.ImageClient().ImageV1().ImageStreams("openshift").Get(lang, metav1.GetOptions{})
			if err != nil {
				e2e.Logf("ImageStream Error: %#v \n", err)
				return false
			}
			if !strings.HasPrefix(is.Status.DockerImageRepository, registryHostname) {
				e2e.Logf("ImageStream repository %s does not match expected host %s \n", is.Status.DockerImageRepository, registryHostname)
				return false
			}
			for _, tag := range is.Spec.Tags {
				e2e.Logf("Checking tag %v \n", tag)
				if _, found := imageutil.StatusHasTag(is, tag.Name); !found {
					e2e.Logf("Tag Error: %#v \n", tag)
					return false
				}
			}
		}
		return true
	}

	// with the move to ocp/rhel as the default for the samples in 4.0, there are alot more imagestreams;
	// if by some chance this path runs very soon after the cluster has come up, the original time out would
	// not be sufficient;
	// so we've bumped what was 30 seconds to 2 min 30 seconds or 150 seconds (manual perf testing shows typical times of
	// 1 to 2 minutes, assuming registry.access.redhat.com / registry.redhat.io are behaving ... they
	// have proven less reliable that docker.io)
	// we've also determined that e2e-aws-image-ecosystem can be started before all the operators have completed; while
	// that is getting sorted out, the longer time will help there as well
	e2e.Logf("Scanning openshift ImageStreams \n")
	success := false
	wait.Poll(10*time.Second, 150*time.Second, func() (bool, error) {
		success = scan()
		return success, nil
	})
	if success {
		e2e.Logf("Success! \n")
		return nil
	}
	DumpImageStreams(oc)
	DumpSampleOperator(oc)
	return fmt.Errorf("Failed to import expected imagestreams")
}

//DumpImageStreams will dump both the openshift namespace and local namespace imagestreams
// as part of debugging when the language imagestreams in the openshift namespace seem to disappear
func DumpImageStreams(oc *CLI) {
	out, err := oc.AsAdmin().Run("get").Args("is", "-n", "openshift", "-o", "yaml", "--config", KubeConfigPath()).Output()
	if err == nil {
		e2e.Logf("\n  imagestreams in openshift namespace: \n%s\n", out)
	} else {
		e2e.Logf("\n  error on getting imagestreams in openshift namespace: %+v\n%#v\n", err, out)
	}
	out, err = oc.AsAdmin().Run("get").Args("is", "-o", "yaml").Output()
	if err == nil {
		e2e.Logf("\n  imagestreams in dynamic test namespace: \n%s\n", out)
	} else {
		e2e.Logf("\n  error on getting imagestreams in dynamic test namespace: %+v\n%#v\n", err, out)
	}
	ids, err := ListImages()
	if err != nil {
		e2e.Logf("\n  got error on container images %+v\n", err)
	} else {
		for _, id := range ids {
			e2e.Logf(" found local image %s\n", id)
		}
	}
}

func DumpSampleOperator(oc *CLI) {
	out, err := oc.AsAdmin().Run("get").Args("configs.samples.operator.openshift.io", "cluster", "-o", "yaml", "--config", KubeConfigPath()).Output()
	if err == nil {
		e2e.Logf("\n  samples operator CR: \n%s\n", out)
	} else {
		e2e.Logf("\n  error on getting samples operator CR: %+v\n%#v\n", err, out)
	}
	DumpPodLogsStartingWithInNamespace("cluster-samples-operator", "openshift-cluster-samples-operator", oc)

}

// DumpBuildLogs will dump the latest build logs for a BuildConfig for debug purposes
func DumpBuildLogs(bc string, oc *CLI) {
	buildOutput, err := oc.AsAdmin().Run("logs").Args("-f", "bc/"+bc, "--timestamps").Output()
	if err == nil {
		e2e.Logf("\n\n  build logs : %s\n\n", buildOutput)
	} else {
		e2e.Logf("\n\n  got error on build logs %+v\n\n", err)
	}

	// if we suspect that we are filling up the registry file system, call ExamineDiskUsage / ExaminePodDiskUsage
	// also see if manipulations of the quota around /mnt/openshift-xfs-vol-dir exist in the extended test set up scripts
	ExamineDiskUsage()
	ExaminePodDiskUsage(oc)
}

// DumpBuilds will dump the yaml for every build in the test namespace; remember, pipeline builds
// don't have build pods so a generic framework dump won't cat our pipeline builds objs in openshift
func DumpBuilds(oc *CLI) {
	buildOutput, err := oc.AsAdmin().Run("get").Args("builds", "-o", "yaml").Output()
	if err == nil {
		e2e.Logf("\n\n builds yaml:\n%s\n\n", buildOutput)
	} else {
		e2e.Logf("\n\n got error on build yaml dump: %#v\n\n", err)
	}
}

func GetDeploymentConfigPods(oc *CLI, dcName string, version int64) (*kapiv1.PodList, error) {
	return oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).List(metav1.ListOptions{LabelSelector: ParseLabelsOrDie(fmt.Sprintf("%s=%s-%d",
		appsv1.DeployerPodForDeploymentLabel, dcName, version)).String()})
}

func GetApplicationPods(oc *CLI, dcName string) (*kapiv1.PodList, error) {
	return oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).List(metav1.ListOptions{LabelSelector: ParseLabelsOrDie(fmt.Sprintf("deploymentconfig=%s", dcName)).String()})
}

func GetStatefulSetPods(oc *CLI, setName string) (*kapiv1.PodList, error) {
	return oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).List(metav1.ListOptions{LabelSelector: ParseLabelsOrDie(fmt.Sprintf("name=%s", setName)).String()})
}

// DumpDeploymentLogs will dump the latest deployment logs for a DeploymentConfig for debug purposes
func DumpDeploymentLogs(dcName string, version int64, oc *CLI) {
	e2e.Logf("Dumping deployment logs for deploymentconfig %q\n", dcName)

	pods, err := GetDeploymentConfigPods(oc, dcName, version)
	if err != nil {
		e2e.Logf("Unable to retrieve pods for deploymentconfig %q: %v\n", dcName, err)
		return
	}

	DumpPodLogs(pods.Items, oc)
}

// DumpApplicationPodLogs will dump the latest application logs for a DeploymentConfig for debug purposes
func DumpApplicationPodLogs(dcName string, oc *CLI) {
	e2e.Logf("Dumping application logs for deploymentconfig %q\n", dcName)

	pods, err := GetApplicationPods(oc, dcName)
	if err != nil {
		e2e.Logf("Unable to retrieve pods for deploymentconfig %q: %v\n", dcName, err)
		return
	}

	DumpPodLogs(pods.Items, oc)
}

// DumpPodStates dumps the state of all pods in the CLI's current namespace.
func DumpPodStates(oc *CLI) {
	e2e.Logf("Dumping pod state for namespace %s", oc.Namespace())
	out, err := oc.AsAdmin().Run("get").Args("pods", "-o", "yaml").Output()
	if err != nil {
		e2e.Logf("Error dumping pod states: %v", err)
		return
	}
	e2e.Logf(out)
}

// DumpPodStatesInNamespace dumps the state of all pods in the provided namespace.
func DumpPodStatesInNamespace(namespace string, oc *CLI) {
	e2e.Logf("Dumping pod state for namespace %s", namespace)
	out, err := oc.AsAdmin().Run("get").Args("pods", "-n", namespace, "-o", "yaml").Output()
	if err != nil {
		e2e.Logf("Error dumping pod states: %v", err)
		return
	}
	e2e.Logf(out)
}

// DumpPodLogsStartingWith will dump any pod starting with the name prefix provided
func DumpPodLogsStartingWith(prefix string, oc *CLI) {
	podsToDump := []kapiv1.Pod{}
	podList, err := oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).List(metav1.ListOptions{})
	if err != nil {
		e2e.Logf("Error listing pods: %v", err)
		return
	}
	for _, pod := range podList.Items {
		if strings.HasPrefix(pod.Name, prefix) {
			podsToDump = append(podsToDump, pod)
		}
	}
	if len(podsToDump) > 0 {
		DumpPodLogs(podsToDump, oc)
	}
}

// DumpPodLogsStartingWith will dump any pod starting with the name prefix provided
func DumpPodLogsStartingWithInNamespace(prefix, namespace string, oc *CLI) {
	podsToDump := []kapiv1.Pod{}
	podList, err := oc.AdminKubeClient().CoreV1().Pods(namespace).List(metav1.ListOptions{})
	if err != nil {
		e2e.Logf("Error listing pods: %v", err)
		return
	}
	for _, pod := range podList.Items {
		if strings.HasPrefix(pod.Name, prefix) {
			podsToDump = append(podsToDump, pod)
		}
	}
	if len(podsToDump) > 0 {
		DumpPodLogs(podsToDump, oc)
	}
}

func DumpPodLogs(pods []kapiv1.Pod, oc *CLI) {
	for _, pod := range pods {
		descOutput, err := oc.AsAdmin().Run("describe").WithoutNamespace().Args("pod/"+pod.Name, "-n", pod.Namespace).Output()
		if err == nil {
			e2e.Logf("Describing pod %q\n%s\n\n", pod.Name, descOutput)
		} else {
			e2e.Logf("Error retrieving description for pod %q: %v\n\n", pod.Name, err)
		}

		dumpContainer := func(container *kapiv1.Container) {
			depOutput, err := oc.AsAdmin().Run("logs").WithoutNamespace().Args("pod/"+pod.Name, "-c", container.Name, "-n", pod.Namespace).Output()
			if err == nil {
				e2e.Logf("Log for pod %q/%q\n---->\n%s\n<----end of log for %[1]q/%[2]q\n", pod.Name, container.Name, depOutput)
			} else {
				e2e.Logf("Error retrieving logs for pod %q/%q: %v\n\n", pod.Name, container.Name, err)
			}
		}

		for _, c := range pod.Spec.InitContainers {
			dumpContainer(&c)
		}
		for _, c := range pod.Spec.Containers {
			dumpContainer(&c)
		}
	}
}

// DumpPodsCommand runs the provided command in every pod identified by selector in the provided namespace.
func DumpPodsCommand(c kubernetes.Interface, ns string, selector labels.Selector, cmd string) {
	podList, err := c.CoreV1().Pods(ns).List(metav1.ListOptions{LabelSelector: selector.String()})
	o.Expect(err).NotTo(o.HaveOccurred())

	values := make(map[string]string)
	for _, pod := range podList.Items {
		stdout, err := e2e.RunHostCmdWithRetries(pod.Namespace, pod.Name, cmd, e2e.StatefulSetPoll, e2e.StatefulPodTimeout)
		o.Expect(err).NotTo(o.HaveOccurred())
		values[pod.Name] = stdout
	}
	for name, stdout := range values {
		stdout = strings.TrimSuffix(stdout, "\n")
		e2e.Logf(name + ": " + strings.Join(strings.Split(stdout, "\n"), fmt.Sprintf("\n%s: ", name)))
	}
}

// DumpConfigMapStates dumps the state of all ConfigMaps in the CLI's current namespace.
func DumpConfigMapStates(oc *CLI) {
	e2e.Logf("Dumping configMap state for namespace %s", oc.Namespace())
	out, err := oc.AsAdmin().Run("get").Args("configmaps", "-o", "yaml").Output()
	if err != nil {
		e2e.Logf("Error dumping configMap states: %v", err)
		return
	}
	e2e.Logf(out)
}

// GetMasterThreadDump will get a golang thread stack dump
func GetMasterThreadDump(oc *CLI) {
	out, err := oc.AsAdmin().Run("get").Args("--raw", "/debug/pprof/goroutine?debug=2").Output()
	if err == nil {
		e2e.Logf("\n\n Master thread stack dump:\n\n%s\n\n", string(out))
		return
	}
	e2e.Logf("\n\n got error on oc get --raw /debug/pprof/goroutine?godebug=2: %v\n\n", err)
}

func PreTestDump() {
	// dump any state we want to know prior to running tests
}

// ExamineDiskUsage will dump df output on the testing system; leveraging this as part of diagnosing
// the registry's disk filling up during external tests on jenkins
func ExamineDiskUsage() {
	// disabling this for now, easier to do it here than everywhere that's calling it.
	return
	/*
				out, err := exec.Command("/bin/df", "-m").Output()
				if err == nil {
					e2e.Logf("\n\n df -m output: %s\n\n", string(out))
				} else {
					e2e.Logf("\n\n got error on df %v\n\n", err)
				}
		                DumpDockerInfo()
	*/
}

// ExaminePodDiskUsage will dump df/du output on registry pod; leveraging this as part of diagnosing
// the registry's disk filling up during external tests on jenkins
func ExaminePodDiskUsage(oc *CLI) {
	// disabling this for now, easier to do it here than everywhere that's calling it.
	return
	/*
		out, err := oc.Run("get").Args("pods", "-o", "json", "-n", "default", "--config", KubeConfigPath()).Output()
		var podName string
		if err == nil {
			b := []byte(out)
			var list kapiv1.PodList
			err = json.Unmarshal(b, &list)
			if err == nil {
				for _, pod := range list.Items {
					e2e.Logf("\n\n looking at pod %s \n\n", pod.ObjectMeta.Name)
					if strings.Contains(pod.ObjectMeta.Name, "docker-registry-") && !strings.Contains(pod.ObjectMeta.Name, "deploy") {
						podName = pod.ObjectMeta.Name
						break
					}
				}
			} else {
				e2e.Logf("\n\n got json unmarshal err: %v\n\n", err)
			}
		} else {
			e2e.Logf("\n\n  got error on get pods: %v\n\n", err)
		}
		if len(podName) == 0 {
			e2e.Logf("Unable to determine registry pod name, so we can't examine its disk usage.")
			return
		}

		out, err = oc.Run("exec").Args("-n", "default", podName, "df", "--config", KubeConfigPath()).Output()
		if err == nil {
			e2e.Logf("\n\n df from registry pod: \n%s\n\n", out)
		} else {
			e2e.Logf("\n\n got error on reg pod df: %v\n", err)
		}
		out, err = oc.Run("exec").Args("-n", "default", podName, "du", "/registry", "--config", KubeConfigPath()).Output()
		if err == nil {
			e2e.Logf("\n\n du from registry pod: \n%s\n\n", out)
		} else {
			e2e.Logf("\n\n got error on reg pod du: %v\n", err)
		}
	*/
}

// VarSubOnFile reads in srcFile, finds instances of ${key} from the map
// and replaces them with their associated values.
func VarSubOnFile(srcFile string, destFile string, vars map[string]string) error {
	srcData, err := ioutil.ReadFile(srcFile)
	if err == nil {
		srcString := string(srcData)
		for k, v := range vars {
			k = "${" + k + "}"
			srcString = strings.Replace(srcString, k, v, -1) // -1 means unlimited replacements
		}
		err = ioutil.WriteFile(destFile, []byte(srcString), 0644)
	}
	return err
}

// StartBuild executes OC start-build with the specified arguments. StdOut and StdErr from the process
// are returned as separate strings.
func StartBuild(oc *CLI, args ...string) (stdout, stderr string, err error) {
	stdout, stderr, err = oc.Run("start-build").Args(args...).Outputs()
	e2e.Logf("\n\nstart-build output with args %v:\nError>%v\nStdOut>\n%s\nStdErr>\n%s\n\n", args, err, stdout, stderr)
	return stdout, stderr, err
}

var buildPathPattern = regexp.MustCompile(`^build\.build\.openshift\.io/([\w\-\._]+)$`)

type LogDumperFunc func(oc *CLI, br *BuildResult) (string, error)

func NewBuildResult(oc *CLI, build *buildv1.Build) *BuildResult {
	return &BuildResult{
		Oc:        oc,
		BuildName: build.Name,
		BuildPath: "builds/" + build.Name,
	}
}

type BuildResult struct {
	// BuildPath is a resource qualified name (e.g. "build/test-1").
	BuildPath string
	// BuildName is the non-resource qualified name.
	BuildName string
	// StartBuildStdErr is the StdErr output generated by oc start-build.
	StartBuildStdErr string
	// StartBuildStdOut is the StdOut output generated by oc start-build.
	StartBuildStdOut string
	// StartBuildErr is the error, if any, returned by the direct invocation of the start-build command.
	StartBuildErr error
	// The buildconfig which generated this build.
	BuildConfigName string
	// Build is the resource created. May be nil if there was a timeout.
	Build *buildv1.Build
	// BuildAttempt represents that a Build resource was created.
	// false indicates a severe error unrelated to Build success or failure.
	BuildAttempt bool
	// BuildSuccess is true if the build was finshed successfully.
	BuildSuccess bool
	// BuildFailure is true if the build was finished with an error.
	BuildFailure bool
	// BuildCancelled is true if the build was canceled.
	BuildCancelled bool
	// BuildTimeout is true if there was a timeout waiting for the build to finish.
	BuildTimeout bool
	// Alternate log dumper function. If set, this is called instead of 'oc logs'
	LogDumper LogDumperFunc
	// The openshift client which created this build.
	Oc *CLI
}

// DumpLogs sends logs associated with this BuildResult to the GinkgoWriter.
func (t *BuildResult) DumpLogs() {
	e2e.Logf("\n\n*****************************************\n")
	e2e.Logf("Dumping Build Result: %#v\n", *t)

	if t == nil {
		e2e.Logf("No build result available!\n\n")
		return
	}

	desc, err := t.Oc.Run("describe").Args(t.BuildPath).Output()

	e2e.Logf("\n** Build Description:\n")
	if err != nil {
		e2e.Logf("Error during description retrieval: %+v\n", err)
	} else {
		e2e.Logf("%s\n", desc)
	}

	e2e.Logf("\n** Build Logs:\n")

	buildOuput, err := t.Logs()
	if err != nil {
		e2e.Logf("Error during log retrieval: %+v\n", err)
	} else {
		e2e.Logf("%s\n", buildOuput)
	}

	e2e.Logf("\n\n")

	t.dumpRegistryLogs()

	// if we suspect that we are filling up the registry file system, call ExamineDiskUsage / ExaminePodDiskUsage
	// also see if manipulations of the quota around /mnt/openshift-xfs-vol-dir exist in the extended test set up scripts
	/*
		ExamineDiskUsage()
		ExaminePodDiskUsage(t.oc)
		e2e.Logf( "\n\n")
	*/
}

func (t *BuildResult) dumpRegistryLogs() {
	var buildStarted *time.Time
	oc := t.Oc
	e2e.Logf("\n** Registry Logs:\n")

	if t.Build != nil && !t.Build.CreationTimestamp.IsZero() {
		buildStarted = &t.Build.CreationTimestamp.Time
	} else {
		proj, err := oc.ProjectClient().ProjectV1().Projects().Get(oc.Namespace(), metav1.GetOptions{})
		if err != nil {
			e2e.Logf("Failed to get project %s: %v\n", oc.Namespace(), err)
		} else {
			buildStarted = &proj.CreationTimestamp.Time
		}
	}

	if buildStarted == nil {
		e2e.Logf("Could not determine test' start time\n\n\n")
		return
	}

	since := time.Now().Sub(*buildStarted)

	// Changing the namespace on the derived client still changes it on the original client
	// because the kubeFramework field is only copied by reference. Saving the original namespace
	// here so we can restore it when done with registry logs
	// TODO remove the default/docker-registry log retrieval when we are fully migrated to 4.0 for our test env.
	savedNamespace := t.Oc.Namespace()
	oadm := t.Oc.AsAdmin().SetNamespace("default")
	out, err := oadm.Run("logs").Args("dc/docker-registry", "--since="+since.String()).Output()
	if err != nil {
		e2e.Logf("Error during log retrieval: %+v\n", err)
	} else {
		e2e.Logf("%s\n", out)
	}
	oadm = t.Oc.AsAdmin().SetNamespace("openshift-image-registry")
	out, err = oadm.Run("logs").Args("deployment/image-registry", "--since="+since.String()).Output()
	if err != nil {
		e2e.Logf("Error during log retrieval: %+v\n", err)
	} else {
		e2e.Logf("%s\n", out)
	}
	t.Oc.SetNamespace(savedNamespace)

	e2e.Logf("\n\n")
}

// Logs returns the logs associated with this build.
func (t *BuildResult) Logs() (string, error) {
	if t == nil || t.BuildPath == "" {
		return "", fmt.Errorf("Not enough information to retrieve logs for %#v", *t)
	}

	if t.LogDumper != nil {
		return t.LogDumper(t.Oc, t)
	}

	buildOuput, err := t.Oc.Run("logs").Args("-f", t.BuildPath, "--timestamps").Output()
	if err != nil {
		return "", fmt.Errorf("Error retrieving logs for %#v: %v", *t, err)
	}

	return buildOuput, nil
}

// LogsNoTimestamp returns the logs associated with this build.
func (t *BuildResult) LogsNoTimestamp() (string, error) {
	if t == nil || t.BuildPath == "" {
		return "", fmt.Errorf("Not enough information to retrieve logs for %#v", *t)
	}

	if t.LogDumper != nil {
		return t.LogDumper(t.Oc, t)
	}

	buildOuput, err := t.Oc.Run("logs").Args("-f", t.BuildPath).Output()
	if err != nil {
		return "", fmt.Errorf("Error retrieving logs for %#v: %v", *t, err)
	}

	return buildOuput, nil
}

// Dumps logs and triggers a Ginkgo assertion if the build did NOT succeed.
func (t *BuildResult) AssertSuccess() *BuildResult {
	if !t.BuildSuccess {
		t.DumpLogs()
	}
	o.ExpectWithOffset(1, t.BuildSuccess).To(o.BeTrue())
	return t
}

// Dumps logs and triggers a Ginkgo assertion if the build did NOT have an error (this will not assert on timeouts)
func (t *BuildResult) AssertFailure() *BuildResult {
	if !t.BuildFailure {
		t.DumpLogs()
	}
	o.ExpectWithOffset(1, t.BuildFailure).To(o.BeTrue())
	return t
}

func StartBuildResult(oc *CLI, args ...string) (result *BuildResult, err error) {
	args = append(args, "-o=name") // ensure that the build name is the only thing send to stdout
	stdout, stderr, err := StartBuild(oc, args...)

	// Usually, with -o=name, we only expect the build path.
	// However, the caller may have added --follow which can add
	// content to stdout. So just grab the first line.
	buildPath := strings.TrimSpace(strings.Split(stdout, "\n")[0])

	result = &BuildResult{
		Build:            nil,
		BuildPath:        buildPath,
		StartBuildStdOut: stdout,
		StartBuildStdErr: stderr,
		StartBuildErr:    nil,
		BuildAttempt:     false,
		BuildSuccess:     false,
		BuildFailure:     false,
		BuildCancelled:   false,
		BuildTimeout:     false,
		Oc:               oc,
	}

	// An error here does not necessarily mean we could not run start-build. For example
	// when --wait is specified, start-build returns an error if the build fails. Therefore,
	// we continue to collect build information even if we see an error.
	result.StartBuildErr = err

	matches := buildPathPattern.FindStringSubmatch(buildPath)
	if len(matches) != 2 {
		return result, fmt.Errorf("Build path output did not match expected format 'build/name' : %q", buildPath)
	}

	result.BuildName = matches[1]

	return result, nil
}

// StartBuildAndWait executes OC start-build with the specified arguments on an existing buildconfig.
// Note that start-build will be run with "-o=name" as a parameter when using this method.
// If no error is returned from this method, it means that the build attempted successfully, NOT that
// the build completed. For completion information, check the BuildResult object.
func StartBuildAndWait(oc *CLI, args ...string) (result *BuildResult, err error) {
	result, err = StartBuildResult(oc, args...)
	if err != nil {
		return result, err
	}
	return result, WaitForBuildResult(oc.BuildClient().BuildV1().Builds(oc.Namespace()), result)
}

// WaitForBuildResult updates result wit the state of the build
func WaitForBuildResult(c buildv1clienttyped.BuildInterface, result *BuildResult) error {
	e2e.Logf("Waiting for %s to complete\n", result.BuildName)
	err := WaitForABuild(c, result.BuildName,
		func(b *buildv1.Build) bool {
			result.Build = b
			result.BuildSuccess = CheckBuildSuccess(b)
			return result.BuildSuccess
		},
		func(b *buildv1.Build) bool {
			result.Build = b
			result.BuildFailure = CheckBuildFailed(b)
			return result.BuildFailure
		},
		func(b *buildv1.Build) bool {
			result.Build = b
			result.BuildCancelled = CheckBuildCancelled(b)
			return result.BuildCancelled
		},
	)

	if result.Build == nil {
		// We only abort here if the build progress was unobservable. Only known cause would be severe, non-build related error in WaitForABuild.
		return fmt.Errorf("Severe error waiting for build: %v", err)
	}

	result.BuildAttempt = true
	result.BuildTimeout = !(result.BuildFailure || result.BuildSuccess || result.BuildCancelled)

	e2e.Logf("Done waiting for %s: %#v\n with error: %v\n", result.BuildName, *result, err)
	return nil
}

// WaitForABuild waits for a Build object to match either isOK or isFailed conditions.
func WaitForABuild(c buildv1clienttyped.BuildInterface, name string, isOK, isFailed, isCanceled func(*buildv1.Build) bool) error {
	if isOK == nil {
		isOK = CheckBuildSuccess
	}
	if isFailed == nil {
		isFailed = CheckBuildFailed
	}
	if isCanceled == nil {
		isCanceled = CheckBuildCancelled
	}

	// wait 2 minutes for build to exist
	err := wait.Poll(1*time.Second, 2*time.Minute, func() (bool, error) {
		if _, err := c.Get(name, metav1.GetOptions{}); err != nil {
			return false, nil
		}
		return true, nil
	})
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("Timed out waiting for build %q to be created", name)
	}
	if err != nil {
		return err
	}
	// wait longer for the build to run to completion
	err = wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
		list, err := c.List(metav1.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String()})
		if err != nil {
			e2e.Logf("error listing builds: %v", err)
			return false, err
		}
		for i := range list.Items {
			if name == list.Items[i].Name && (isOK(&list.Items[i]) || isCanceled(&list.Items[i])) {
				return true, nil
			}
			if name != list.Items[i].Name {
				return false, fmt.Errorf("While listing builds named %s, found unexpected build %#v", name, list.Items[i])
			}
			if isFailed(&list.Items[i]) {
				return false, fmt.Errorf("The build %q status is %q", name, list.Items[i].Status.Phase)
			}
		}
		return false, nil
	})
	if err != nil {
		e2e.Logf("WaitForABuild returning with error: %v", err)
	}
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("Timed out waiting for build %q to complete", name)
	}
	return err
}

// CheckBuildSuccess returns true if the build succeeded
func CheckBuildSuccess(b *buildv1.Build) bool {
	return b.Status.Phase == buildv1.BuildPhaseComplete
}

// CheckBuildFailed return true if the build failed
func CheckBuildFailed(b *buildv1.Build) bool {
	return b.Status.Phase == buildv1.BuildPhaseFailed || b.Status.Phase == buildv1.BuildPhaseError
}

// CheckBuildCancelled return true if the build was canceled
func CheckBuildCancelled(b *buildv1.Build) bool {
	return b.Status.Phase == buildv1.BuildPhaseCancelled
}

// WaitForServiceAccount waits until the named service account gets fully
// provisioned
func WaitForServiceAccount(c corev1client.ServiceAccountInterface, name string) error {
	waitFn := func() (bool, error) {
		sc, err := c.Get(name, metav1.GetOptions{})
		if err != nil {
			// If we can't access the service accounts, let's wait till the controller
			// create it.
			if errors.IsNotFound(err) || errors.IsForbidden(err) {
				return false, nil
			}
			return false, err
		}
		for _, s := range sc.Secrets {
			if strings.Contains(s.Name, "dockercfg") {
				return true, nil
			}
		}
		return false, nil
	}
	return wait.Poll(time.Duration(100*time.Millisecond), 3*time.Minute, waitFn)
}

// WaitForAnImageStream waits for an ImageStream to fulfill the isOK function
func WaitForAnImageStream(client imagev1typedclient.ImageStreamInterface,
	name string,
	isOK, isFailed func(*imagev1.ImageStream) bool) error {
	for {
		list, err := client.List(metav1.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String()})
		if err != nil {
			return err
		}
		for i := range list.Items {
			if isOK(&list.Items[i]) {
				return nil
			}
			if isFailed(&list.Items[i]) {
				return fmt.Errorf("The image stream %q status is %q",
					name, list.Items[i].Annotations[imagev1.DockerImageRepositoryCheckAnnotation])
			}
		}

		rv := list.ResourceVersion
		w, err := client.Watch(metav1.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String(), ResourceVersion: rv})
		if err != nil {
			return err
		}
		defer w.Stop()

		for {
			val, ok := <-w.ResultChan()
			if !ok {
				// reget and re-watch
				break
			}
			if e, ok := val.Object.(*imagev1.ImageStream); ok {
				if isOK(e) {
					return nil
				}
				if isFailed(e) {
					return fmt.Errorf("The image stream %q status is %q",
						name, e.Annotations[imagev1.DockerImageRepositoryCheckAnnotation])
				}
			}
		}
	}
}

// WaitForAnImageStreamTag waits until an image stream with given name has non-empty history for given tag.
// Defaults to waiting for 300 seconds
func WaitForAnImageStreamTag(oc *CLI, namespace, name, tag string) error {
	return TimedWaitForAnImageStreamTag(oc, namespace, name, tag, time.Second*300)
}

// TimedWaitForAnImageStreamTag waits until an image stream with given name has non-empty history for given tag.
// Gives up waiting after the specified waitTimeout
func TimedWaitForAnImageStreamTag(oc *CLI, namespace, name, tag string, waitTimeout time.Duration) error {
	g.By(fmt.Sprintf("waiting for an is importer to import a tag %s into a stream %s", tag, name))
	start := time.Now()
	c := make(chan error)
	go func() {
		err := WaitForAnImageStream(
			oc.ImageClient().ImageV1().ImageStreams(namespace),
			name,
			func(is *imagev1.ImageStream) bool {
				statusTag, exists := imageutil.StatusHasTag(is, tag)
				if !exists || len(statusTag.Items) == 0 {
					return false
				}
				return true
			},
			func(is *imagev1.ImageStream) bool {
				return time.Now().After(start.Add(waitTimeout))
			})
		c <- err
	}()

	select {
	case e := <-c:
		return e
	case <-time.After(waitTimeout):
		return fmt.Errorf("timed out while waiting of an image stream tag %s/%s:%s", namespace, name, tag)
	}
}

// CheckImageStreamLatestTagPopulated returns true if the imagestream has a ':latest' tag filed
func CheckImageStreamLatestTagPopulated(i *imagev1.ImageStream) bool {
	_, ok := imageutil.StatusHasTag(i, "latest")
	return ok
}

// CheckImageStreamTagNotFound return true if the imagestream update was not successful
func CheckImageStreamTagNotFound(i *imagev1.ImageStream) bool {
	return strings.Contains(i.Annotations[imagev1.DockerImageRepositoryCheckAnnotation], "not") ||
		strings.Contains(i.Annotations[imagev1.DockerImageRepositoryCheckAnnotation], "error")
}

// WaitForDeploymentConfig waits for a DeploymentConfig to complete transition
// to a given version and report minimum availability.
func WaitForDeploymentConfig(kc kubernetes.Interface, dcClient appsv1clienttyped.DeploymentConfigsGetter, namespace, name string, version int64, enforceNotProgressing bool, cli *CLI) error {
	e2e.Logf("waiting for deploymentconfig %s/%s to be available with version %d\n", namespace, name, version)
	var dc *appsv1.DeploymentConfig

	start := time.Now()
	err := wait.Poll(time.Second, 15*time.Minute, func() (done bool, err error) {
		dc, err = dcClient.DeploymentConfigs(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		// TODO re-enable this check once @mfojtik introduces a test that ensures we'll only ever get
		// exactly one deployment triggered.
		/*
			if dc.Status.LatestVersion > version {
				return false, fmt.Errorf("latestVersion %d passed %d", dc.Status.LatestVersion, version)
			}
		*/
		if dc.Status.LatestVersion < version {
			return false, nil
		}

		var progressing, available *appsv1.DeploymentCondition
		for i, condition := range dc.Status.Conditions {
			switch condition.Type {
			case appsv1.DeploymentProgressing:
				progressing = &dc.Status.Conditions[i]

			case appsv1.DeploymentAvailable:
				available = &dc.Status.Conditions[i]
			}
		}

		if enforceNotProgressing {
			if progressing != nil && progressing.Status == corev1.ConditionFalse {
				return false, fmt.Errorf("not progressing")
			}
		}

		if progressing != nil &&
			progressing.Status == corev1.ConditionTrue &&
			progressing.Reason == appsutil.NewRcAvailableReason &&
			available != nil &&
			available.Status == corev1.ConditionTrue {
			return true, nil
		}

		return false, nil
	})

	if err != nil {
		e2e.Logf("got error %q when waiting for deploymentconfig %s/%s to be available with version %d\n", err, namespace, name, version)
		cli.Run("get").Args("dc", dc.Name, "-o", "yaml").Execute()

		DumpDeploymentLogs(name, version, cli)
		DumpApplicationPodLogs(name, cli)

		return err
	}

	requirement, err := labels.NewRequirement(appsutil.DeploymentLabel, selection.Equals, []string{appsutil.LatestDeploymentNameForConfigAndVersion(
		dc.Name, dc.Status.LatestVersion)})
	if err != nil {
		return err
	}

	podnames, err := GetPodNamesByFilter(kc.CoreV1().Pods(namespace), labels.NewSelector().Add(*requirement), func(kapiv1.Pod) bool { return true })
	if err != nil {
		return err
	}

	e2e.Logf("deploymentconfig %s/%s available after %s\npods: %s\n", namespace, name, time.Now().Sub(start), strings.Join(podnames, ", "))

	return nil
}

func isUsageSynced(received, expected corev1.ResourceList, expectedIsUpperLimit bool) bool {
	resourceNames := quota.ResourceNames(expected)
	masked := quota.Mask(received, resourceNames)
	if len(masked) != len(expected) {
		return false
	}
	if expectedIsUpperLimit {
		if le, _ := quota.LessThanOrEqual(masked, expected); !le {
			return false
		}
	} else {
		if le, _ := quota.LessThanOrEqual(expected, masked); !le {
			return false
		}
	}
	return true
}

// WaitForResourceQuotaSync watches given resource quota until its usage is updated to desired level or a
// timeout occurs. If successful, used quota values will be returned for expected resources. Otherwise an
// ErrWaitTimeout will be returned. If expectedIsUpperLimit is true, given expected usage must compare greater
// or equal to quota's usage, which is useful for expected usage increment. Otherwise expected usage must
// compare lower or equal to quota's usage, which is useful for expected usage decrement.
func WaitForResourceQuotaSync(
	client corev1client.ResourceQuotaInterface,
	name string,
	expectedUsage corev1.ResourceList,
	expectedIsUpperLimit bool,
	timeout time.Duration,
) (corev1.ResourceList, error) {

	startTime := time.Now()
	endTime := startTime.Add(timeout)

	expectedResourceNames := quota.ResourceNames(expectedUsage)

	list, err := client.List(metav1.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String()})
	if err != nil {
		return nil, err
	}

	for i := range list.Items {
		used := quota.Mask(list.Items[i].Status.Used, expectedResourceNames)
		if isUsageSynced(used, expectedUsage, expectedIsUpperLimit) {
			return used, nil
		}
	}

	rv := list.ResourceVersion
	w, err := client.Watch(metav1.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String(), ResourceVersion: rv})
	if err != nil {
		return nil, err
	}
	defer w.Stop()

	for time.Now().Before(endTime) {
		select {
		case val, ok := <-w.ResultChan():
			if !ok {
				// reget and re-watch
				continue
			}
			if rq, ok := val.Object.(*corev1.ResourceQuota); ok {
				used := quota.Mask(rq.Status.Used, expectedResourceNames)
				if isUsageSynced(used, expectedUsage, expectedIsUpperLimit) {
					return used, nil
				}
			}
		case <-time.After(endTime.Sub(time.Now())):
			return nil, wait.ErrWaitTimeout
		}
	}
	return nil, wait.ErrWaitTimeout
}

// GetPodNamesByFilter looks up pods that satisfy the predicate and returns their names.
func GetPodNamesByFilter(c corev1client.PodInterface, label labels.Selector, predicate func(kapiv1.Pod) bool) (podNames []string, err error) {
	podList, err := c.List(metav1.ListOptions{LabelSelector: label.String()})
	if err != nil {
		return nil, err
	}
	for _, pod := range podList.Items {
		if predicate(pod) {
			podNames = append(podNames, pod.Name)
		}
	}
	return podNames, nil
}

func WaitForAJob(c batchv1client.JobInterface, name string, timeout time.Duration) error {
	return wait.Poll(1*time.Second, timeout, func() (bool, error) {
		j, e := c.Get(name, metav1.GetOptions{})
		if e != nil {
			return true, e
		}
		// TODO soltysh: replace this with a function once such exist, currently
		// it's private in the controller
		for _, c := range j.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == kapiv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// WaitForPods waits until given number of pods that match the label selector and
// satisfy the predicate are found
func WaitForPods(c corev1client.PodInterface, label labels.Selector, predicate func(kapiv1.Pod) bool, count int, timeout time.Duration) ([]string, error) {
	var podNames []string
	err := wait.Poll(1*time.Second, timeout, func() (bool, error) {
		p, e := GetPodNamesByFilter(c, label, predicate)
		if e != nil {
			return true, e
		}
		if len(p) != count {
			return false, nil
		}
		podNames = p
		return true, nil
	})
	return podNames, err
}

// CheckPodIsRunning returns true if the pod is running
func CheckPodIsRunning(pod kapiv1.Pod) bool {
	return pod.Status.Phase == kapiv1.PodRunning
}

// CheckPodIsSucceeded returns true if the pod status is "Succdeded"
func CheckPodIsSucceeded(pod kapiv1.Pod) bool {
	return pod.Status.Phase == kapiv1.PodSucceeded
}

// CheckPodIsReady returns true if the pod's ready probe determined that the pod is ready.
func CheckPodIsReady(pod kapiv1.Pod) bool {
	if pod.Status.Phase != kapiv1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type != kapiv1.PodReady {
			continue
		}
		return cond.Status == kapiv1.ConditionTrue
	}
	return false
}

// CheckPodNoOp always returns true
func CheckPodNoOp(pod kapiv1.Pod) bool {
	return true
}

// WaitUntilPodIsGone waits until the named Pod will disappear
func WaitUntilPodIsGone(c corev1client.PodInterface, podName string, timeout time.Duration) error {
	return wait.Poll(1*time.Second, timeout, func() (bool, error) {
		_, err := c.Get(podName, metav1.GetOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return true, nil
			}
			return true, err
		}
		return false, nil
	})
}

// GetDockerImageReference retrieves the full Docker pull spec from the given ImageStream
// and tag
func GetDockerImageReference(c imagev1typedclient.ImageStreamInterface, name, tag string) (string, error) {
	imageStream, err := c.Get(name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	isTag, ok := imageutil.StatusHasTag(imageStream, tag)
	if !ok {
		return "", fmt.Errorf("ImageStream %q does not have tag %q", name, tag)
	}
	return isTag.Items[0].DockerImageReference, nil
}

// GetPodForContainer creates a new Pod that runs specified container
func GetPodForContainer(container kapiv1.Container) *kapiv1.Pod {
	name := naming.GetPodName("test-pod", string(uuid.NewUUID()))
	return &kapiv1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"name": name},
		},
		Spec: kapiv1.PodSpec{
			Containers:    []kapiv1.Container{container},
			RestartPolicy: kapiv1.RestartPolicyNever,
		},
	}
}

// KubeConfigPath returns the value of KUBECONFIG environment variable
func KubeConfigPath() string {
	// can't use gomega in this method since it is used outside of It()
	return os.Getenv("KUBECONFIG")
}

//ArtifactDirPath returns the value of ARTIFACT_DIR environment variable
func ArtifactDirPath() string {
	path := os.Getenv("ARTIFACT_DIR")
	o.Expect(path).NotTo(o.BeNil())
	o.Expect(path).NotTo(o.BeEmpty())
	return path
}

//ArtifactPath returns the absolute path to the fix artifact file
//The path is relative to ARTIFACT_DIR
func ArtifactPath(elem ...string) string {
	return filepath.Join(append([]string{ArtifactDirPath()}, elem...)...)
}

var (
	fixtureDirLock sync.Once
	fixtureDir     string
)

// FixturePath returns an absolute path to a fixture file in test/extended/testdata/,
// test/integration/, or examples/.
func FixturePath(elem ...string) string {
	switch {
	case len(elem) == 0:
		panic("must specify path")
	case len(elem) > 3 && elem[0] == ".." && elem[1] == ".." && elem[2] == "examples":
		elem = elem[2:]
	case len(elem) > 3 && elem[0] == ".." && elem[1] == ".." && elem[2] == "install":
		elem = elem[2:]
	case len(elem) > 3 && elem[0] == ".." && elem[1] == "integration":
		elem = append([]string{"test"}, elem[1:]...)
	case elem[0] == "testdata":
		elem = append([]string{"test", "extended"}, elem...)
	default:
		panic(fmt.Sprintf("Fixtures must be in test/extended/testdata or examples not %s", path.Join(elem...)))
	}
	fixtureDirLock.Do(func() {
		dir, err := ioutil.TempDir("", "fixture-testdata-dir")
		if err != nil {
			panic(err)
		}
		fixtureDir = dir
	})
	relativePath := path.Join(elem...)
	fullPath := path.Join(fixtureDir, relativePath)
	if err := testdata.RestoreAsset(fixtureDir, relativePath); err != nil {
		if err := testdata.RestoreAssets(fixtureDir, relativePath); err != nil {
			panic(err)
		}
		if err := filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
			if err := os.Chmod(path, 0640); err != nil {
				return err
			}
			if stat, err := os.Lstat(path); err == nil && stat.IsDir() {
				return os.Chmod(path, 0755)
			}
			return nil
		}); err != nil {
			panic(err)
		}
	} else {
		if err := os.Chmod(fullPath, 0640); err != nil {
			panic(err)
		}
	}

	p, err := filepath.Abs(fullPath)
	if err != nil {
		panic(err)
	}
	return p
}

// FetchURL grabs the output from the specified url and returns it.
// It will retry once per second for duration retryTimeout if an error occurs during the request.
func FetchURL(oc *CLI, url string, retryTimeout time.Duration) (string, error) {

	ns := oc.KubeFramework().Namespace.Name
	execPodName := CreateExecPodOrFail(oc.AdminKubeClient().CoreV1(), ns, string(uuid.NewUUID()))
	defer func() { oc.AdminKubeClient().CoreV1().Pods(ns).Delete(execPodName, metav1.NewDeleteOptions(1)) }()

	execPod, err := oc.AdminKubeClient().CoreV1().Pods(ns).Get(execPodName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	var response string
	waitFn := func() (bool, error) {
		e2e.Logf("Waiting up to %v to wget %s", retryTimeout, url)
		//cmd := fmt.Sprintf("wget -T 30 -O- %s", url)
		cmd := fmt.Sprintf("curl -vvv %s", url)
		response, err = e2e.RunHostCmd(execPod.Namespace, execPod.Name, cmd)
		if err != nil {
			e2e.Logf("got err: %v, retry until timeout", err)
			return false, nil
		}
		// Need to check output because wget -q might omit the error.
		if strings.TrimSpace(response) == "" {
			e2e.Logf("got empty stdout, retry until timeout")
			return false, nil
		}
		return true, nil
	}
	pollErr := wait.Poll(time.Duration(1*time.Second), retryTimeout, waitFn)
	if pollErr == wait.ErrWaitTimeout {
		return "", fmt.Errorf("Timed out while fetching url %q", url)
	}
	if pollErr != nil {
		return "", pollErr
	}
	return response, nil
}

// ParseLabelsOrDie turns the given string into a label selector or
// panics; for tests or other cases where you know the string is valid.
// TODO: Move this to the upstream labels package.
func ParseLabelsOrDie(str string) labels.Selector {
	ret, err := labels.Parse(str)
	if err != nil {
		panic(fmt.Sprintf("cannot parse '%v': %v", str, err))
	}
	return ret
}

// GetEndpointAddress will return an "ip:port" string for the endpoint.
func GetEndpointAddress(oc *CLI, name string) (string, error) {
	err := e2e.WaitForEndpoint(oc.KubeFramework().ClientSet, oc.Namespace(), name)
	if err != nil {
		return "", err
	}
	endpoint, err := oc.KubeClient().CoreV1().Endpoints(oc.Namespace()).Get(name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port), nil
}

// CreateExecPodOrFail creates a simple busybox pod in a sleep loop used as a
// vessel for kubectl exec commands.
// Returns the name of the created pod.
// TODO: expose upstream
func CreateExecPodOrFail(client corev1client.CoreV1Interface, ns, name string) string {
	e2e.Logf("Creating new exec pod")
	execPod := e2e.NewExecPodSpec(ns, name, true)
	created, err := client.Pods(ns).Create(execPod)
	o.Expect(err).NotTo(o.HaveOccurred())
	err = wait.PollImmediate(e2e.Poll, 5*time.Minute, func() (bool, error) {
		retrievedPod, err := client.Pods(execPod.Namespace).Get(created.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return retrievedPod.Status.Phase == kapiv1.PodRunning, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred())
	return created.Name
}

// CheckForBuildEvent will poll a build for up to 1 minute looking for an event with
// the specified reason and message template.
func CheckForBuildEvent(client corev1client.CoreV1Interface, build *buildv1.Build, reason, message string) {
	scheme, _ := apitesting.SchemeForOrDie(buildv1.Install)
	var expectedEvent *kapiv1.Event
	err := wait.PollImmediate(e2e.Poll, 1*time.Minute, func() (bool, error) {
		events, err := client.Events(build.Namespace).Search(scheme, build)
		if err != nil {
			return false, err
		}
		for _, event := range events.Items {
			e2e.Logf("Found event %#v", event)
			if reason == event.Reason {
				expectedEvent = &event
				return true, nil
			}
		}
		return false, nil
	})
	o.ExpectWithOffset(1, err).NotTo(o.HaveOccurred(), "Should be able to get events from the build")
	o.ExpectWithOffset(1, expectedEvent).NotTo(o.BeNil(), "Did not find a %q event on build %s/%s", reason, build.Namespace, build.Name)
	o.ExpectWithOffset(1, expectedEvent.Message).To(o.Equal(fmt.Sprintf(message, build.Namespace, build.Name)))
}

type podExecutor struct {
	client  *CLI
	podName string
}

// NewPodExecutor returns an executor capable of running commands in a Pod.
func NewPodExecutor(oc *CLI, name, image string) (*podExecutor, error) {
	out, err := oc.Run("run").Args(name, "--labels", "name="+name, "--image", image, "--restart", "Never", "--command", "--", "/bin/bash", "-c", "sleep infinity").Output()
	if err != nil {
		return nil, fmt.Errorf("error: %v\n(%s)", err, out)
	}
	_, err = WaitForPods(oc.KubeClient().CoreV1().Pods(oc.Namespace()), ParseLabelsOrDie("name="+name), CheckPodIsReady, 1, 3*time.Minute)
	if err != nil {
		return nil, err
	}
	return &podExecutor{client: oc, podName: name}, nil
}

// Exec executes a single command or a bash script in the running pod. It returns the
// command output and error if the command finished with non-zero status code or the
// command took longer then 3 minutes to run.
func (r *podExecutor) Exec(script string) (string, error) {
	var out string
	waitErr := wait.PollImmediate(1*time.Second, 3*time.Minute, func() (bool, error) {
		var err error
		out, err = r.client.Run("exec").Args(r.podName, "--", "/bin/bash", "-c", script).Output()
		return true, err
	})
	return out, waitErr
}

func (r *podExecutor) CopyFromHost(local, remote string) error {
	_, err := r.client.Run("cp").Args(local, fmt.Sprintf("%s:%s", r.podName, remote)).Output()
	return err
}

// RunOneShotCommandPod runs the given command in a pod and waits for completion and log output for the given timeout
// duration, returning the command output or an error.
// TODO: merge with the PodExecutor above
func RunOneShotCommandPod(
	oc *CLI,
	name, image, command string,
	volumeMounts []corev1.VolumeMount,
	volumes []corev1.Volume,
	env []corev1.EnvVar,
	timeout time.Duration,
) (string, []error) {
	errs := []error{}
	cmd := strings.Split(command, " ")
	args := cmd[1:]
	var output string

	pod, err := oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).Create(newCommandPod(name, image, cmd[0], args,
		volumeMounts, volumes, env))
	if err != nil {
		return "", []error{err}
	}

	// Wait for command completion.
	err = wait.PollImmediate(1*time.Second, timeout, func() (done bool, err error) {
		cmdPod, getErr := oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).Get(pod.Name, v1.GetOptions{})
		if err != nil {
			return false, getErr
		}

		if podHasErrored(cmdPod) {
			return true, fmt.Errorf("the pod errored trying to run the command")
		}
		return podHasCompleted(cmdPod), nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("error waiting for the pod '%s' to complete: %v", pod.Name, err))
	}

	// Gather pod log output
	err = wait.PollImmediate(1*time.Second, timeout, func() (done bool, err error) {
		logs, logErr := getPodLogs(oc, pod)
		if logErr != nil {
			return false, logErr
		}
		if len(logs) == 0 {
			return false, nil
		}
		output = logs
		return true, nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("command pod %s did not complete: %v", pod.Name, err))
	}

	return output, errs
}

func podHasCompleted(pod *corev1.Pod) bool {
	return len(pod.Status.ContainerStatuses) > 0 &&
		pod.Status.ContainerStatuses[0].State.Terminated != nil &&
		pod.Status.ContainerStatuses[0].State.Terminated.Reason == "Completed"
}

func podHasErrored(pod *corev1.Pod) bool {
	return len(pod.Status.ContainerStatuses) > 0 &&
		pod.Status.ContainerStatuses[0].State.Terminated != nil &&
		pod.Status.ContainerStatuses[0].State.Terminated.Reason == "Error"
}

func getPodLogs(oc *CLI, pod *corev1.Pod) (string, error) {
	reader, err := oc.AdminKubeClient().CoreV1().Pods(oc.Namespace()).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream()
	if err != nil {
		return "", err
	}
	logs, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(logs), nil
}

func newCommandPod(name, image, command string, args []string, volumeMounts []corev1.VolumeMount,
	volumes []corev1.Volume, env []corev1.EnvVar) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
		},
		Spec: corev1.PodSpec{
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            name,
					Image:           image,
					Command:         []string{command},
					Args:            args,
					VolumeMounts:    volumeMounts,
					ImagePullPolicy: "Always",
					Env:             env,
				},
			},
		},
	}
}

type GitRepo struct {
	baseTempDir  string
	upstream     git.Repository
	upstreamPath string
	repo         git.Repository
	RepoPath     string
}

// AddAndCommit commits a file with its content to local repo
func (r GitRepo) AddAndCommit(file, content string) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(filepath.Join(r.RepoPath, dir), 0777); err != nil {
		return err
	}
	if err := ioutil.WriteFile(filepath.Join(r.RepoPath, file), []byte(content), 0666); err != nil {
		return err
	}
	if err := r.repo.Add(r.RepoPath, file); err != nil {
		return err
	}
	if err := r.repo.Commit(r.RepoPath, "added file "+file); err != nil {
		return err
	}
	return nil
}

// Remove performs cleanup of no longer needed directories with local and "remote" git repo
func (r GitRepo) Remove() {
	if r.baseTempDir != "" {
		os.RemoveAll(r.baseTempDir)
	}
}

// NewGitRepo creates temporary test directories with local and "remote" git repo
func NewGitRepo(repoName string) (GitRepo, error) {
	testDir, err := ioutil.TempDir(os.TempDir(), repoName)
	if err != nil {
		return GitRepo{}, err
	}
	repoPath := filepath.Join(testDir, repoName)
	upstreamPath := repoPath + `.git`
	upstream := git.NewRepository()
	if err = upstream.Init(upstreamPath, true); err != nil {
		return GitRepo{baseTempDir: testDir}, err
	}
	repo := git.NewRepository()
	if err = repo.Clone(repoPath, upstreamPath); err != nil {
		return GitRepo{baseTempDir: testDir}, err
	}

	return GitRepo{testDir, upstream, upstreamPath, repo, repoPath}, nil
}

// WaitForUserBeAuthorized waits a minute until the cluster bootstrap roles are available
// and the provided user is authorized to perform the action on the resource.
func WaitForUserBeAuthorized(oc *CLI, user, verb, resource string) error {
	sar := &authorizationapi.SubjectAccessReview{
		Spec: authorizationapi.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationapi.ResourceAttributes{
				Namespace: oc.Namespace(),
				Verb:      verb,
				Resource:  resource,
			},
			User: user,
		},
	}
	return wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
		resp, err := oc.AdminKubeClient().AuthorizationV1().SubjectAccessReviews().Create(sar)
		if err == nil && resp != nil && resp.Status.Allowed {
			return true, nil
		}
		return false, err
	})
}

// GetRouterPodTemplate finds the router pod template across different namespaces,
// helping to mitigate the transition from the default namespace to an operator
// namespace.
func GetRouterPodTemplate(oc *CLI) (*corev1.PodTemplateSpec, string, error) {
	appsclient := oc.AdminAppsClient().AppsV1()
	k8sappsclient := oc.AdminKubeClient().AppsV1()
	for _, ns := range []string{"default", "openshift-ingress", "tectonic-ingress"} {
		dc, err := appsclient.DeploymentConfigs(ns).Get("router", metav1.GetOptions{})
		if err == nil {
			return dc.Spec.Template, ns, nil
		}
		if !errors.IsNotFound(err) {
			return nil, "", err
		}
		deploy, err := k8sappsclient.Deployments(ns).Get("router", metav1.GetOptions{})
		if err == nil {
			return &deploy.Spec.Template, ns, nil
		}
		if !errors.IsNotFound(err) {
			return nil, "", err
		}
		deploy, err = k8sappsclient.Deployments(ns).Get("router-default", metav1.GetOptions{})
		if err == nil {
			return &deploy.Spec.Template, ns, nil
		}
		if !errors.IsNotFound(err) {
			return nil, "", err
		}
	}
	return nil, "", errors.NewNotFound(schema.GroupResource{Group: "apps.openshift.io", Resource: "deploymentconfigs"}, "router")
}

// FindImageFormatString returns a format string for components on the cluster. It returns false
// if no format string could be inferred from the cluster. OpenShift 4.0 clusters will not be able
// to infer an image format string, so you must wrap this method in one that can locate your specific
// image.
func FindImageFormatString(oc *CLI) (string, bool) {
	// legacy support for 3.x clusters
	template, _, err := GetRouterPodTemplate(oc)
	if err == nil {
		if strings.Contains(template.Spec.Containers[0].Image, "haproxy-router") {
			return strings.Replace(template.Spec.Containers[0].Image, "haproxy-router", "${component}", -1), true
		}
	}
	// in openshift 4.0, no image format can be calculated on cluster
	return "openshift/origin-${component}:latest", false
}

func FindCLIImage(oc *CLI) (string, bool) {
	// look up image stream
	is, err := oc.AdminImageClient().ImageV1().ImageStreams("openshift").Get("cli", metav1.GetOptions{})
	if err == nil {
		for _, tag := range is.Spec.Tags {
			if tag.Name == "latest" && tag.From != nil && tag.From.Kind == "DockerImage" {
				return tag.From.Name, true
			}
		}
	}

	format, ok := FindImageFormatString(oc)
	return strings.Replace(format, "${component}", "cli", -1), ok
}

func FindRouterImage(oc *CLI) (string, bool) {
	format, ok := FindImageFormatString(oc)
	return strings.Replace(format, "${component}", "haproxy-router", -1), ok
}

func IsClusterOperated(oc *CLI) bool {
	configclient := oc.AdminConfigClient().ConfigV1()
	o, err := configclient.Images().Get("cluster", metav1.GetOptions{})
	if o == nil || err != nil {
		e2e.Logf("Could not find image config object, assuming non-4.0 installed cluster: %v", err)
		return false
	}
	return true
}
