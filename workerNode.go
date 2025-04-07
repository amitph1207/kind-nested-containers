// my_worker_node.go
package main

import (
    "context"
    "flag"
    "fmt"
    "os"
    "time"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/tools/clientcmd"
    "k8s.io/client-go/tools/remotecommand"
    "k8s.io/klog/v2"
    rbacv1 "k8s.io/api/rbac/v1"
)

type WorkerNodeConfig struct {
    Name                 string
    ControlPlaneEndpoint string
    Token               string
    CACertHash          string
    Namespace           string
    KindNodeImage       string
}

func main() {
    config := &WorkerNodeConfig{}
    flag.StringVar(&config.Name, "name", "worker-1", "Name of the worker node")
    flag.StringVar(&config.ControlPlaneEndpoint, "endpoint", "", "Control plane endpoint")
    flag.StringVar(&config.Token, "token", "", "Join token")
    flag.StringVar(&config.CACertHash, "ca-cert-hash", "", "CA certificate hash")
    flag.StringVar(&config.Namespace, "namespace", "default", "Namespace for the worker node")
    flag.StringVar(&config.KindNodeImage, "image", "kindest/node:v1.30.0", "Kind node image")
    flag.Parse()

    if config.ControlPlaneEndpoint == "" || config.Token == "" || config.CACertHash == "" {
        klog.Fatal("Required parameters are missing")
    }

    klog.Info("Starting worker node setup process...")
    klog.Infof("Configuration: name=%s, endpoint=%s, namespace=%s", 
        config.Name, config.ControlPlaneEndpoint, config.Namespace)

    // Create k8s client
    k8sClient, err := createK8sClient()
    if err != nil {
        klog.Fatalf("Failed to create k8s client: %v", err)
    }

    // Check API server connectivity
    if err := checkAPIServerConnectivity(context.Background(), k8sClient); err != nil {
        klog.Fatalf("Failed to connect to API server: %v", err)
    }

    // Delete existing worker node pod if it exists
    if err := deleteExistingPod(context.Background(), k8sClient, config); err != nil {
        klog.Fatalf("Failed to delete existing pod: %v", err)
    }

    // Create worker node pod
    if err := createWorkerNode(context.Background(), k8sClient, config); err != nil {
        klog.Fatalf("Failed to create worker node: %v", err)
    }

    // Wait for pod to be running
    if err := waitForPodRunning(context.Background(), k8sClient, config); err != nil {
        klog.Fatalf("Failed to wait for pod running: %v", err)
    }

    // Join cluster
    if err := configureAndJoinCluster(context.Background(), k8sClient, config); err != nil {
        klog.Fatalf("Failed to join cluster: %v", err)
    }

    // Wait for node to be ready
    if err := waitForNodeReady(context.Background(), k8sClient, config); err != nil {
        klog.Fatalf("Failed to wait for node ready: %v", err)
    }

    klog.Info("Worker node setup completed successfully!")
}

func createWorkerNodeRBAC(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    // Create ServiceAccount
    sa := &corev1.ServiceAccount{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "worker-node-sa",
            Namespace: config.Namespace,
        },
    }
    _, err := k8sClient.CoreV1().ServiceAccounts(config.Namespace).Create(ctx, sa, metav1.CreateOptions{})
    if err != nil {
        return fmt.Errorf("failed to create service account: %v", err)
    }

    // Create ClusterRole
    cr := &rbacv1.ClusterRole{
        ObjectMeta: metav1.ObjectMeta{
            Name: "worker-node-role",
        },
        Rules: []rbacv1.PolicyRule{
            {
                APIGroups: []string{""},
                Resources: []string{"configmaps"},
                Verbs:     []string{"get", "list", "watch"},
            },
            {
                APIGroups: []string{""},
                Resources: []string{"nodes"},
                Verbs:     []string{"create", "get", "list", "watch", "patch", "update"},
            },
            {
                APIGroups: []string{""},
                Resources: []string{"nodes/status"},
                Verbs:     []string{"patch", "update"},
            },
        },
    }
    _, err = k8sClient.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
    if err != nil {
        return fmt.Errorf("failed to create cluster role: %v", err)
    }

    // Create ClusterRoleBinding
    crb := &rbacv1.ClusterRoleBinding{
        ObjectMeta: metav1.ObjectMeta{
            Name: "worker-node-binding",
        },
        RoleRef: rbacv1.RoleRef{
            APIGroup: "rbac.authorization.k8s.io",
            Kind:     "ClusterRole",
            Name:     "worker-node-role",
        },
        Subjects: []rbacv1.Subject{
            {
                Kind:      "ServiceAccount",
                Name:      "worker-node-sa",
                Namespace: config.Namespace,
            },
        },
    }
    _, err = k8sClient.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
    if err != nil {
        return fmt.Errorf("failed to create cluster role binding: %v", err)
    }

    return nil
}

func createServiceAccountToken(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name: "worker-node-sa-token",
            Namespace: config.Namespace,
            Annotations: map[string]string{
                "kubernetes.io/service-account.name": "worker-node-sa",
            },
        },
        Type: corev1.SecretTypeServiceAccountToken,
        Data: map[string][]byte{
            "token": []byte(""), // Will be automatically populated by Kubernetes
        },
    }

    _, err := k8sClient.CoreV1().Secrets(config.Namespace).Create(ctx, secret, metav1.CreateOptions{})
    if err != nil {
        return fmt.Errorf("failed to create service account token secret: %v", err)
    }

    return nil
}

func createWorkerNode(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    // Create RBAC resources
    if err := createWorkerNodeRBAC(ctx, k8sClient, config); err != nil {
        return fmt.Errorf("failed to create RBAC resources: %v", err)
    }

    // Create service account token
    if err := createServiceAccountToken(ctx, k8sClient, config); err != nil {
        return fmt.Errorf("failed to create service account token: %v", err)
    }

    // Define the worker node pod
    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      config.Name,
            Namespace: config.Namespace,
        },
        Spec: corev1.PodSpec{
            Hostname: config.Name,
            ServiceAccountName: "worker-node-sa",
            HostAliases: []corev1.HostAlias{
                {
                    IP:        "172.18.0.2",
                    Hostnames: []string{"test-cluster-control-plane", "test-cluster-control-plane.kind-control-plane.svc.cluster.local"},
                },
            },
            Containers: []corev1.Container{
                {
                    Name:  "kind-container",
                    Image: config.KindNodeImage,
                    SecurityContext: &corev1.SecurityContext{
                        Privileged: &[]bool{true}[0],
                        SeccompProfile: &corev1.SeccompProfile{
                            Type: corev1.SeccompProfileTypeUnconfined,
                        },
                        Capabilities: &corev1.Capabilities{
                            Add: []corev1.Capability{
                                "SYS_ADMIN",
                                "SYS_MODULE",
                                "NET_ADMIN",
                                "IPC_LOCK",
                            },
                        },
                    },
                    VolumeMounts: []corev1.VolumeMount{
                        {
                            Name:      "lib-modules",
                            MountPath: "/lib/modules",
                            ReadOnly:  true,
                        },
                        {
                            Name:      "run-tmpfs",
                            MountPath: "/run",
                        },
                        {
                            Name:      "tmp-tmpfs",
                            MountPath: "/tmp",
                        },
                        {
                            Name:      "containerd-data",
                            MountPath: "/var/lib/containerd",
                        },
                        {
                            Name:      "containerd-run",
                            MountPath: "/var/run/containerd",
                        },
                        {
                            Name:      "containerd-config",
                            MountPath: "/etc/containerd",
                        },
                        {
                            Name:      "kubelet-config",
                            MountPath: "/var/lib/kubelet",
                        },
                        {
                            Name:      "etc-hosts",
                            MountPath: "/etc/hosts",
                        },
                        {
                            Name:      "service-account-token",
                            MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
                            ReadOnly:  true,
                        },
                    },
                },
            },
            Volumes: []corev1.Volume{
                {
                    Name: "lib-modules",
                    VolumeSource: corev1.VolumeSource{
                        HostPath: &corev1.HostPathVolumeSource{
                            Path: "/lib/modules",
                            Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
                        },
                    },
                },
                {
                    Name: "run-tmpfs",
                    VolumeSource: corev1.VolumeSource{
                        EmptyDir: &corev1.EmptyDirVolumeSource{
                            Medium: corev1.StorageMediumMemory,
                        },
                    },
                },
                {
                    Name: "tmp-tmpfs",
                    VolumeSource: corev1.VolumeSource{
                        EmptyDir: &corev1.EmptyDirVolumeSource{
                            Medium: corev1.StorageMediumMemory,
                        },
                    },
                },
                {
                    Name: "containerd-data",
                    VolumeSource: corev1.VolumeSource{
                        HostPath: &corev1.HostPathVolumeSource{
                            Path: "/var/lib/containerd",
                            Type: &[]corev1.HostPathType{corev1.HostPathDirectoryOrCreate}[0],
                        },
                    },
                },
                {
                    Name: "containerd-run",
                    VolumeSource: corev1.VolumeSource{
                        HostPath: &corev1.HostPathVolumeSource{
                            Path: "/var/run/containerd",
                            Type: &[]corev1.HostPathType{corev1.HostPathDirectoryOrCreate}[0],
                        },
                    },
                },
                {
                    Name: "containerd-config",
                    VolumeSource: corev1.VolumeSource{
                        HostPath: &corev1.HostPathVolumeSource{
                            Path: "/etc/containerd",
                            Type: &[]corev1.HostPathType{corev1.HostPathDirectoryOrCreate}[0],
                        },
                    },
                },
                {
                    Name: "kubelet-config",
                    VolumeSource: corev1.VolumeSource{
                        EmptyDir: &corev1.EmptyDirVolumeSource{},
                    },
                },
                {
                    Name: "etc-hosts",
                    VolumeSource: corev1.VolumeSource{
                        EmptyDir: &corev1.EmptyDirVolumeSource{
                            Medium: corev1.StorageMediumMemory,
                        },
                    },
                },
                {
                    Name: "service-account-token",
                    VolumeSource: corev1.VolumeSource{
                        Secret: &corev1.SecretVolumeSource{
                            SecretName: "worker-node-sa-token",
                        },
                    },
                },
            },
        },
    }

    // Create the pod
    _, err := k8sClient.CoreV1().Pods(config.Namespace).Create(ctx, pod, metav1.CreateOptions{})
    if err != nil {
        return fmt.Errorf("failed to create pod: %v", err)
    }

    return nil
}

func waitForPodRunning(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    klog.Info("Waiting for pod to be Running...")
    for {
        pod, err := k8sClient.CoreV1().Pods(config.Namespace).Get(ctx, config.Name, metav1.GetOptions{})
        if err != nil {
            return fmt.Errorf("failed to get pod: %v", err)
        }
        klog.Infof("Pod status: Phase=%s, Conditions=%v", pod.Status.Phase, pod.Status.Conditions)
        if pod.Status.Phase == corev1.PodRunning {
            klog.Info("Pod is now in Running state")
            break
        }
        klog.Info("Pod is not yet Running, waiting 2 seconds...")
        time.Sleep(2 * time.Second)
    }
    return nil
}

func waitForNodeReady(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    klog.Info("Waiting for node to be Ready...")
    for {
        node, err := k8sClient.CoreV1().Nodes().Get(ctx, config.Name, metav1.GetOptions{})
        if err != nil {
            klog.Infof("Node not found yet, waiting 2 seconds... (error: %v)", err)
            time.Sleep(2 * time.Second)
            continue
        }
        klog.Infof("Node status: Conditions=%v", node.Status.Conditions)
        for _, condition := range node.Status.Conditions {
            if condition.Type == "Ready" && condition.Status == "True" {
                klog.Info("Node is now Ready")
                return nil
            }
        }
        klog.Info("Node is not yet Ready, waiting 2 seconds...")
        time.Sleep(2 * time.Second)
    }
}

func configureAndJoinCluster(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    klog.Info("Joining cluster...")
    cmd := []string{
        "kubeadm",
        "join",
        "172.18.0.2:6443",
        "--token",
        config.Token,
        "--discovery-token-ca-cert-hash",
        "sha256:" + config.CACertHash,
        "--ignore-preflight-errors=SystemVerification,Hostname",
        "--v=5",
    }
    if err := execInPod(ctx, k8sClient, config.Namespace, config.Name, cmd); err != nil {
        return fmt.Errorf("failed to join cluster: %v", err)
    }
    klog.Info("Successfully joined cluster")
    return nil
}

func createK8sClient() (*kubernetes.Clientset, error) {
    config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
    if err != nil {
        return nil, err
    }
    return kubernetes.NewForConfig(config)
}

func execInPod(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, podName string, cmd []string) error {
    klog.Infof("Executing command in pod %s/%s: %v", namespace, podName, cmd)
    
    pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get pod: %v", err)
    }

    if len(pod.Spec.Containers) == 0 {
        return fmt.Errorf("pod has no containers")
    }
    containerName := pod.Spec.Containers[0].Name

    req := k8sClient.CoreV1().RESTClient().Post().
        Resource("pods").
        Name(podName).
        Namespace(namespace).
        SubResource("exec")

    req.VersionedParams(&corev1.PodExecOptions{
        Container: containerName,
        Command:   cmd,
        Stdin:     false,
        Stdout:    true,
        Stderr:    true,
        TTY:       false,
    }, scheme.ParameterCodec)

    config, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
    if err != nil {
        return fmt.Errorf("failed to get REST config: %v", err)
    }

    exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
    if err != nil {
        return fmt.Errorf("failed to create executor: %v", err)
    }

    err = exec.Stream(remotecommand.StreamOptions{
        Stdout: os.Stdout,
        Stderr: os.Stderr,
    })
    if err != nil {
        return fmt.Errorf("failed to execute command: %v", err)
    }

    klog.Info("Command executed successfully")
    return nil
}

func deleteExistingPod(ctx context.Context, k8sClient *kubernetes.Clientset, config *WorkerNodeConfig) error {
    _, err := k8sClient.CoreV1().Pods(config.Namespace).Get(ctx, config.Name, metav1.GetOptions{})
    if err != nil {
        klog.Info("No existing worker node pod found")
        return nil
    }

    klog.Info("Found existing worker node pod, deleting it...")
    err = k8sClient.CoreV1().Pods(config.Namespace).Delete(ctx, config.Name, metav1.DeleteOptions{})
    if err != nil {
        return fmt.Errorf("failed to delete existing pod: %v", err)
    }

    for {
        _, err := k8sClient.CoreV1().Pods(config.Namespace).Get(ctx, config.Name, metav1.GetOptions{})
        if err != nil {
            klog.Info("Existing worker node pod successfully deleted")
            return nil
        }
        klog.Info("Waiting for existing pod to be deleted...")
        time.Sleep(2 * time.Second)
    }
}

func checkAPIServerConnectivity(ctx context.Context, k8sClient *kubernetes.Clientset) error {
    klog.Info("Checking API server connectivity...")
    maxRetries := 5
    for i := 0; i < maxRetries; i++ {
        _, err := k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
        if err == nil {
            klog.Info("Successfully connected to API server")
            return nil
        }
        klog.Warningf("Failed to connect to API server (attempt %d/%d): %v", i+1, maxRetries, err)
        if i < maxRetries-1 {
            time.Sleep(5 * time.Second)
        }
    }
    return fmt.Errorf("failed to connect to API server after %d attempts", maxRetries)
}
