package controller

import (
	"context"
	"fmt"

	mcpv1alpha1 "github.com/Kuadrant/mcp-gateway/api/v1alpha1"
	consolev1 "github.com/openshift/api/console/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// console plugin constants
	consolePluginName         = "mcp-gateway-console-plugin"
	consolePluginPort         = 9443
	consolePluginReplicas     = 1
	DefaultConsolePluginImage = "ghcr.io/kuadrant/mcp-gateway-console-plugin:latest"
)

func consolePluginLabels() map[string]string {
	return map[string]string{
		labelAppName:   consolePluginName,
		labelManagedBy: labelManagedByValue,
	}
}

// reconcileConsolePlugin ensures the console plugin resources exist
// Console plugin is deployed once per cluster (singleton pattern)
func (r *MCPGatewayExtensionReconciler) reconcileConsolePlugin(ctx context.Context, mcpExt *mcpv1alpha1.MCPGatewayExtension, targetGateway *gatewayv1.Gateway, listenerConfig *mcpv1alpha1.ListenerConfig) error {
	// build gateway service name following Gateway API pattern: <gateway-name>-<gateway-class-name>
	gatewayServiceName := fmt.Sprintf("%s-%s", targetGateway.Name, targetGateway.Spec.GatewayClassName)

	// reconcile nginx configmap
	nginxConfigMap := r.buildConsolePluginNginxConfigMap(mcpExt, listenerConfig, gatewayServiceName, targetGateway.Namespace)
	if err := r.ensureConsolePluginResource(ctx, mcpExt, nginxConfigMap); err != nil {
		return fmt.Errorf("failed to reconcile nginx configmap: %w", err)
	}

	// reconcile service account
	serviceAccount := r.buildConsolePluginServiceAccount(mcpExt)
	if err := r.ensureConsolePluginResource(ctx, mcpExt, serviceAccount); err != nil {
		return fmt.Errorf("failed to reconcile service account: %w", err)
	}

	// reconcile deployment
	deployment := r.buildConsolePluginDeployment(mcpExt)
	if err := r.ensureConsolePluginResource(ctx, mcpExt, deployment); err != nil {
		return fmt.Errorf("failed to reconcile deployment: %w", err)
	}

	// reconcile service
	service := r.buildConsolePluginService(mcpExt)
	if err := r.ensureConsolePluginResource(ctx, mcpExt, service); err != nil {
		return fmt.Errorf("failed to reconcile service: %w", err)
	}

	// reconcile ConsolePlugin CR (cluster-scoped - no owner reference)
	consolePlugin := r.buildConsolePluginCR(mcpExt)
	if err := r.ensureClusterScopedResource(ctx, consolePlugin); err != nil {
		return fmt.Errorf("failed to reconcile ConsolePlugin CR: %w", err)
	}

	return nil
}

// ensureClusterScopedResource creates or updates a cluster-scoped console plugin resource
// Cluster-scoped resources can't have owner references to namespace-scoped resources,
// so we just ensure they exist and track them via labels instead
func (r *MCPGatewayExtensionReconciler) ensureClusterScopedResource(ctx context.Context, obj client.Object) error {
	existing := obj.DeepCopyObject().(client.Object)
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Info("creating cluster-scoped console plugin resource", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName())
			return r.Create(ctx, obj)
		}
		return fmt.Errorf("failed to get resource: %w", err)
	}

	// resource exists - nothing to update for now
	return nil
}

// ensureConsolePluginResource creates or updates a console plugin resource
func (r *MCPGatewayExtensionReconciler) ensureConsolePluginResource(ctx context.Context, mcpExt *mcpv1alpha1.MCPGatewayExtension, obj client.Object) error {
	// set owner reference (but not controller reference - allows multiple MCPGatewayExtensions)
	// this ensures the resource is cleaned up when the last MCPGatewayExtension is deleted
	if err := controllerutil.SetOwnerReference(mcpExt, obj, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := obj.DeepCopyObject().(client.Object)
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Info("creating console plugin resource", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
			return r.Create(ctx, obj)
		}
		return fmt.Errorf("failed to get resource: %w", err)
	}

	// resource exists - update owner references to include this MCPGatewayExtension
	ownerRefs := existing.GetOwnerReferences()
	found := false
	for _, ref := range ownerRefs {
		if ref.UID == mcpExt.UID {
			found = true
			break
		}
	}

	if !found {
		r.log.Info("adding owner reference to existing console plugin resource", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName())
		ownerRefs = append(ownerRefs, metav1.OwnerReference{
			APIVersion: mcpExt.APIVersion,
			Kind:       mcpExt.Kind,
			Name:       mcpExt.Name,
			UID:        mcpExt.UID,
		})
		existing.SetOwnerReferences(ownerRefs)
		return r.Update(ctx, existing)
	}

	return nil
}

func (r *MCPGatewayExtensionReconciler) buildConsolePluginNginxConfigMap(mcpExt *mcpv1alpha1.MCPGatewayExtension, listenerConfig *mcpv1alpha1.ListenerConfig, gatewayServiceName, gatewayNamespace string) *corev1.ConfigMap {
	labels := consolePluginLabels()

	// determine gateway hostname for Host header - use publicHost if set, otherwise listener hostname
	gatewayHostname := mcpExt.Spec.PublicHost
	if gatewayHostname == "" {
		gatewayHostname = listenerConfig.Hostname
	}

	// build internal gateway service URL for MCP requests (goes through ext_proc)
	// use internal service name so nginx can reach it from inside the cluster
	gatewayURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/mcp", gatewayServiceName, gatewayNamespace, listenerConfig.Port)

	nginxConf := fmt.Sprintf(`error_log /dev/stdout info;
events {}
http {
  access_log /dev/stdout;
  include /etc/nginx/mime.types;
  default_type application/octet-stream;
  server {
    listen %d ssl;
    ssl_certificate /var/serving-cert/tls.crt;
    ssl_certificate_key /var/serving-cert/tls.key;
    root /usr/share/nginx/html;

    # proxy status to broker service (for server list)
    location /status {
      proxy_pass http://mcp-gateway.%s.svc.cluster.local:8080/status;
      proxy_set_header Host $host;
      proxy_set_header X-Real-IP $remote_addr;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto $scheme;
    }

    # proxy MCP requests to Gateway service (goes through ext_proc for session management)
    location /mcp {
      proxy_pass %s;
      proxy_set_header Host %s;
      proxy_set_header X-Real-IP $remote_addr;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto $scheme;
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection "upgrade";
      proxy_buffering off;
      proxy_cache off;
    }

    location / {
      try_files $uri $uri/ /index.html;
    }
    location /plugin-manifest.json {
      default_type application/json;
    }
  }
}`, consolePluginPort, mcpExt.Namespace, gatewayURL, gatewayHostname)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      consolePluginName + "-nginx",
			Namespace: mcpExt.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"nginx.conf": nginxConf,
		},
	}
}

func (r *MCPGatewayExtensionReconciler) buildConsolePluginServiceAccount(mcpExt *mcpv1alpha1.MCPGatewayExtension) *corev1.ServiceAccount {
	labels := consolePluginLabels()

	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      consolePluginName,
			Namespace: mcpExt.Namespace,
			Labels:    labels,
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

func (r *MCPGatewayExtensionReconciler) buildConsolePluginDeployment(mcpExt *mcpv1alpha1.MCPGatewayExtension) *appsv1.Deployment {
	labels := consolePluginLabels()
	replicas := int32(consolePluginReplicas)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      consolePluginName,
			Namespace: mcpExt.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: consolePluginName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "console-plugin",
							Image:           r.ConsolePluginImage,
							ImagePullPolicy: corev1.PullAlways,
							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: consolePluginPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("50Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "plugin-serving-cert",
									MountPath: "/var/serving-cert",
									ReadOnly:  true,
								},
								{
									Name:      "nginx-conf",
									MountPath: "/etc/nginx/nginx.conf",
									SubPath:   "nginx.conf",
									ReadOnly:  true,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/",
										Port:   intstr.FromInt(consolePluginPort),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/",
										Port:   intstr.FromInt(consolePluginPort),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       30,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "plugin-serving-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  consolePluginName + "-cert",
									DefaultMode: ptr.To[int32](420),
								},
							},
						},
						{
							Name: "nginx-conf",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: consolePluginName + "-nginx",
									},
									DefaultMode: ptr.To[int32](420),
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *MCPGatewayExtensionReconciler) buildConsolePluginService(mcpExt *mcpv1alpha1.MCPGatewayExtension) *corev1.Service {
	labels := consolePluginLabels()

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      consolePluginName,
			Namespace: mcpExt.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": consolePluginName + "-cert",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "https",
					Port:       consolePluginPort,
					TargetPort: intstr.FromInt(consolePluginPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: labels,
		},
	}
}

func (r *MCPGatewayExtensionReconciler) buildConsolePluginCR(mcpExt *mcpv1alpha1.MCPGatewayExtension) *consolev1.ConsolePlugin {
	labels := consolePluginLabels()

	return &consolev1.ConsolePlugin{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "console.openshift.io/v1",
			Kind:       "ConsolePlugin",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   consolePluginName,
			Labels: labels,
		},
		Spec: consolev1.ConsolePluginSpec{
			DisplayName: "MCP Gateway Plugin",
			I18n: consolev1.ConsolePluginI18n{
				LoadType: consolev1.Preload,
			},
			Backend: consolev1.ConsolePluginBackend{
				Type: consolev1.Service,
				Service: &consolev1.ConsolePluginService{
					Name:      consolePluginName,
					Namespace: mcpExt.Namespace,
					Port:      consolePluginPort,
					BasePath:  "/",
				},
			},
			Proxy: []consolev1.ConsolePluginProxy{
				{
					Alias:         "broker",
					Authorization: consolev1.UserToken,
					Endpoint: consolev1.ConsolePluginProxyEndpoint{
						Type: consolev1.ProxyTypeService,
						Service: &consolev1.ConsolePluginProxyServiceConfig{
							Name:      consolePluginName,
							Namespace: mcpExt.Namespace,
							Port:      consolePluginPort,
						},
					},
				},
			},
		},
	}
}
