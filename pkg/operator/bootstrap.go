package operator

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/golang/glog"

	configv1 "github.com/openshift/api/config/v1"
	configscheme "github.com/openshift/client-go/config/clientset/versioned/scheme"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	templatectrl "github.com/openshift/machine-config-operator/pkg/controller/template"
)

type manifest struct {
	name     string
	data     []byte
	filename string
}

// RenderBootstrap writes to destinationDir static Pods.
func RenderBootstrap(
	additionalTrustBundleFile,
	proxyFile,
	clusterConfigConfigMapFile,
	infraFile, networkFile, dnsFile,
	cloudConfigFile, cloudProviderCAFile,
	rootCAFile, kubeAPIServerServingCA, pullSecretFile string,
	imgs *Images,
	destinationDir, releaseImage string,
) error {
	filesData := map[string][]byte{}
	files := []string{
		proxyFile,
		clusterConfigConfigMapFile,
		infraFile,
		networkFile,
		rootCAFile,
		pullSecretFile,
		dnsFile,
	}
	if kubeAPIServerServingCA != "" {
		files = append(files, kubeAPIServerServingCA)
	}
	if cloudProviderCAFile != "" {
		files = append(files, cloudProviderCAFile)
	}
	for _, file := range files {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			return err
		}
		filesData[file] = data
	}

	// create ControllerConfigSpec
	obji, err := runtime.Decode(configscheme.Codecs.UniversalDecoder(configv1.SchemeGroupVersion), filesData[infraFile])
	if err != nil {
		return err
	}
	infra, ok := obji.(*configv1.Infrastructure)
	if !ok {
		return fmt.Errorf("expected *configv1.Infrastructure found %T", obji)
	}

	obji, err = runtime.Decode(configscheme.Codecs.UniversalDecoder(configv1.SchemeGroupVersion), filesData[proxyFile])
	if err != nil {
		return err
	}
	proxy, ok := obji.(*configv1.Proxy)
	if !ok {
		return fmt.Errorf("expected *configv1.Proxy found %T", obji)
	}

	obji, err = runtime.Decode(configscheme.Codecs.UniversalDecoder(configv1.SchemeGroupVersion), filesData[networkFile])
	if err != nil {
		return err
	}
	network, ok := obji.(*configv1.Network)
	if !ok {
		return fmt.Errorf("expected *configv1.Network found %T", obji)
	}

	obji, err = runtime.Decode(configscheme.Codecs.UniversalDecoder(configv1.SchemeGroupVersion), filesData[dnsFile])
	if err != nil {
		return err
	}
	dns, ok := obji.(*configv1.DNS)
	if !ok {
		return fmt.Errorf("expected *configv1.DNS found %T", obji)
	}

	spec, err := createDiscoveredControllerConfigSpec(infra, network, proxy, dns)
	if err != nil {
		return err
	}

	additionalTrustBundleData, err := ioutil.ReadFile(additionalTrustBundleFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if additionalTrustBundleData != nil {
		obji, err := runtime.Decode(scheme.Codecs.UniversalDecoder(corev1.SchemeGroupVersion), additionalTrustBundleData)
		if err != nil {
			return err
		}
		additionalTrustBundle, ok := obji.(*corev1.ConfigMap)
		if !ok {
			return fmt.Errorf("expected *corev1.ConfigMap found %T", obji)
		}
		spec.AdditionalTrustBundle = []byte(additionalTrustBundle.Data["ca-bundle.crt"])
	}

	// if the cloudConfig is set in infra read the cloudConfigFile
	if infra.Spec.CloudConfig.Name != "" {
		cloudConf, err := loadBootstrapCloudProviderConfig(infra, cloudConfigFile)
		if err != nil {
			return fmt.Errorf("failed to load the cloud provider config: %v", err)
		}
		spec.CloudProviderConfig = cloudConf
	}

	bundle := make([]byte, 0)
	bundle = append(bundle, filesData[rootCAFile]...)
	// Append the kube-ca if given.
	if _, ok := filesData[kubeAPIServerServingCA]; ok {
		bundle = append(bundle, filesData[kubeAPIServerServingCA]...)
		spec.KubeAPIServerServingCAData = filesData[kubeAPIServerServingCA]
	}
	// Set the cloud-provider CA if given.
	if data, ok := filesData[cloudProviderCAFile]; ok {
		spec.CloudProviderCAData = data
	}

	spec.RootCAData = bundle
	spec.PullSecret = nil
	spec.OSImageURL = imgs.MachineOSContent
	spec.ReleaseImage = releaseImage
	spec.Images = map[string]string{
		templatectrl.MachineConfigOperatorKey: imgs.MachineConfigOperator,

		templatectrl.APIServerWatcherKey:    imgs.MachineConfigOperator,
		templatectrl.InfraImageKey:          imgs.InfraImage,
		templatectrl.KeepalivedKey:          imgs.Keepalived,
		templatectrl.CorednsKey:             imgs.Coredns,
		templatectrl.MdnsPublisherKey:       imgs.MdnsPublisher,
		templatectrl.HaproxyKey:             imgs.Haproxy,
		templatectrl.BaremetalRuntimeCfgKey: imgs.BaremetalRuntimeCfg,
	}

	config := getRenderConfig("", string(filesData[kubeAPIServerServingCA]), spec, &imgs.RenderConfigImages, infra.Status.APIServerInternalURL)

	manifests := []manifest{
		{
			name:     "manifests/machineconfigcontroller/controllerconfig.yaml",
			filename: "bootstrap/manifests/machineconfigcontroller-controllerconfig.yaml",
		}, {
			name:     "manifests/master.machineconfigpool.yaml",
			filename: "bootstrap/manifests/master.machineconfigpool.yaml",
		}, {
			name:     "manifests/worker.machineconfigpool.yaml",
			filename: "bootstrap/manifests/worker.machineconfigpool.yaml",
		}, {
			name:     "manifests/bootstrap-pod-v2.yaml",
			filename: "bootstrap/machineconfigoperator-bootstrap-pod.yaml",
		}, {
			data:     filesData[pullSecretFile],
			filename: "bootstrap/manifests/machineconfigcontroller-pull-secret",
		}, {
			name:     "manifests/machineconfigserver/csr-bootstrap-role-binding.yaml",
			filename: "manifests/csr-bootstrap-role-binding.yaml",
		}, {
			name:     "manifests/machineconfigserver/kube-apiserver-serving-ca-configmap.yaml",
			filename: "manifests/kube-apiserver-serving-ca-configmap.yaml",
		}}

	manifests = appendManifestsByPlatform(manifests, *infra)

	for _, m := range manifests {
		var b []byte
		var err error
		switch {
		case len(m.name) > 0:
			glog.Info(m.name)
			b, err = renderAsset(config, m.name)
			if err != nil {
				return err
			}
		case len(m.data) > 0:
			b = m.data
		default:
			continue
		}

		path := filepath.Join(destinationDir, m.filename)
		dirname := filepath.Dir(path)
		if err := os.MkdirAll(dirname, 0655); err != nil {
			return err
		}
		if err := ioutil.WriteFile(path, b, 0644); err != nil {
			return err
		}
	}
	return nil
}

func appendManifestsByPlatform(manifests []manifest, infra configv1.Infrastructure) []manifest {

	if infra.Status.PlatformStatus.BareMetal != nil {
		manifests = append(manifests,
			manifest{
				name:     "manifests/on-prem/coredns.yaml",
				filename: "baremetal/manifests/coredns.yaml",
			},
			manifest{
				name:     "manifests/on-prem/coredns-corefile.tmpl",
				filename: "baremetal/static-pod-resources/coredns/Corefile.tmpl",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.yaml",
				filename: "baremetal/manifests/keepalived.yaml",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.conf.tmpl",
				filename: "baremetal/static-pod-resources/keepalived/keepalived.conf.tmpl",
			},
		)
	}

	if infra.Status.PlatformStatus.OpenStack != nil {
		manifests = append(manifests,
			manifest{
				name:     "manifests/on-prem/coredns.yaml",
				filename: "openstack/manifests/coredns.yaml",
			},
			manifest{
				name:     "manifests/on-prem/coredns-corefile.tmpl",
				filename: "openstack/static-pod-resources/coredns/Corefile.tmpl",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.yaml",
				filename: "openstack/manifests/keepalived.yaml",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.conf.tmpl",
				filename: "openstack/static-pod-resources/keepalived/keepalived.conf.tmpl",
			},
		)
	}

	if infra.Status.PlatformStatus.Ovirt != nil {
		manifests = append(manifests,
			manifest{
				name:     "manifests/on-prem/coredns.yaml",
				filename: "ovirt/manifests/coredns.yaml",
			},
			manifest{
				name:     "manifests/on-prem/coredns-corefile.tmpl",
				filename: "ovirt/static-pod-resources/coredns/Corefile.tmpl",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.yaml",
				filename: "ovirt/manifests/keepalived.yaml",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.conf.tmpl",
				filename: "ovirt/static-pod-resources/keepalived/keepalived.conf.tmpl",
			},
		)
	}

	if infra.Status.PlatformStatus.Kubevirt != nil {
		manifests = append(manifests,
			manifest{
				name:     "manifests/on-prem/coredns.yaml",
				filename: "kubevirt/manifests/coredns.yaml",
			},
			manifest{
				name:     "manifests/on-prem/coredns-corefile.tmpl",
				filename: "kubevirt/static-pod-resources/coredns/Corefile.tmpl",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.yaml",
				filename: "kubevirt/manifests/keepalived.yaml",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.conf.tmpl",
				filename: "kubevirt/static-pod-resources/keepalived/keepalived.conf.tmpl",
			},
		)
	}

	if infra.Status.PlatformStatus.VSphere != nil && infra.Status.PlatformStatus.VSphere.APIServerInternalIP != "" {
		manifests = append(manifests,
			manifest{
				name:     "manifests/on-prem/coredns.yaml",
				filename: "vsphere/manifests/coredns.yaml",
			},
			manifest{
				name:     "manifests/on-prem/coredns-corefile.tmpl",
				filename: "vsphere/static-pod-resources/coredns/Corefile.tmpl",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.yaml",
				filename: "vsphere/manifests/keepalived.yaml",
			},
			manifest{
				name:     "manifests/on-prem/keepalived.conf.tmpl",
				filename: "vsphere/static-pod-resources/keepalived/keepalived.conf.tmpl",
			},
		)
	}

	return manifests
}

// loadBootstrapCloudProviderConfig reads the cloud provider config from cloudConfigFile based on infra object.
func loadBootstrapCloudProviderConfig(infra *configv1.Infrastructure, cloudConfigFile string) (string, error) {
	data, err := ioutil.ReadFile(cloudConfigFile)
	if err != nil {
		return "", err
	}
	obji, err := runtime.Decode(scheme.Codecs.UniversalDecoder(corev1.SchemeGroupVersion), data)
	if err != nil {
		return "", err
	}
	cm, ok := obji.(*corev1.ConfigMap)
	if !ok {
		return "", fmt.Errorf("expected *corev1.ConfigMap found %T", obji)
	}
	cloudConf, ok := cm.Data["cloud.conf"]
	if !ok {
		glog.Infof("falling back to reading cloud provider config from user specified key %s", infra.Spec.CloudConfig.Key)
		cloudConf = cm.Data[infra.Spec.CloudConfig.Key]
	}
	return cloudConf, nil
}
