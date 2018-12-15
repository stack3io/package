package metaparticle

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesExecuterConfig is the configuration needed to deploy to kubernetes
// such as the namespace
type KubernetesExecuterConfig struct {
	Namespace string
}

// KubernetesImpl is a kubernetes implementation of the Builder and Executor interfaces
type KubernetesImpl struct {
	imageClient     dockerImageClient
	containerRunner *kubernetes.Clientset
	authStr         string
}

func newKubernetesImpl(imageClient dockerImageClient, containerRunner *kubernetes.Clientset) (*KubernetesImpl, error) {
	authStr, err := getAuthStringFromEnv()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create auth string from environment variables (did you forget to set MP_REGISTRY_USER or MP_REGISTRY_PASSWORD?)")
	}
	if imageClient == nil && containerRunner == nil {

		dockerClient, err := client.NewEnvClient()
		if err != nil {
			return nil, errors.Wrap(err, "Failed to create docker client")
		}

		var k8sclient *kubernetes.Clientset
		var kubeconfig string

		// try to get an in-cluster config
		config, err := rest.InClusterConfig()
		if err != nil {
			// @todo this should be configurable
			if home := homeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}

			// use the current context in kubeconfig
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return nil, err
			}
		}

		// creates the kubernetes clientset
		k8sclient, err = kubernetes.NewForConfig(config)
		if err != nil {
			return nil, err
		}

		return &KubernetesImpl{dockerClient, k8sclient, authStr}, nil
	}
	return &KubernetesImpl{imageClient, containerRunner, authStr}, nil
}

// NewKubernetesImpl returns a singleton struct that uses docker to implement metaparticle.Builder and
// kubernetes to implement metaparticle.Executor.
//
// It uses the environment variables DOCKER_CERT_PATH, DOCKER_HOST, DOCKER_API_VERSION and DOCKER_TLS_VERIFY
// to instantiate instantiate a docker API client.
// When these variables are not specified, it defaults to the client running on the local machine.
func NewKubernetesImpl() (*KubernetesImpl, error) {
	return newKubernetesImpl(nil, nil)
}

// Run creates and starts a container with the given image and name, and runtime options (e.g. exposed ports) specified in the config parameter
func (k *KubernetesImpl) Run(image string, name string, config *Runtime, stdout io.Writer, stderr io.Writer) error {
	if len(image) == 0 {
		return errEmptyImageName
	}

	if len(name) == 0 {
		return errEmptyContainerName
	}

	if config == nil {
		return errNilRuntimeConfig
	}

	if len(config.Ports) != 0 {
		if err := k.createService(image, name, config); err != nil {
			return err
		}
	}

	// @question should we create the deployment if replicas is 0?
	if err := k.createDeployment(image, name, config, true); err != nil {
		return err
	}

	return nil
}

func (k *KubernetesImpl) createDeployment(image string, name string, config *Runtime, wait bool) error {
	var ports []v1.ContainerPort
	for _, port := range config.Ports {
		ports = append(ports, v1.ContainerPort{
			ContainerPort: port,
		})
	}

	container := v1.Container{
		Name:  name,
		Image: image,
		Ports: ports,
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app": name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &config.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{container},
				},
			},
		},
	}

	_, err := k.containerRunner.AppsV1().Deployments(deployment.Namespace).Get(deployment.Name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
		log.Println("deployment not found, creating...")
		deployment, err = k.containerRunner.AppsV1().Deployments(deployment.Namespace).Create(deployment)
		if err != nil {
			return err
		}
		log.Println("deployment created successfully")
	} else {
		log.Println("deployment already exists, updatating...")
		deployment, err = k.containerRunner.AppsV1().Deployments(deployment.Namespace).Update(deployment)
		if err != nil {
			return err
		}
		log.Println("deployment updated successfully")
	}

	if wait {
		log.Println("waiting for running pod")
		for {
			pod, _ := k.findPod(name)
			if pod == nil {
				log.Println("pod is not running...")
				time.Sleep(1 * time.Second)
				continue
			}
			log.Println("pod is running")
			break
		}
	}
	return nil
}

func (k *KubernetesImpl) createService(image string, name string, config *Runtime) error {
	var ports []v1.ServicePort
	for i, port := range config.Ports {
		ports = append(ports, v1.ServicePort{
			Name:       fmt.Sprintf("%d", i),
			Port:       port,
			TargetPort: intstr.FromInt(int(port)),
			Protocol:   v1.ProtocolTCP,
		})
	}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app": name,
			},
		},
		Spec: v1.ServiceSpec{
			Ports: ports,
			Selector: map[string]string{
				"app": name,
			},
		},
	}
	if config.PublicAddress {
		service.Spec.Type = v1.ServiceTypeLoadBalancer
	}

	_, err := k.containerRunner.CoreV1().Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
		log.Println("service not found, creating...")

		_, err := k.containerRunner.CoreV1().Services(service.Namespace).Create(service)
		if err != nil {
			return err
		}
		log.Println("service created successfully")
	} else {
		log.Println("service already exists...")
		//service, err = k.containerRunner.CoreV1().Services(service.Namespace).Update(service)
		//if err != nil {
		//	return err
		//}
		// log.Println("service updated successfully")
	}
	return nil
}

func (k *KubernetesImpl) findPod(name string) (*v1.Pod, error) {
	if len(name) == 0 {
		return nil, errEmptyContainerName
	}
	podList, err := k.containerRunner.CoreV1().Pods("default").List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
		Limit:         1,
	})
	if err != nil {
		return nil, err
	}
	if len(podList.Items) == 0 {
		return nil, errNoRunningContainer
	}

	var runningPod *v1.Pod
	for _, pod := range podList.Items {
		if len(pod.Status.ContainerStatuses) == 0 {
			continue
		}
		if pod.Status.ContainerStatuses[0].State.Running != nil {
			runningPod = &pod
			break
		}
	}
	if runningPod == nil {
		return nil, errNoRunningContainer
	}
	return runningPod, nil
}

// Logs attaches to the container with the given name and prints the log to stdout
func (k *KubernetesImpl) Logs(name string, stdout io.Writer, stderr io.Writer) error {
	selectedPod, err := k.findPod(name)
	if err != nil {
		return err
	}

	req := k.containerRunner.CoreV1().Pods(selectedPod.Namespace).GetLogs(selectedPod.Name, &v1.PodLogOptions{
		Follow: true,
	})

	readCloser, err := req.Stream()
	if err != nil {
		return err
	}

	defer readCloser.Close()

	_, err = io.Copy(stdout, readCloser)
	return err
}

// Cancel stops and removes the container with the given name
func (k *KubernetesImpl) Cancel(name string) error {
	if len(name) == 0 {
		return errEmptyContainerName
	}

	if err := k.containerRunner.CoreV1().Services("default").Delete(name, &metav1.DeleteOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
		log.Println("service deleted")
	}
	if err := k.containerRunner.AppsV1().Deployments("default").Delete(name, &metav1.DeleteOptions{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
		log.Println("deployment deleted")
	}

	return nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
