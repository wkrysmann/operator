// Copyright (c) 2019 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
package render

import (
	"fmt"
	"strings"

	oprv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/components"
	"github.com/tigera/operator/pkg/dns"
	rmeta "github.com/tigera/operator/pkg/render/common/meta"
	"github.com/tigera/operator/pkg/render/common/podsecuritycontext"
	"github.com/tigera/operator/pkg/render/common/secret"
	"gopkg.in/yaml.v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Manifest object variables
	DexNamespace  = "tigera-dex"
	DexObjectName = "tigera-dex"
	DexPort       = 5556
	// This is the secret containing just a cert that a client should mount in order to trust Dex.
	DexCertSecretName = "tigera-dex-tls-crt"
	// This is the secret that Dex mounts, containing a key and a cert.
	DexTLSSecretName = "tigera-dex-tls"

	// Constants related to Dex configurations
	DexClientId = "tigera-manager"

	// Common name to add to the Dex TLS secret.
	DexCNPattern = "tigera-dex.tigera-dex.svc.%s"
)

func Dex(
	pullSecrets []*corev1.Secret,
	openshift bool,
	installation *oprv1.InstallationSpec,
	dexConfig DexConfig,
	clusterDomain string,
) Component {

	return &dexComponent{
		dexConfig:     dexConfig,
		pullSecrets:   pullSecrets,
		openshift:     openshift,
		installation:  installation,
		connector:     dexConfig.Connector(),
		clusterDomain: clusterDomain,
	}
}

type dexComponent struct {
	dexConfig     DexConfig
	pullSecrets   []*corev1.Secret
	openshift     bool
	installation  *oprv1.InstallationSpec
	connector     map[string]interface{}
	image         string
	csrInitImage  string
	clusterDomain string
}

func (c *dexComponent) ResolveImages(is *oprv1.ImageSet) error {
	reg := c.installation.Registry
	path := c.installation.ImagePath
	var err error
	c.image, err = components.GetReference(components.ComponentDex, reg, path, is)

	var errMsgs []string
	if err != nil {
		errMsgs = append(errMsgs, err.Error())
	}

	if c.installation.CertificateManagement != nil {
		c.csrInitImage, err = ResolveCSRInitImage(c.installation, is)
		if err != nil {
			errMsgs = append(errMsgs, err.Error())
		}
	}

	if len(errMsgs) != 0 {
		return fmt.Errorf(strings.Join(errMsgs, ","))
	}
	return nil
}

func (*dexComponent) SupportedOSType() rmeta.OSType {
	return rmeta.OSTypeLinux
}

func (c *dexComponent) Objects() ([]client.Object, []client.Object) {
	objs := []client.Object{
		c.serviceAccount(),
		c.deployment(),
		c.service(),
		c.clusterRole(),
		c.clusterRoleBinding(),
		c.configMap(),
	}
	objs = append(objs, secret.ToRuntimeObjects(c.dexConfig.RequiredSecrets(rmeta.OperatorNamespace())...)...)
	objs = append(objs, c.dexConfig.CreateCertSecret())
	objs = append(objs, secret.ToRuntimeObjects(c.dexConfig.RequiredSecrets(DexNamespace)...)...)
	objs = append(objs, secret.ToRuntimeObjects(secret.CopyToNamespace(DexNamespace, c.pullSecrets...)...)...)

	if c.installation.CertificateManagement != nil {
		objs = append(objs, csrClusterRoleBinding(DexObjectName, DexNamespace))
	}

	return objs, nil
}

// Method to satisfy the Component interface.
func (c *dexComponent) Ready() bool {
	return true
}

func (c *dexComponent) serviceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{Kind: "ServiceAccount", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: DexObjectName, Namespace: DexNamespace},
	}
}

func (c *dexComponent) clusterRole() client.Object {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{Kind: "ClusterRole", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: DexObjectName,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"dex.coreos.com"},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{"apiextensions.k8s.io"},
				Resources: []string{"customresourcedefinitions"},
				Verbs:     []string{"create"},
			},
		},
	}
}

func (c *dexComponent) clusterRoleBinding() client.Object {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{Kind: "ClusterRoleBinding", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: DexObjectName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     DexObjectName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      DexObjectName,
				Namespace: DexNamespace,
			},
		},
	}
}

func (c *dexComponent) deployment() client.Object {
	var initContainers []corev1.Container
	if c.installation.CertificateManagement != nil {
		initContainers = append(initContainers, CreateCSRInitContainer(
			c.installation.CertificateManagement,
			c.csrInitImage,
			"tls",
			DexObjectName,
			corev1.TLSPrivateKeyKey,
			corev1.TLSCertKey,
			dns.GetServiceDNSNames(DexObjectName, DexNamespace, c.clusterDomain),
			DexNamespace))
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DexObjectName,
			Namespace: DexNamespace,
			Labels: map[string]string{
				"k8s-app": DexObjectName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"k8s-app": DexObjectName,
				},
			},
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      DexObjectName,
					Namespace: DexNamespace,
					Labels: map[string]string{
						"k8s-app": DexObjectName,
					},
					Annotations: c.dexConfig.RequiredAnnotations(),
				},
				Spec: corev1.PodSpec{
					NodeSelector:       c.installation.ControlPlaneNodeSelector,
					ServiceAccountName: DexObjectName,
					Tolerations:        append(c.installation.ControlPlaneTolerations, rmeta.TolerateMaster),
					ImagePullSecrets:   secret.GetReferenceList(c.pullSecrets),
					InitContainers:     initContainers,
					Containers: []corev1.Container{
						{
							Name:            DexObjectName,
							Image:           c.image,
							Env:             c.dexConfig.RequiredEnv(""),
							LivenessProbe:   c.probe(),
							SecurityContext: podsecuritycontext.NewBaseContext(),

							Command: []string{"/usr/local/bin/dex", "serve", "/etc/dex/baseCfg/config.yaml"},

							Ports: []corev1.ContainerPort{
								{
									Name:          "https",
									ContainerPort: DexPort,
								},
							},

							VolumeMounts: c.dexConfig.RequiredVolumeMounts(),
						},
					},
					Volumes: c.dexConfig.RequiredVolumes(),
				},
			},
		},
	}
}

func (c *dexComponent) service() client.Object {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{Kind: "Service", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DexObjectName,
			Namespace: DexNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"k8s-app": DexObjectName,
			},
			Ports: []corev1.ServicePort{
				{
					Name: DexObjectName,
					Port: DexPort,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: DexPort,
					},
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

// Perform a HTTP GET to determine if an endpoint is available.
func (c *dexComponent) probe() *corev1.Probe {
	return &corev1.Probe{
		Handler: corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/dex/.well-known/openid-configuration",
				Port:   intstr.FromInt(DexPort),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 90,
		PeriodSeconds:       10,
	}
}

func (c *dexComponent) configMap() *corev1.ConfigMap {
	redirectURIs := []string{
		"https://localhost:9443/login/oidc/callback",
		"https://127.0.0.1:9443/login/oidc/callback",
		"https://localhost:9443/tigera-kibana/api/security/oidc/callback",
		"https://127.0.0.1:9443/tigera-kibana/api/security/oidc/callback",
	}
	host := c.dexConfig.ManagerURI()
	if host != "" && !strings.Contains(host, "localhost") && !strings.Contains(host, "127.0.0.1") {
		redirectURIs = append(redirectURIs, fmt.Sprintf("%s/login/oidc/callback", host))
		redirectURIs = append(redirectURIs, fmt.Sprintf("%s/tigera-kibana/api/security/oidc/callback", host))
	}

	data := map[string]interface{}{
		"issuer": fmt.Sprintf("%s/dex", c.dexConfig.ManagerURI()),
		"storage": map[string]interface{}{
			"type": "kubernetes",
			"config": map[string]bool{
				"inCluster": true,
			},
		},
		"web": map[string]interface{}{
			"https":                   "0.0.0.0:5556",
			"tlsCert":                 "/etc/dex/tls/tls.crt",
			"tlsKey":                  "/etc/dex/tls/tls.key",
			"allowedOrigins":          []string{"*"},
			"discoveryAllowedOrigins": []string{"*"},
		},
		"connectors": []map[string]interface{}{c.connector},
		"oauth2": map[string]interface{}{
			"skipApprovalScreen": true,
			"responseTypes":      []string{"id_token", "code", "token"},
		},
		"staticClients": []map[string]interface{}{
			{
				"id":           DexClientId,
				"redirectURIs": redirectURIs,
				"name":         "Calico Enterprise Manager",
				"secretEnv":    dexSecretEnv,
			},
		},
	}

	bytes, err := yaml.Marshal(data)
	if err != nil { // Don't think this is possible.
		panic(err)
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DexObjectName,
			Namespace: DexNamespace,
		},
		Data: map[string]string{
			"config.yaml": string(bytes),
		},
	}
}
