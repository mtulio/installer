package manifests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/openshift/installer/cmd/openshift-install/command"
	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/installconfig"
	installconfigaws "github.com/openshift/installer/pkg/asset/installconfig/aws"
	"github.com/openshift/installer/pkg/asset/installconfig/gcp"
	"github.com/openshift/installer/pkg/asset/installconfig/ibmcloud"
	"github.com/openshift/installer/pkg/asset/installconfig/ovirt"
	"github.com/openshift/installer/pkg/asset/machines"
	osmachine "github.com/openshift/installer/pkg/asset/machines/openstack"
	openstackmanifests "github.com/openshift/installer/pkg/asset/manifests/openstack"
	"github.com/openshift/installer/pkg/asset/openshiftinstall"
	"github.com/openshift/installer/pkg/asset/password"
	"github.com/openshift/installer/pkg/asset/rhcos"
	"github.com/openshift/installer/pkg/asset/templates/content/openshift"
	"github.com/openshift/installer/pkg/infrastructure/external/payload"
	"github.com/openshift/installer/pkg/types"
	awstypes "github.com/openshift/installer/pkg/types/aws"
	azuretypes "github.com/openshift/installer/pkg/types/azure"
	baremetaltypes "github.com/openshift/installer/pkg/types/baremetal"
	externaltypes "github.com/openshift/installer/pkg/types/external"
	gcptypes "github.com/openshift/installer/pkg/types/gcp"
	ibmcloudtypes "github.com/openshift/installer/pkg/types/ibmcloud"
	openstacktypes "github.com/openshift/installer/pkg/types/openstack"
	ovirttypes "github.com/openshift/installer/pkg/types/ovirt"
	powervctypes "github.com/openshift/installer/pkg/types/powervc"
	vspheretypes "github.com/openshift/installer/pkg/types/vsphere"
)

const (
	openshiftManifestDir = "openshift"
)

var (
	_ asset.WritableAsset = (*Openshift)(nil)
)

// Openshift generates the dependent resource manifests for openShift (as against bootkube)
type Openshift struct {
	FileList []*asset.File
}

// Name returns a human friendly name for the operator
func (o *Openshift) Name() string {
	return "Openshift Manifests"
}

// Dependencies returns all of the dependencies directly needed by the
// Openshift asset
func (o *Openshift) Dependencies() []asset.Asset {
	return []asset.Asset{
		&installconfig.InstallConfig{},
		&installconfig.ClusterID{},
		&password.KubeadminPassword{},
		&openshiftinstall.Config{},
		&FeatureGate{},

		&openshift.CloudCredsSecret{},
		&openshift.KubeadminPasswordSecret{},
		&openshift.RoleCloudCredsSecretReader{},
		&openshift.BaremetalConfig{},
		new(rhcos.Image),
		&openshift.AzureCloudProviderSecret{},
		&OSImageStream{},
		&ImageRegistryConfig{},
	}
}

// Generate generates the respective operator config.yml files
//
//nolint:gocyclo
func (o *Openshift) Generate(ctx context.Context, dependencies asset.Parents) error {
	installConfig := &installconfig.InstallConfig{}
	clusterID := &installconfig.ClusterID{}
	kubeadminPassword := &password.KubeadminPassword{}
	openshiftInstall := &openshiftinstall.Config{}
	featureGate := &FeatureGate{}
	imageRegistryConfig := &ImageRegistryConfig{}
	dependencies.Get(installConfig, kubeadminPassword, clusterID, openshiftInstall, featureGate, imageRegistryConfig)
	var cloudCreds cloudCredsSecretData
	platform := installConfig.Config.Platform.Name()
	switch platform {
	case awstypes.Name:
		awsconfig, err := installconfigaws.GetConfigWithOptions(ctx, config.WithRegion(installConfig.AWS.Region))
		if err != nil {
			return err
		}

		creds, err := awsconfig.Credentials.Retrieve(ctx)
		if err != nil {
			return fmt.Errorf("failed to retrieve aws credentials: %w", err)
		}

		if !installconfigaws.IsStaticCredentials(creds) {
			switch {
			case installConfig.Config.CredentialsMode == "":
				return errors.Errorf("AWS credentials provided by %s are not valid for default credentials mode", creds.Source)
			case installConfig.Config.CredentialsMode != types.ManualCredentialsMode:
				return errors.Errorf("AWS credentials provided by %s are not valid for %s credentials mode", creds.Source, installConfig.Config.CredentialsMode)
			}
		}
		cloudCreds = cloudCredsSecretData{
			AWS: &AwsCredsSecretData{
				Base64encodeAccessKeyID:     base64.StdEncoding.EncodeToString([]byte(creds.AccessKeyID)),
				Base64encodeSecretAccessKey: base64.StdEncoding.EncodeToString([]byte(creds.SecretAccessKey)),
			},
		}
	case azuretypes.Name:
		resourceGroupName := installConfig.Config.Azure.ClusterResourceGroupName(clusterID.InfraID)
		session, err := installConfig.Azure.Session()
		if err != nil {
			return err
		}
		creds := session.Credentials
		cloudCreds = cloudCredsSecretData{
			Azure: &AzureCredsSecretData{
				Base64encodeSubscriptionID: base64.StdEncoding.EncodeToString([]byte(creds.SubscriptionID)),
				Base64encodeClientID:       base64.StdEncoding.EncodeToString([]byte(creds.ClientID)),
				Base64encodeClientSecret:   base64.StdEncoding.EncodeToString([]byte(creds.ClientSecret)),
				Base64encodeTenantID:       base64.StdEncoding.EncodeToString([]byte(creds.TenantID)),
				Base64encodeResourcePrefix: base64.StdEncoding.EncodeToString([]byte(clusterID.InfraID)),
				Base64encodeResourceGroup:  base64.StdEncoding.EncodeToString([]byte(resourceGroupName)),
				Base64encodeRegion:         base64.StdEncoding.EncodeToString([]byte(installConfig.Config.Azure.Region)),
			},
		}
	case gcptypes.Name:
		session, err := gcp.GetSession(ctx)
		if err != nil {
			return err
		}
		creds := session.Credentials.JSON
		cloudCreds = cloudCredsSecretData{
			GCP: &GCPCredsSecretData{
				Base64encodeServiceAccount: base64.StdEncoding.EncodeToString(creds),
			},
		}
	case ibmcloudtypes.Name:
		client, err := ibmcloud.NewClient(installConfig.Config.Platform.IBMCloud.ServiceEndpoints)
		if err != nil {
			return err
		}
		cloudCreds = cloudCredsSecretData{
			IBMCloud: &IBMCloudCredsSecretData{
				Base64encodeAPIKey: base64.StdEncoding.EncodeToString([]byte(client.GetAPIKey())),
			},
		}
	case openstacktypes.Name, powervctypes.Name:
		opts := new(clientconfig.ClientOpts)
		opts.Cloud = installConfig.Config.Platform.OpenStack.Cloud
		cloud, err := clientconfig.GetCloudFromYAML(opts)
		if err != nil {
			return err
		}

		var caCert []byte
		if cloud.CACertFile != "" {
			var err error
			caCert, err = os.ReadFile(cloud.CACertFile)
			if err != nil {
				return err
			}
			// We need to replace the local cacert path with one that is used in OpenShift
			cloud.CACertFile = "/etc/kubernetes/static-pod-resources/configmaps/cloud-config/ca-bundle.pem"
		}

		// Application credentials are easily rotated in the event of a leak and should be preferred. Encourage their use.
		authTypes := sets.New(clientconfig.AuthPassword, clientconfig.AuthV2Password, clientconfig.AuthV3Password)
		if cloud.AuthInfo != nil && authTypes.Has(cloud.AuthType) {
			logrus.Warnf(
				"clouds.yaml file is using %q type auth. Consider using the %q auth type instead to rotate credentials more easily.",
				cloud.AuthType,
				clientconfig.AuthV3ApplicationCredential,
			)
		}

		clouds := make(map[string]map[string]*clientconfig.Cloud)
		clouds["clouds"] = map[string]*clientconfig.Cloud{
			osmachine.CloudName: cloud,
		}

		marshalled, err := yaml.Marshal(clouds)
		if err != nil {
			return err
		}

		cloudProviderConf, err := openstackmanifests.CloudProviderConfigSecret(cloud)
		if err != nil {
			return err
		}

		credsEncoded := base64.StdEncoding.EncodeToString(marshalled)
		cloudProviderConfEncoded := base64.StdEncoding.EncodeToString(cloudProviderConf)
		caCertEncoded := base64.StdEncoding.EncodeToString(caCert)
		cloudCreds = cloudCredsSecretData{
			OpenStack: &OpenStackCredsSecretData{
				Base64encodeCloudsYAML: credsEncoded,
				Base64encodeCloudsConf: cloudProviderConfEncoded,
				Base64encodeCACert:     caCertEncoded,
			},
		}
	case vspheretypes.Name:
		vsphereCredList := make([]*VSphereCredsSecretData, 0)

		for _, vCenter := range installConfig.Config.VSphere.VCenters {
			vsphereCred := VSphereCredsSecretData{
				VCenter:              vCenter.Server,
				Base64encodeUsername: base64.StdEncoding.EncodeToString([]byte(vCenter.Username)),
				Base64encodePassword: base64.StdEncoding.EncodeToString([]byte(vCenter.Password)),
			}
			vsphereCredList = append(vsphereCredList, &vsphereCred)
		}

		cloudCreds = cloudCredsSecretData{
			VSphere: &vsphereCredList,
		}
	case ovirttypes.Name:
		conf, err := ovirt.NewConfig()
		if err != nil {
			return err
		}

		if len(conf.CABundle) == 0 && len(conf.CAFile) > 0 {
			content, err := os.ReadFile(conf.CAFile)
			if err != nil {
				return errors.Wrapf(err, "failed to read the cert file: %s", conf.CAFile)
			}
			conf.CABundle = strings.TrimSpace(string(content))
		}

		cloudCreds = cloudCredsSecretData{
			Ovirt: &OvirtCredsSecretData{
				Base64encodeURL:      base64.StdEncoding.EncodeToString([]byte(conf.URL)),
				Base64encodeUsername: base64.StdEncoding.EncodeToString([]byte(conf.Username)),
				Base64encodePassword: base64.StdEncoding.EncodeToString([]byte(conf.Password)),
				Base64encodeInsecure: base64.StdEncoding.EncodeToString([]byte(strconv.FormatBool(conf.Insecure))),
				Base64encodeCABundle: base64.StdEncoding.EncodeToString([]byte(conf.CABundle)),
			},
		}
	}

	templateData := &openshiftTemplateData{
		CloudCreds:                   cloudCreds,
		Base64EncodedKubeadminPwHash: base64.StdEncoding.EncodeToString(kubeadminPassword.PasswordHash),
	}

	cloudCredsSecret := &openshift.CloudCredsSecret{}
	kubeadminPasswordSecret := &openshift.KubeadminPasswordSecret{}
	roleCloudCredsSecretReader := &openshift.RoleCloudCredsSecretReader{}
	baremetalConfig := &openshift.BaremetalConfig{}
	rhcosImage := new(rhcos.Image)
	osImageStream := &OSImageStream{}

	dependencies.Get(
		cloudCredsSecret,
		kubeadminPasswordSecret,
		roleCloudCredsSecretReader,
		baremetalConfig,
		rhcosImage,
		osImageStream)

	assetData := map[string][]byte{
		"99_kubeadmin-password-secret.yaml": applyTemplateData(kubeadminPasswordSecret.Files()[0].Data, templateData),
	}

	switch platform {
	case awstypes.Name, openstacktypes.Name, powervctypes.Name, vspheretypes.Name, azuretypes.Name, gcptypes.Name, ibmcloudtypes.Name, ovirttypes.Name:
		if installConfig.Config.CredentialsMode != types.ManualCredentialsMode {
			assetData["99_cloud-creds-secret.yaml"] = applyTemplateData(cloudCredsSecret.Files()[0].Data, templateData)
		}
		assetData["99_role-cloud-creds-secret-reader.yaml"] = applyTemplateData(roleCloudCredsSecretReader.Files()[0].Data, templateData)
	case baremetaltypes.Name:
		bmTemplateData := baremetalTemplateData{
			Baremetal:                 installConfig.Config.Platform.BareMetal,
			ProvisioningOSDownloadURL: rhcosImage.ControlPlane,
		}
		assetData["99_baremetal-provisioning-config.yaml"] = applyTemplateData(baremetalConfig.Files()[0].Data, bmTemplateData)
	}

	o.FileList = []*asset.File{}
	for name, data := range assetData {
		if len(data) == 0 {
			continue
		}
		o.FileList = append(o.FileList, &asset.File{
			Filename: path.Join(openshiftManifestDir, name),
			Data:     data,
		})
	}

	o.FileList = append(o.FileList, openshiftInstall.Files()...)
	o.FileList = append(o.FileList, featureGate.Files()...)
	o.FileList = append(o.FileList, osImageStream.Files()...)
	o.FileList = append(o.FileList, imageRegistryConfig.Files()...)

	if platform == externaltypes.Name {
		// A provider payload carries its own day-0 manifests, and they are
		// rendered into the same extra-manifests directory a user writes by
		// hand. This is the first stage that reads that directory, so it is
		// the first that has to make sure the payload is there -- and doing it
		// here rather than in a dedicated asset keeps the resolution out of
		// every other platform's dependency graph.
		if _, err := payload.ResolveForInstallConfig(ctx, installConfig.Config, command.RootOpts.Dir, clusterID.InfraID); err != nil {
			o.FileList = nil
			return fmt.Errorf("failed to resolve the Cluster API provider payload: %w", err)
		}

		generated := sets.New[string]()
		for _, f := range o.FileList {
			generated.Insert(path.Base(f.Filename))
		}
		extra, err := externalExtraManifests(command.RootOpts.Dir, generated)
		if err != nil {
			// Drop everything generated so far, because a failed Generate is
			// still persisted: pkg/asset/store/assetsfetcher.go:52 calls
			// PersistToFile unconditionally and only then returns the error.
			// Leaving the partial set on disk would be worse than the error
			// it came from -- the openshift directory is an asset load path,
			// so the next run would find those files, mark the asset
			// onDiskSource, skip Generate entirely and install a cluster with
			// no extra manifests and no second error.
			o.FileList = nil
			return err
		}
		o.FileList = append(o.FileList, extra...)
	}

	asset.SortFiles(o.FileList)

	return nil
}

// externalExtraManifests reads the cluster manifests the user supplied for the
// External platform and returns them as openshift manifest files.
//
// This runs inside Generate rather than Load on purpose, and the difference is
// the whole point. Load is exclusive: an asset the store finds on disk is
// marked onDiskSource and its Generate never runs (pkg/asset/store/store.go:196-232),
// so a user who writes one file into the openshift directory replaces every
// generated manifest rather than adding to one. Reading a separate directory
// here is additive, which is what lets a single `create cluster` deliver a
// partner's cloud controller manager -- the alternative is the three-command
// `create manifests`, edit, `create ignition-configs` dance the CI job for
// this platform performs today.
//
// A missing directory is not an error. Supplying nothing is a legitimate
// choice: an infrastructure-only run has nothing to deliver, and the install
// that needs a CCM fails later in a way the user can already read.
func externalExtraManifests(installDir string, generated sets.Set[string]) ([]*asset.File, error) {
	dir := filepath.Join(installDir, externaltypes.ManifestDir, externaltypes.ExtraManifestDir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", dir, err)
	}

	var files []*asset.File
	for _, entry := range entries {
		if entry.IsDir() || !manifestFileExtensions.Has(strings.ToLower(filepath.Ext(entry.Name()))) {
			continue
		}
		// A user file sharing a name with a generated one would be written
		// over it, and the loss -- a missing feature gate, a missing
		// kubeadmin password secret -- would surface nowhere near the cause.
		// Refusing is safe because the user picks these names.
		if generated.Has(entry.Name()) {
			return nil, fmt.Errorf("the %s manifest %s is also generated by the installer: "+
				"rename it, since one would silently overwrite the other in the %q directory",
				externaltypes.ExtraManifestDir, entry.Name(), openshiftManifestDir)
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", path, err)
		}
		if n, err := countManifestDocuments(data); err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		} else if n > 1 {
			return nil, fmt.Errorf("the %s manifest %s contains %d objects: "+
				"split it so each file holds exactly one, since the bootstrap node applies "+
				"the %q directory one object per file and silently ignores the rest",
				externaltypes.ExtraManifestDir, entry.Name(), n, openshiftManifestDir)
		}
		files = append(files, &asset.File{
			Filename: filepath.Join(openshiftManifestDir, entry.Name()),
			Data:     data,
		})
		// Path and size, never contents: a partner's CCM manifest may carry
		// the cloud credential it needs, and this log line is the first thing
		// pasted into a bug report.
		logrus.Infof("Including the %s manifest %s (%d bytes) in the openshift manifests",
			externaltypes.Name, entry.Name(), len(data))
	}
	return files, nil
}

// countManifestDocuments returns the number of non-empty YAML documents in a
// manifest file.
//
// This exists because of a failure that cost a cluster its worker nodes and
// reported nothing. A file in the extra manifest directory held two
// MachineConfigs -- one for the master pool, one for the worker pool -- in the
// ordinary multi-document form any `kubectl apply -f` accepts. The installer
// copied it through byte for byte, as it should. The bootstrap node then
// applied the first object and dropped the second without a word, so the
// worker pool never got its providerID drop-in, its kubelet registered a Node
// with no providerID, and the cloud controller manager deleted that Node as an
// instance it could not find. The install still reported success.
//
// Every manifest the installer itself writes into the openshift directory
// holds exactly one object, which is why this was never exercised before a
// user-supplied file arrived. Refusing here turns a silent, far-away node
// deletion into a message naming the file, and costs a user one `csplit`.
func countManifestDocuments(data []byte) (int, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	count := 0
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
		// Comment-only and whitespace-only documents are not objects. A file
		// that leads with a licence header and a `---` is the common shape and
		// must not be counted as two.
		var obj interface{}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			return 0, err
		}
		if obj != nil {
			count++
		}
	}
}

// manifestFileExtensions are the extensions read from the extra manifest
// directory. It matches what the openshift manifest directory accepts on Load,
// so a file that works in one works in the other.
var manifestFileExtensions = sets.New(".yaml", ".yml", ".json")

// Files returns the files generated by the asset.
func (o *Openshift) Files() []*asset.File {
	return o.FileList
}

// Load returns the openshift asset from disk.
func (o *Openshift) Load(f asset.FileFetcher) (bool, error) {
	yamlFileList, err := f.FetchByPattern(filepath.Join(openshiftManifestDir, "*.yaml"))
	if err != nil {
		return false, errors.Wrap(err, "failed to load *.yaml files")
	}
	ymlFileList, err := f.FetchByPattern(filepath.Join(openshiftManifestDir, "*.yml"))
	if err != nil {
		return false, errors.Wrap(err, "failed to load *.yml files")
	}
	jsonFileList, err := f.FetchByPattern(filepath.Join(openshiftManifestDir, "*.json"))
	if err != nil {
		return false, errors.Wrap(err, "failed to load *.json files")
	}
	fileList := append(yamlFileList, ymlFileList...)
	fileList = append(fileList, jsonFileList...)

	for _, file := range fileList {
		if machines.IsMachineManifest(file) {
			continue
		}

		o.FileList = append(o.FileList, file)
	}

	asset.SortFiles(o.FileList)
	return len(o.FileList) > 0, nil
}
