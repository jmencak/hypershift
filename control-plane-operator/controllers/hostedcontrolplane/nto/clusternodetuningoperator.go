package nto

import (
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	"github.com/openshift/hypershift/support/config"
	"github.com/openshift/hypershift/support/metrics"
	"github.com/openshift/hypershift/support/util"
	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilpointer "k8s.io/utils/pointer"
)

const operatorName = "cluster-node-tuning-operator"

type Images struct {
	NodeTuningOperator string
	NodeTunedContainer string
}

type Params struct {
	ReleaseVersion          string
	AvailabilityProberImage string
	HostedClusterName       string
	Images                  Images
	OwnerRef                config.OwnerRef
	DeploymentConfig        config.DeploymentConfig
}

// TODO use official NTO image
func NewParams(hcp *hyperv1.HostedControlPlane, version string, images map[string]string, setDefaultSecurityContext bool) Params {
	p := Params{
		Images: Images{
			NodeTuningOperator: "quay.io/dagray/cluster-node-tuning-operator:hypershift-poc", //images["cluster-node-tuning-operator"],
			NodeTunedContainer: "quay.io/dagray/cluster-node-tuning-operator:hypershift-poc", //images["cluster-node-tuning-operator"],
		},
		ReleaseVersion: version,
		OwnerRef:       config.OwnerRefFrom(hcp),
	}

	p.DeploymentConfig.Scheduling.PriorityClass = config.DefaultPriorityClass
	p.DeploymentConfig.SetDefaults(hcp, nil, utilpointer.IntPtr(1))
	p.DeploymentConfig.SetRestartAnnotation(hcp.ObjectMeta)

	p.DeploymentConfig.SetDefaultSecurityContext = setDefaultSecurityContext
	p.HostedClusterName = hcp.Name

	return p
}

// TODO does NTO need any custom Role rules? default serviceaccount already works, \
// but should we ensure all needed rules are still explicit here?
// From management side, we should only need read/watch access to Secrets in the
// control plane namespace
func ReconcileRole(role *rbacv1.Role, ownerRef config.OwnerRef) error {
	ownerRef.ApplyTo(role)
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{corev1.SchemeGroupVersion.Group},
			Resources: []string{
				"events",
				"configmaps",
				"pods",
				"secrets",
				"services",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

// TODO do we need anything in here? default serviceaccount already has everything needed.
func ReconcileRoleBinding(rb *rbacv1.RoleBinding, ownerRef config.OwnerRef) error {
	ownerRef.ApplyTo(rb)
	rb.RoleRef = rbacv1.RoleRef{
		APIGroup: rbacv1.SchemeGroupVersion.Group,
		Kind:     "Role",
		Name:     manifests.ClusterNetworkOperatorRoleBinding("").Name,
	}
	rb.Subjects = []rbacv1.Subject{
		{
			Kind: "ServiceAccount",
			Name: "default",
		},
	}
	return nil
}

func ReconcileClusterNodeTuningOperatorMetricsService(svc *corev1.Service, ownerRef config.OwnerRef) error {
	ownerRef.ApplyTo(svc)

	svc.Annotations = map[string]string{
		"service.beta.openshift.io/serving-cert-secret-name": "node-tuning-operator-tls",
	}
	// The service is assigned a cluster IP when it is created.
	// This field is immutable as shown here: https://github.com/kubernetes/api/blob/62998e98c313b2ca15b1da278aa702bdd7b84cb0/core/v1/types.go#L4114-L4130
	// As such, to avoid an error when updating the object, only update the fields OLM specifies.
	svc.Labels = map[string]string{"name": "node-tuning-operator", "hypershift.openshift.io/control-plane-component": "olm-operator"}
	svc.Spec.Ports = []corev1.ServicePort{
		{
			Port:       60000,
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(60000),
		},
	}
	//svc.Spec.Type = olmOperatorMetricsServiceDeepCopy.Spec.Type
	svc.Spec.Selector = map[string]string{"name": operatorName}

	return nil
}

func ReconcileClusterNodeTuningOperatorServiceMonitor(sm *prometheusoperatorv1.ServiceMonitor, ownerRef config.OwnerRef, clusterID string, metricsSet metrics.MetricsSet) error {
	ownerRef.ApplyTo(sm)

	sm.Spec.Selector.MatchLabels = map[string]string{"name": "node-tuning-operator"}
	sm.Spec.NamespaceSelector = prometheusoperatorv1.NamespaceSelector{
		MatchNames: []string{sm.Namespace},
	}
	targetPort := intstr.FromString("60000")
	sm.Spec.Endpoints = []prometheusoperatorv1.Endpoint{
		{
			Interval:   "15s",
			TargetPort: &targetPort,
			Scheme:     "https",
			Path:       "/metrics",
			TLSConfig: &prometheusoperatorv1.TLSConfig{
				SafeTLSConfig: prometheusoperatorv1.SafeTLSConfig{
					ServerName: "node-tuning-operator",
					Cert: prometheusoperatorv1.SecretOrConfigMap{
						Secret: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "nto-metrics-client",
							},
							Key: "tls.crt",
						},
					},
					KeySecret: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "nto-metrics-client",
						},
						Key: "tls.key",
					},
					CA: prometheusoperatorv1.SecretOrConfigMap{
						Secret: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "nto-metrics-client",
							},
							Key: "ca.crt",
						},
					},
				},
			},
		},
	}

	util.ApplyClusterIDLabel(&sm.Spec.Endpoints[0], clusterID)

	return nil
}

func ReconcileDeployment(dep *appsv1.Deployment, params Params) error {
	params.OwnerRef.ApplyTo(dep)

	dep.Spec.Replicas = utilpointer.Int32(1)
	dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"name": operatorName}}
	dep.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	// TODO may be not needed?
	dep.Spec.Template.Annotations["target.workload.openshift.io/management"] = `{"effect": "PreferredDuringScheduling"}`
	if dep.Spec.Template.Labels == nil {
		dep.Spec.Template.Labels = map[string]string{}
	}
	dep.Spec.Template.Labels["name"] = operatorName

	ntoArgs := []string{
		"-v=2",
	}

	var ntoEnv []corev1.EnvVar

	// TODO KUBECONFIG location should be set in variable
	ntoEnv = append(ntoEnv, []corev1.EnvVar{
		{Name: "RELEASE_VERSION", Value: params.ReleaseVersion},
		{Name: "HYPERSHIFT", Value: "true"},
		{Name: "MY_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.namespace",
			},
		}},
		{Name: "WATCH_NAMESPACE", Value: "openshift-cluster-node-tuning-operator"},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.name",
			},
		}},
		{Name: "RESYNC_PERIOD", Value: "600"},
		{Name: "CLUSTER_NODE_TUNED_IMAGE", Value: params.Images.NodeTunedContainer},
		{Name: "KUBECONFIG", Value: "/etc/kubernetes/kubeconfig"},
	}...)

	// TODO PullIfNotPresent not PullAlways (useful for dev)
	dep.Spec.Template.Spec.Containers = []corev1.Container{{
		Command: []string{"cluster-node-tuning-operator"},
		Args:    ntoArgs,
		Env:     ntoEnv,
		Name:    operatorName,
		Image:   params.Images.NodeTuningOperator,
		Ports: []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: 60000},
		},
		ImagePullPolicy: corev1.PullAlways,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("50Mi"),
		}},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "node-tuning-operator-tls", MountPath: "/etc/secrets"},
			{Name: "trusted-ca", MountPath: "/var/run/configmaps/trusted-ca/"},
			{Name: "hosted-kubeconfig", MountPath: "/etc/kubernetes"},
			// TODO add {Name: "apiservice-cert", MountPath: "/apiserver.local.config/certificates"},

		},
	}}
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "node-tuning-operator-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "node-tuning-operator-tls"}}},
		{Name: "apiservice-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  "performance-addon-operator-webhook-cert",
			DefaultMode: utilpointer.Int32(420),
			Items: []corev1.KeyToPath{
				{Key: "tls.crt", Path: "apiserver.crt"},
				{Key: "tls.key", Path: "apiserver.key"},
			},
		}}},
		{Name: "trusted-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			Optional:             utilpointer.Bool(true),
			LocalObjectReference: corev1.LocalObjectReference{Name: "trusted-ca"},
			Items: []corev1.KeyToPath{
				{Key: "ca-bundle.crt", Path: "tls-ca-bundle.pem"},
			},
		}}},
		{Name: "hosted-kubeconfig", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: manifests.KASServiceKubeconfigSecret("").Name}}},
	}

	params.DeploymentConfig.ApplyTo(dep)
	return nil
}
