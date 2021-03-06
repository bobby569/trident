package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/netapp/trident/cli/api"
	"github.com/netapp/trident/cli/k8s_client"
	"github.com/netapp/trident/cli/ucp_client"

	tridentconfig "github.com/netapp/trident/config"
	"github.com/netapp/trident/logging"
	"github.com/netapp/trident/storage"
	"github.com/netapp/trident/storage/factory"
	sa "github.com/netapp/trident/storage_attribute"
	"github.com/netapp/trident/utils"
)

const (
	PreferredNamespace = tridentconfig.OrchestratorName
	DefaultPVCName     = tridentconfig.OrchestratorName
	DefaultPVName      = tridentconfig.OrchestratorName
	DefaultVolumeName  = tridentconfig.OrchestratorName
	DefaultVolumeSize  = "2Gi"

	BackendConfigFilename      = "backend.json"
	NamespaceFilename          = "trident-namespace.yaml"
	ServiceAccountFilename     = "trident-serviceaccount.yaml"
	ClusterRoleFilename        = "trident-clusterrole.yaml"
	ClusterRoleBindingFilename = "trident-clusterrolebinding.yaml"
	PVCFilename                = "trident-pvc.yaml"
	DeploymentFilename         = "trident-deployment.yaml"
	ServiceFilename            = "trident-service.yaml"
	StatefulSetFilename        = "trident-statefulset.yaml"
	DaemonSetFilename          = "trident-daemonset.yaml"
)

var (
	// CLI flags
	dryRun       bool
	generateYAML bool
	useYAML      bool
	silent       bool
	csi          bool
	pvName       string
	pvcName      string
	volumeName   string
	volumeSize   string
	tridentImage string
	etcdImage    string
	k8sTimeout   time.Duration

	// Docker EE / UCP related
	useKubernetesRBAC bool
	ucpBearerToken    string
	ucpHost           string
	ucpTraceREST      bool

	// CLI-based K8S client
	client k8s_client.Interface

	// UCP REST client
	ucpClient ucpclient.Interface

	// File paths
	installerDirectoryPath string
	setupPath              string
	backendConfigFilePath  string
	namespacePath          string
	serviceAccountPath     string
	clusterRolePath        string
	clusterRoleBindingPath string
	pvcPath                string
	deploymentPath         string
	csiServicePath         string
	csiStatefulSetPath     string
	csiDaemonSetPath       string
	setupYAMLPaths         []string

	appLabel      string
	appLabelKey   string
	appLabelValue string

	dns1123LabelRegex  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123DomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

func init() {
	RootCmd.AddCommand(installCmd)
	installCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Run all the pre-checks, but don't install anything.")
	installCmd.Flags().BoolVar(&generateYAML, "generate-custom-yaml", false, "Generate YAML files, but don't install anything.")
	installCmd.Flags().BoolVar(&useYAML, "use-custom-yaml", false, "Use any existing YAML files that exist in setup directory.")
	installCmd.Flags().BoolVar(&silent, "silent", false, "Disable most output during installation.")
	installCmd.Flags().BoolVar(&csi, "csi", false, "Install CSI Trident (experimental).")

	installCmd.Flags().StringVar(&pvcName, "pvc", "", "The name of the PVC used by Trident.")
	installCmd.Flags().StringVar(&pvName, "pv", "", "The name of the PV used by Trident.")
	installCmd.Flags().StringVar(&volumeName, "volume-name", "", "The name of the storage volume used by Trident.")
	installCmd.Flags().StringVar(&volumeSize, "volume-size", DefaultVolumeSize, "The size of the storage volume used by Trident.")
	installCmd.Flags().StringVar(&tridentImage, "trident-image", "", "The Trident image to install.")
	installCmd.Flags().StringVar(&etcdImage, "etcd-image", "", "The etcd image to install.")

	installCmd.Flags().DurationVar(&k8sTimeout, "k8s-timeout", 180*time.Second, "The number of seconds to wait before timing out on Kubernetes operations.")

	installCmd.Flags().StringVar(&ucpBearerToken, "ucp-bearer-token", "", "UCP authorization token.")
	installCmd.Flags().StringVar(&ucpHost, "ucp-host", "", "IP address of the UCP host.")
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Trident",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {

		initInstallerLogging()

		if err := discoverInstallationEnvironment(); err != nil {
			log.Fatalf("Install pre-checks failed; %v", err)
		}
		processInstallationArguments()
		if err := validateInstallationArguments(); err != nil {
			log.Fatalf("Invalid arguments; %v", err)
		}
	},
	Run: func(cmd *cobra.Command, args []string) {

		if generateYAML {

			// If generate-custom-yaml was specified, write the YAML files to the setup directory
			if csi {
				if err := prepareCSIYAMLFiles(); err != nil {
					log.Fatalf("YAML generation failed; %v", err)
				}
			} else {
				if err := prepareYAMLFiles(); err != nil {
					log.Fatalf("YAML generation failed; %v", err)
				}
			}
			log.WithField("setupPath", setupPath).Info("Wrote installation YAML files.")

		} else {

			// Run the installer
			if err := installTrident(); err != nil {
				log.Fatalf("Install failed; %v.  Resolve the issue; use 'tridentctl uninstall' "+
					"to clean up; and try again.", err)
			}
		}
	},
}

// initInstallerLogging configures logging for Trident installation. Logs are written to stdout.
func initInstallerLogging() {

	// Installer logs to stdout only
	log.SetOutput(os.Stdout)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true})

	logLevel := "info"
	if silent {
		logLevel = "fatal"
	}
	err := logging.InitLogLevel(Debug, logLevel)
	if err != nil {
		log.WithField("error", err).Fatal("Failed to initialize logging.")
	}

	log.WithField("logLevel", log.GetLevel().String()).Debug("Initialized logging.")
}

// discoverInstallationEnvironment inspects the current environment and checks
// that everything looks good for Trident installation, but it makes no changes
// to the environment.
func discoverInstallationEnvironment() error {

	var err error

	OperatingMode = ModeInstall

	// Default deployment image to what Trident was built with
	if tridentImage == "" {
		tridentImage = tridentconfig.BuildImage
	}

	// Default deployment image to what etcd was built with
	if etcdImage == "" {
		etcdImage = tridentconfig.BuildEtcdImage
	} else if !strings.Contains(etcdImage, tridentconfig.BuildEtcdVersion) {
		log.Warningf("Trident was qualified with etcd %s. You appear to be using a different version.", tridentconfig.BuildEtcdVersion)
	}

	// Ensure we're on Linux
	if runtime.GOOS != "linux" {
		return errors.New("the Trident installer only runs on Linux")
	}

	// Create the CLI-based Kubernetes client
	client, err = k8s_client.NewKubectlClient()
	if err != nil {
		return fmt.Errorf("could not initialize Kubernetes client; %v", err)
	}

	useKubernetesRBAC = true
	if ucpBearerToken != "" || ucpHost != "" {
		useKubernetesRBAC = false
		if ucpClient, err = ucpclient.NewClient(ucpHost, ucpBearerToken); err != nil {
			return err
		}
	}

	// Prepare input file paths
	if err = prepareYAMLFilePaths(); err != nil {
		return err
	}

	// Infer installation namespace if not specified
	if TridentPodNamespace == "" {
		TridentPodNamespace = client.Namespace()

		// Warn the user if no namespace was specified and the current namespace isn't "trident"
		if client.Namespace() != PreferredNamespace {
			log.WithFields(log.Fields{
				"example": fmt.Sprintf("./tridentctl install -n %s", PreferredNamespace),
			}).Warning("For maximum security, we recommend running Trident in its own namespace.")
		}
	}

	// Direct all subsequent client commands to the chosen namespace
	client.SetNamespace(TridentPodNamespace)

	log.WithFields(log.Fields{
		"installationNamespace": TridentPodNamespace,
		"kubernetesVersion":     client.Version().String(),
	}).Debug("Validated Trident installation environment.")

	return nil
}

func processInstallationArguments() {

	if pvcName == "" {
		if csi {
			pvcName = DefaultPVCName + "-csi"
		} else {
			pvcName = DefaultPVCName
		}
	}

	if pvName == "" {
		if csi {
			pvName = DefaultPVName + "-csi"
		} else {
			pvName = DefaultPVName
		}
	}

	if volumeName == "" {
		if csi {
			volumeName = DefaultVolumeName + "-csi"
		} else {
			volumeName = DefaultVolumeName
		}
	}

	if csi {
		appLabel = TridentCSILabel
		appLabelKey = TridentCSILabelKey
		appLabelValue = TridentCSILabelValue
	} else {
		appLabel = TridentLabel
		appLabelKey = TridentLabelKey
		appLabelValue = TridentLabelValue
	}
}

func validateInstallationArguments() error {

	labelFormat := "a DNS-1123 label must consist of lower case alphanumeric characters or '-', " +
		"and must start and end with an alphanumeric character"
	subdomainFormat := "a DNS-1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', " +
		"and must start and end with an alphanumeric character"

	if !dns1123LabelRegex.MatchString(TridentPodNamespace) {
		return fmt.Errorf("'%s' is not a valid namespace name; %s", TridentPodNamespace, labelFormat)
	}
	if !dns1123DomainRegex.MatchString(pvcName) {
		return fmt.Errorf("'%s' is not a valid PVC name; %s", pvcName, subdomainFormat)
	}
	if !dns1123DomainRegex.MatchString(pvName) {
		return fmt.Errorf("'%s' is not a valid PV name; %s", pvName, subdomainFormat)
	}

	return nil
}

// prepareYAMLFilePaths sets up the absolute file paths to all files
func prepareYAMLFilePaths() error {

	var err error

	// Get directory of installer
	installerDirectoryPath, err = filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return fmt.Errorf("could not determine installer working directory; %v", err)
	}

	setupPath = path.Join(installerDirectoryPath, "setup")
	backendConfigFilePath = path.Join(setupPath, BackendConfigFilename)
	namespacePath = path.Join(setupPath, NamespaceFilename)
	serviceAccountPath = path.Join(setupPath, ServiceAccountFilename)
	clusterRolePath = path.Join(setupPath, ClusterRoleFilename)
	clusterRoleBindingPath = path.Join(setupPath, ClusterRoleBindingFilename)
	pvcPath = path.Join(setupPath, PVCFilename)
	deploymentPath = path.Join(setupPath, DeploymentFilename)
	csiServicePath = path.Join(setupPath, ServiceFilename)
	csiStatefulSetPath = path.Join(setupPath, StatefulSetFilename)
	csiDaemonSetPath = path.Join(setupPath, DaemonSetFilename)

	setupYAMLPaths = []string{
		namespacePath, serviceAccountPath, clusterRolePath, clusterRoleBindingPath,
		pvcPath, deploymentPath, csiServicePath, csiStatefulSetPath, csiDaemonSetPath,
	}

	return nil
}

func cleanYAMLFiles() {

	for _, filePath := range setupYAMLPaths {
		os.Remove(filePath)
	}
}

func prepareYAMLFiles() error {

	var err error

	cleanYAMLFiles()

	namespaceYAML := k8s_client.GetNamespaceYAML(TridentPodNamespace)
	if err = writeFile(namespacePath, namespaceYAML); err != nil {
		return fmt.Errorf("could not write namespace YAML file; %v", err)
	}

	serviceAccountYAML := k8s_client.GetServiceAccountYAML(false)
	if err = writeFile(serviceAccountPath, serviceAccountYAML); err != nil {
		return fmt.Errorf("could not write service account YAML file; %v", err)
	}

	clusterRoleYAML := k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), false)
	if err = writeFile(clusterRolePath, clusterRoleYAML); err != nil {
		return fmt.Errorf("could not write cluster role YAML file; %v", err)
	}

	clusterRoleBindingYAML := k8s_client.GetClusterRoleBindingYAML(
		TridentPodNamespace, client.Flavor(), client.Version(), false)
	if err = writeFile(clusterRoleBindingPath, clusterRoleBindingYAML); err != nil {
		return fmt.Errorf("could not write cluster role binding YAML file; %v", err)
	}

	pvcYAML := k8s_client.GetPVCYAML(pvcName, TridentPodNamespace, volumeSize, appLabelValue)
	if err = writeFile(pvcPath, pvcYAML); err != nil {
		return fmt.Errorf("could not write PVC YAML file; %v", err)
	}

	deploymentYAML := k8s_client.GetDeploymentYAML(pvcName, tridentImage, etcdImage, appLabelValue, Debug)
	if err = writeFile(deploymentPath, deploymentYAML); err != nil {
		return fmt.Errorf("could not write deployment YAML file; %v", err)
	}

	return nil
}

func prepareCSIYAMLFiles() error {

	var err error

	cleanYAMLFiles()

	namespaceYAML := k8s_client.GetNamespaceYAML(TridentPodNamespace)
	if err = writeFile(namespacePath, namespaceYAML); err != nil {
		return fmt.Errorf("could not write namespace YAML file; %v", err)
	}

	serviceAccountYAML := k8s_client.GetServiceAccountYAML(true)
	if err = writeFile(serviceAccountPath, serviceAccountYAML); err != nil {
		return fmt.Errorf("could not write service account YAML file; %v", err)
	}

	clusterRoleYAML := k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), true)
	if err = writeFile(clusterRolePath, clusterRoleYAML); err != nil {
		return fmt.Errorf("could not write cluster role YAML file; %v", err)
	}

	clusterRoleBindingYAML := k8s_client.GetClusterRoleBindingYAML(
		TridentPodNamespace, client.Flavor(), client.Version(), true)
	if err = writeFile(clusterRoleBindingPath, clusterRoleBindingYAML); err != nil {
		return fmt.Errorf("could not write cluster role binding YAML file; %v", err)
	}

	pvcYAML := k8s_client.GetPVCYAML(pvcName, TridentPodNamespace, volumeSize, appLabelValue)
	if err = writeFile(pvcPath, pvcYAML); err != nil {
		return fmt.Errorf("could not write PVC YAML file; %v", err)
	}

	serviceYAML := k8s_client.GetCSIServiceYAML(appLabelValue)
	if err = writeFile(csiServicePath, serviceYAML); err != nil {
		return fmt.Errorf("could not write service YAML file; %v", err)
	}

	statefulSetYAML := k8s_client.GetCSIStatefulSetYAML(pvcName, tridentImage, etcdImage, appLabelValue, Debug)
	if err = writeFile(csiStatefulSetPath, statefulSetYAML); err != nil {
		return fmt.Errorf("could not write statefulset YAML file; %v", err)
	}

	daemonSetYAML := k8s_client.GetCSIDaemonSetYAML(tridentImage, TridentNodeLabelValue, Debug)
	if err = writeFile(csiDaemonSetPath, daemonSetYAML); err != nil {
		return fmt.Errorf("could not write daemonset YAML file; %v", err)
	}

	return nil
}

func writeFile(filePath, data string) error {
	return ioutil.WriteFile(filePath, []byte(data), 0644)
}

func installTrident() (returnError error) {

	var (
		logFields           log.Fields
		pvcExists           bool
		pvExists            bool
		pvc                 *v1.PersistentVolumeClaim
		pv                  *v1.PersistentVolume
		pvRequestedQuantity resource.Quantity
		storageBackend      *storage.Backend
	)

	// Validate volume size
	pvRequestedQuantity, err := resource.ParseQuantity(volumeSize)
	if err != nil {
		return fmt.Errorf("volume-size '%s' is invalid; %v", volumeSize, err)
	}
	log.WithField("quantity", pvRequestedQuantity.String()).Debug("Parsed requested volume size.")

	if !csi {
		log.WithFields(log.Fields{
			"useKubernetesRBAC": useKubernetesRBAC,
			"ucpBearerToken":    ucpBearerToken,
			"ucpHost":           ucpHost,
		}).Debug("Dumping RBAC fields.")

		// Ensure Trident isn't already installed
		if installed, namespace, err := isTridentInstalled(); err != nil {
			return fmt.Errorf("could not check if Trident deployment exists; %v", err)
		} else if installed {
			return fmt.Errorf("Trident is already installed in namespace %s", namespace)
		}

	} else {

		// Ensure CSI minimum requirements are met
		minCSIVersion := utils.MustParseSemantic(tridentconfig.KubernetesCSIVersionMin)
		if !client.Version().AtLeast(minCSIVersion) {
			return fmt.Errorf("CSI Trident requires Kubernetes %s or later", minCSIVersion.ShortString())
		}

		// Ensure CSI Trident isn't already installed
		if installed, namespace, err := isCSITridentInstalled(); err != nil {
			return fmt.Errorf("could not check if Trident statefulset exists; %v", err)
		} else if installed {
			return fmt.Errorf("CSI Trident is already installed in namespace %s", namespace)
		}

		log.Warning("CSI Trident for Kubernetes is a technology preview " +
			"and should not be installed in production environments!")
	}

	// Check if the required namespace exists
	namespaceExists, returnError := client.CheckNamespaceExists(TridentPodNamespace)
	if returnError != nil {
		returnError = fmt.Errorf("could not check if namespace %s exists; %v", TridentPodNamespace, returnError)
		return
	}
	if namespaceExists {
		log.WithField("namespace", TridentPodNamespace).Debug("Namespace exists.")
	} else {
		log.WithField("namespace", TridentPodNamespace).Debug("Namespace does not exist.")
	}

	// Check for PVC (also returns (false, nil) if namespace does not exist)
	pvcExists, returnError = client.CheckPVCExists(pvcName)
	if returnError != nil {
		returnError = fmt.Errorf("could not establish the presence of PVC %s; %v", pvcName, returnError)
		return
	}
	if pvcExists {
		pvc, returnError = client.GetPVC(pvcName)
		if returnError != nil {
			returnError = fmt.Errorf("could not retrieve PVC %s; %v", pvcName, returnError)
			return
		}

		// Ensure that the PVC is in a state that we can work with
		if pvc.Status.Phase == v1.ClaimLost {
			returnError = fmt.Errorf("PVC %s phase is Lost; please delete it and try again", pvcName)
			return
		}
		if pvc.Status.Phase == v1.ClaimBound && pvc.Spec.VolumeName != pvName {
			returnError = fmt.Errorf("PVC %s is Bound, but not to PV %s; "+
				"please specify a different PV and/or PVC", pvcName, pvName)
			return
		}
		if pvc.Labels == nil || pvc.Labels[appLabelKey] != appLabelValue {
			returnError = fmt.Errorf("PVC %s does not have %s label; "+
				"please add label or delete PVC and try again", pvcName, appLabel)
			return
		}

		log.WithFields(log.Fields{
			"pvc":       pvcName,
			"namespace": pvc.Namespace,
			"phase":     pvc.Status.Phase,
		}).Debug("PVC already exists.")

	} else {
		log.WithField("pvc", pvcName).Debug("PVC does not exist.")
	}

	// Check for PV
	pvExists, returnError = client.CheckPVExists(pvName)
	if returnError != nil {
		returnError = fmt.Errorf("could not establish the presence of PV %s; %v", pvName, returnError)
		return
	}
	if pvExists {
		pv, returnError = client.GetPV(pvName)
		if returnError != nil {
			returnError = fmt.Errorf("could not retrieve PV %s; %v", pvName, returnError)
			return
		}

		// Ensure that the PV is in a state we can work with
		if pv.Status.Phase == v1.VolumeReleased {
			returnError = fmt.Errorf("PV %s phase is Released; please delete it and try again", pvName)
			return
		}
		if pv.Status.Phase == v1.VolumeFailed {
			returnError = fmt.Errorf("PV %s phase is Failed; please delete it and try again", pvName)
			return
		}
		if pv.Status.Phase == v1.VolumeBound && pv.Spec.ClaimRef != nil {
			if pv.Spec.ClaimRef.Name != pvcName {
				returnError = fmt.Errorf("PV %s is Bound, but not to PVC %s; "+
					"please delete PV and try again", pvName, pvcName)
				return
			}
			if pv.Spec.ClaimRef.Namespace != TridentPodNamespace {
				returnError = fmt.Errorf("PV %s is Bound to a PVC in namespace %s; "+
					"please delete PV and try again", pvName, pv.Spec.ClaimRef.Namespace)
				return
			}
		}
		if pv.Labels == nil || pv.Labels[appLabelKey] != appLabelValue {
			returnError = fmt.Errorf("PV %s does not have %s label; "+
				"please add label or delete PV and try again", pvName, appLabel)
			return
		}

		// Ensure PV size matches the request
		if pvActualQuantity, ok := pv.Spec.Capacity[v1.ResourceStorage]; !ok {
			log.WithField("pv", pvName).Warning("Could not determine size of existing PV.")
		} else if pvRequestedQuantity.Cmp(pvActualQuantity) != 0 {
			log.WithFields(log.Fields{
				"existing": pvActualQuantity.String(),
				"request":  pvRequestedQuantity.String(),
				"pv":       pvName,
			}).Warning("Existing PV size does not match request.")
		}

		log.WithFields(log.Fields{
			"pv":    pvName,
			"phase": pv.Status.Phase,
		}).Debug("PV already exists.")

	} else {
		log.WithField("pv", pvName).Debug("PV does not exist.")
	}

	// If the PV doesn't exist, we will need the storage driver to create it. Load the driver
	// here to detect any problems before starting the installation steps.
	if !pvExists {
		if storageBackend, returnError = loadStorageDriver(); returnError != nil {
			return
		}
	} else {
		log.Debug("PV exists, skipping storage driver check.")
	}

	// If dry-run was specified, stop before we change anything
	if dryRun {
		log.Info("Dry run completed, no problems found.")
		return
	}

	// All checks succeeded, so proceed with installation
	log.WithField("namespace", TridentPodNamespace).Info("Starting Trident installation.")

	// Create namespace if it doesn't exist
	if !namespaceExists {
		if useYAML && fileExists(namespacePath) {
			returnError = client.CreateObjectByFile(namespacePath)
			logFields = log.Fields{"path": namespacePath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetNamespaceYAML(TridentPodNamespace))
			logFields = log.Fields{"namespace": TridentPodNamespace}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create namespace %s; %v", TridentPodNamespace, returnError)
			return
		}
		log.WithFields(logFields).Info("Created namespace.")
	}

	// Remove any RBAC objects from a previous Trident installation
	if anyCleanupErrors := removeRBACObjects(log.DebugLevel); anyCleanupErrors {
		returnError = fmt.Errorf("could not remove one or more previous Trident artifacts; " +
			"please delete them manually and try again")
		return
	}

	// Create the RBAC objects
	if returnError = createRBACObjects(); returnError != nil {
		return
	}

	// Create PVC if necessary
	if !pvcExists {
		if useYAML && fileExists(pvcPath) {
			returnError = validateTridentPVC()
			if returnError != nil {
				returnError = fmt.Errorf("please correct the PVC YAML file; %v", returnError)
				return
			}
			returnError = client.CreateObjectByFile(pvcPath)
			logFields = log.Fields{"path": pvcPath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetPVCYAML(
				pvcName, TridentPodNamespace, volumeSize, appLabelValue))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create PVC %s; %v", pvcName, returnError)
			return
		}
		log.WithFields(logFields).Info("Created PVC.")
	}

	// Create PV if necessary
	if !pvExists {
		returnError = createPV(storageBackend)
		if returnError != nil {
			returnError = fmt.Errorf("could not create PV %s; %v", pvName, returnError)
			return
		}
		log.WithField("pv", pvName).Info("Created PV.")
	}

	// Wait for PV/PVC to be bound
	checkPVCBound := func() error {
		bound, err := client.CheckPVCBound(pvcName)
		if err != nil || !bound {
			return errors.New("PVC not bound")
		}
		return nil
	}
	if checkError := checkPVCBound(); checkError != nil {
		pvcNotify := func(err error, duration time.Duration) {
			log.WithFields(log.Fields{
				"pvc":       pvcName,
				"increment": duration,
			}).Debugf("PVC not yet bound, waiting.")
		}
		pvcBackoff := backoff.NewExponentialBackOff()
		pvcBackoff.MaxElapsedTime = k8sTimeout

		log.WithField("pvc", pvcName).Info("Waiting for PVC to be bound.")

		if err := backoff.RetryNotify(checkPVCBound, pvcBackoff, pvcNotify); err != nil {
			returnError = fmt.Errorf("PVC %s was not bound after %d seconds", pvcName, k8sTimeout)
			return
		}
	}

	if !csi {

		// Create the deployment
		if useYAML && fileExists(deploymentPath) {
			returnError = validateTridentDeployment()
			if returnError != nil {
				returnError = fmt.Errorf("please correct the deployment YAML file; %v", returnError)
				return
			}
			returnError = client.CreateObjectByFile(deploymentPath)
			logFields = log.Fields{"path": deploymentPath}
		} else {
			returnError = client.CreateObjectByYAML(
				k8s_client.GetDeploymentYAML(pvcName, tridentImage, etcdImage, appLabelValue, Debug))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create Trident deployment; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created Trident deployment.")

	} else {

		// Create the service
		if useYAML && fileExists(csiServicePath) {
			returnError = validateTridentService()
			if returnError != nil {
				returnError = fmt.Errorf("please correct the service YAML file; %v", returnError)
				return
			}
			returnError = client.CreateObjectByFile(csiServicePath)
			logFields = log.Fields{"path": csiServicePath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetCSIServiceYAML(appLabelValue))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create Trident service; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created Trident service.")

		// Create the statefulset
		if useYAML && fileExists(csiStatefulSetPath) {
			returnError = validateTridentStatefulSet()
			if returnError != nil {
				returnError = fmt.Errorf("please correct the statefulset YAML file; %v", returnError)
				return
			}
			returnError = client.CreateObjectByFile(csiStatefulSetPath)
			logFields = log.Fields{"path": csiStatefulSetPath}
		} else {
			returnError = client.CreateObjectByYAML(
				k8s_client.GetCSIStatefulSetYAML(pvcName, tridentImage, etcdImage, appLabelValue, Debug))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create Trident statefulset; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created Trident statefulset.")

		// Create the daemonset
		if useYAML && fileExists(csiDaemonSetPath) {
			returnError = validateTridentDaemonSet()
			if returnError != nil {
				returnError = fmt.Errorf("please correct the daemonset YAML file; %v", returnError)
				return
			}
			returnError = client.CreateObjectByFile(csiDaemonSetPath)
			logFields = log.Fields{"path": csiDaemonSetPath}
		} else {
			returnError = client.CreateObjectByYAML(
				k8s_client.GetCSIDaemonSetYAML(tridentImage, TridentNodeLabelValue, Debug))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create Trident daemonset; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created Trident daemonset.")
	}

	// Wait for Trident pod to be running
	var tridentPod *v1.Pod

	tridentPod, returnError = waitForTridentPod()
	if returnError != nil {
		return
	}

	// Wait for Trident REST interface to be available
	TridentPodName = tridentPod.Name
	returnError = waitForRESTInterface()
	if returnError != nil {
		returnError = fmt.Errorf("%v; use 'tridentctl logs' to learn more", returnError)
		return
	}

	log.Info("Trident installation succeeded.")
	return nil
}

func loadStorageDriver() (backend *storage.Backend, returnError error) {

	// Set up telemetry so any PV we create has the correct metadata
	tridentconfig.OrchestratorTelemetry = tridentconfig.Telemetry{
		TridentVersion:  tridentconfig.OrchestratorVersion.String(),
		Platform:        "kubernetes",
		PlatformVersion: client.Version().ShortString(),
	}

	if csi {
		tridentconfig.CurrentDriverContext = tridentconfig.ContextCSI
	} else {
		tridentconfig.CurrentDriverContext = tridentconfig.ContextKubernetes
	}

	// Ensure the setup directory & backend config file are present
	if _, returnError = os.Stat(setupPath); os.IsNotExist(returnError) {
		returnError = fmt.Errorf("setup directory does not exist; %v", returnError)
		return
	}
	if _, returnError = os.Stat(backendConfigFilePath); os.IsNotExist(returnError) {
		returnError = fmt.Errorf("storage backend config file does not exist; %v", returnError)
		return
	}

	// Try to start the driver, which is the source of many installation problems and
	// will be needed to if we have to provision the Trident PV.
	log.WithField("backend", backendConfigFilePath).Info("Starting storage driver.")
	configFileBytes, returnError := ioutil.ReadFile(backendConfigFilePath)
	if returnError != nil {
		returnError = fmt.Errorf("could not read the storage backend config file; %v", returnError)
		return
	}
	backend, returnError = factory.NewStorageBackendForConfig(string(configFileBytes))
	if returnError != nil {
		returnError = fmt.Errorf("could not start the storage backend driver; %v", returnError)
		return
	}

	log.WithField("driver", backend.GetDriverName()).Info("Storage driver loaded.")
	return
}

func createRBACObjects() (returnError error) {

	var logFields log.Fields

	// Create service account
	if useYAML && fileExists(serviceAccountPath) {
		returnError = client.CreateObjectByFile(serviceAccountPath)
		logFields = log.Fields{"path": serviceAccountPath}
	} else {
		returnError = client.CreateObjectByYAML(k8s_client.GetServiceAccountYAML(csi))
		logFields = log.Fields{}
	}
	if returnError != nil {
		returnError = fmt.Errorf("could not create service account; %v", returnError)
		return
	}
	log.WithFields(logFields).Info("Created service account.")

	if useKubernetesRBAC {

		// Create cluster role
		if useYAML && fileExists(clusterRolePath) {
			returnError = client.CreateObjectByFile(clusterRolePath)
			logFields = log.Fields{"path": clusterRolePath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), csi))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create cluster role; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created cluster role.")

		// Create cluster role binding
		if useYAML && fileExists(clusterRoleBindingPath) {
			returnError = client.CreateObjectByFile(clusterRoleBindingPath)
			logFields = log.Fields{"path": clusterRoleBindingPath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetClusterRoleBindingYAML(
				TridentPodNamespace, client.Flavor(), client.Version(), csi))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create cluster role binding; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created cluster role binding.")

		// If OpenShift, add Trident to security context constraint
		if client.Flavor() == k8s_client.FlavorOpenShift {
			if returnError = client.AddTridentUserToOpenShiftSCC(); returnError != nil {
				returnError = fmt.Errorf("could not modify security context constraint; %v", returnError)
				return
			}
			log.Info("Added Trident user to security context constraint.")
		}

	} else {
		createdRole, clientError := ucpClient.CreateTridentRole()
		logFields = log.Fields{"createdRole": createdRole}
		if clientError != nil {
			return fmt.Errorf("could not create Trident UCP role; %v", clientError)
		}
		log.WithFields(logFields).Info("Created Trident UCP role.")

		addedRole, clientError := ucpClient.AddTridentRoleToServiceAccount(TridentPodNamespace)
		logFields = log.Fields{"addedRole": addedRole}
		if clientError != nil {
			return fmt.Errorf("could not add Trident UCP role to service account; %v", clientError)
		}
		log.WithFields(logFields).Info("Added Trident UCP role to service account.")
	}

	return
}

func removeRBACObjects(logLevel log.Level) (anyErrors bool) {

	logFunc := log.Info
	if logLevel == log.DebugLevel {
		logFunc = log.Debug
	}

	if useKubernetesRBAC {

		// Delete cluster role binding
		clusterRoleBindingYAML := k8s_client.GetClusterRoleBindingYAML(
			TridentPodNamespace, client.Flavor(), client.Version(), csi)
		if err := client.DeleteObjectByYAML(clusterRoleBindingYAML, true); err != nil {
			log.WithField("error", err).Warning("Could not delete cluster role binding.")
			anyErrors = true
		} else {
			logFunc("Deleted cluster role binding.")
		}

		// Delete cluster role
		clusterRoleYAML := k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), csi)
		if err := client.DeleteObjectByYAML(clusterRoleYAML, true); err != nil {
			log.WithField("error", err).Warning("Could not delete cluster role.")
			anyErrors = true
		} else {
			logFunc("Deleted cluster role.")
		}
	} else {
		removedRoleFromAccount, clientError := ucpClient.RemoveTridentRoleFromServiceAccount(TridentPodNamespace)
		if clientError != nil {
			log.WithField("error", clientError).Warning("Could not remove Trident UCP role from service account")
			//anyErrors = true
		} else {
			logFields := log.Fields{"removedRoleFromAccount": removedRoleFromAccount}
			log.WithFields(logFields).Info("Removed Trident UCP role from service account.")
		}

		deletedRole, clientError := ucpClient.DeleteTridentRole()
		if clientError != nil {
			log.WithField("error", clientError).Warning("could not delete Trident UCP role")
			//anyErrors = true
		} else {
			logFields := log.Fields{"deletedRole": deletedRole}
			log.WithFields(logFields).Info("Deleted Trident UCP role.")
		}
	}

	// Delete service account
	serviceAccountYAML := k8s_client.GetServiceAccountYAML(csi)
	if err := client.DeleteObjectByYAML(serviceAccountYAML, true); err != nil {
		log.WithField("error", err).Warning("Could not delete service account.")
		anyErrors = true
	} else {
		logFunc("Deleted service account.")
	}

	if useKubernetesRBAC {
		// If OpenShift, remove Trident from security context constraint
		if client.Flavor() == k8s_client.FlavorOpenShift {
			if err := client.RemoveTridentUserFromOpenShiftSCC(); err != nil {
				log.WithField("error", err).Warning("Could not modify security context constraint.")
				anyErrors = true
			} else {
				logFunc("Removed Trident user from security context constraint.")
			}
		}
	}

	return
}

/*
func createRBACObjects() (returnError error) {

	var logFields log.Fields

	// Create service account
	if useYAML && fileExists(serviceAccountPath) {
		returnError = client.CreateObjectByFile(serviceAccountPath)
		logFields = log.Fields{"path": serviceAccountPath}
	} else {
		returnError = client.CreateObjectByYAML(k8s_client.GetServiceAccountYAML(csi))
		logFields = log.Fields{}
	}
	if returnError != nil {
		returnError = fmt.Errorf("could not create service account; %v", returnError)
		return
	}
	log.WithFields(logFields).Info("Created service account.")

	if useKubernetesRBAC {
		// Create cluster role
		if useYAML && fileExists(clusterRolePath) {
			returnError = client.CreateObjectByFile(clusterRolePath)
			logFields = log.Fields{"path": clusterRolePath}
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), csi))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create cluster role; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created cluster role.")

		// If OpenShift, add Trident to security context constraint
		if client.Flavor() == k8s_client.FlavorOpenShift {
			if returnError = client.AddTridentUserToOpenShiftSCC(); returnError != nil {
				returnError = fmt.Errorf("could not modify security context constraint; %v", returnError)
				return
			}
			log.Info("Added Trident user to security context constraint.")
		} else {
			returnError = client.CreateObjectByYAML(k8s_client.GetClusterRoleBindingYAML(
				TridentPodNamespace, client.Flavor(), client.Version(), csi))
			logFields = log.Fields{}
		}
		if returnError != nil {
			returnError = fmt.Errorf("could not create cluster role binding; %v", returnError)
			return
		}
		log.WithFields(logFields).Info("Created cluster role binding.")

	} else {
		createdRole, clientError := ucpClient.CreateTridentRole()
		logFields = log.Fields{"createdRole": createdRole}
		if clientError != nil {
			return fmt.Errorf("could not create Trident UCP role; %v", clientError)
		}
		log.WithFields(logFields).Info("Created Trident UCP role.")

		addedRole, clientError := ucpClient.AddTridentRoleToServiceAccount()
		logFields = log.Fields{"addedRole": addedRole}
		if clientError != nil {
			return fmt.Errorf("could not add Trident UCP role to service account; %v", clientError)
		}
		log.WithFields(logFields).Info("Add Trident UCP role to service account.")
	}

	return
}

func removeRBACObjects(logLevel log.Level) (anyErrors bool) {

	logFunc := log.Info
	if logLevel == log.DebugLevel {
		logFunc = log.Debug
	}

	// Delete cluster role binding
	clusterRoleBindingYAML := k8s_client.GetClusterRoleBindingYAML(
		TridentPodNamespace, client.Flavor(), client.Version(), csi)
	if err := client.DeleteObjectByYAML(clusterRoleBindingYAML, true); err != nil {
		log.WithField("error", err).Warning("Could not delete cluster role binding.")
		anyErrors = true
	} else {
		logFunc("Deleted cluster role binding.")
	}

	// Delete cluster role
	clusterRoleYAML := k8s_client.GetClusterRoleYAML(client.Flavor(), client.Version(), csi)
	if err := client.DeleteObjectByYAML(clusterRoleYAML, true); err != nil {
		log.WithField("error", err).Warning("Could not delete cluster role.")
		anyErrors = true
	} else {
		logFunc("Deleted cluster role.")
	}

	// Delete service account
	serviceAccountYAML := k8s_client.GetServiceAccountYAML(csi)
	if err := client.DeleteObjectByYAML(serviceAccountYAML, true); err != nil {
		log.WithField("error", err).Warning("Could not delete service account.")
		anyErrors = true
	} else {
		logFunc("Deleted service account.")
	}

	// If OpenShift, remove Trident from security context constraint
	if client.Flavor() == k8s_client.FlavorOpenShift {
		if err := client.RemoveTridentUserFromOpenShiftSCC(); err != nil {
			log.WithField("error", err).Warning("Could not modify security context constraint.")
			anyErrors = true
		} else {
			logFunc("Removed Trident user from security context constraint.")
		}
	}

	return
}
*/

func validateTridentDeployment() error {

	deployment, err := client.ReadDeploymentFromFile(deploymentPath)
	if err != nil {
		return fmt.Errorf("could not load deployment YAML file; %v", err)
	}

	// Check the deployment label
	labels := deployment.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident deployment must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	// Check the pod label
	labels = deployment.Spec.Template.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident deployment's pod template must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	tridentImage := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == tridentconfig.ContainerTrident {
			tridentImage = container.Image
		}
	}
	if tridentImage == "" {
		return fmt.Errorf("the Trident deployment must define the %s container", tridentconfig.ContainerTrident)
	}

	return nil
}

func validateTridentService() error {

	service, err := client.ReadServiceFromFile(csiServicePath)
	if err != nil {
		return fmt.Errorf("could not load service YAML file; %v", err)
	}

	// Check the service label
	labels := service.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident service must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	return nil
}

func validateTridentStatefulSet() error {

	statefulset, err := client.ReadStatefulSetFromFile(csiStatefulSetPath)
	if err != nil {
		return fmt.Errorf("could not load statefulset YAML file; %v", err)
	}

	// Check the statefulset label
	labels := statefulset.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident statefulset must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	// Check the pod label
	labels = statefulset.Spec.Template.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident statefulset's pod template must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	tridentImage := ""
	for _, container := range statefulset.Spec.Template.Spec.Containers {
		if container.Name == tridentconfig.ContainerTrident {
			tridentImage = container.Image
		}
	}
	if tridentImage == "" {
		return fmt.Errorf("the Trident statefulset must define the %s container", tridentconfig.ContainerTrident)
	}

	return nil
}

func validateTridentDaemonSet() error {

	daemonset, err := client.ReadDaemonSetFromFile(csiDaemonSetPath)
	if err != nil {
		return fmt.Errorf("could not load daemonset YAML file; %v", err)
	}

	// Check the daemonset label
	labels := daemonset.Labels
	if labels[TridentNodeLabelKey] != TridentNodeLabelValue {
		return fmt.Errorf("the Trident daemonset must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	// Check the pod label
	labels = daemonset.Spec.Template.Labels
	if labels[TridentNodeLabelKey] != TridentNodeLabelValue {
		return fmt.Errorf("the Trident daemonset's pod template must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	tridentImage := ""
	for _, container := range daemonset.Spec.Template.Spec.Containers {
		if container.Name == tridentconfig.ContainerTrident {
			tridentImage = container.Image
		}
	}
	if tridentImage == "" {
		return fmt.Errorf("the Trident daemonset must define the %s container", tridentconfig.ContainerTrident)
	}

	return nil
}

func validateTridentPVC() error {

	pvc, err := client.ReadPVCFromFile(pvcPath)
	if err != nil {
		return fmt.Errorf("could not load PVC YAML file; %v", err)
	}

	// Check the label
	labels := pvc.Labels
	if labels[appLabelKey] != appLabelValue {
		return fmt.Errorf("the Trident PVC must have the label \"%s: %s\"",
			appLabelKey, appLabelValue)
	}

	// Check the name
	if pvc.Name != pvcName {
		return fmt.Errorf("the Trident PVC must be named %s", pvcName)
	}

	// Check the namespace
	if pvc.Namespace != TridentPodNamespace {
		return fmt.Errorf("the Trident PVC must specify namespace %s", TridentPodNamespace)
	}

	return nil
}

func createPV(sb *storage.Backend) error {

	// Choose a pool
	if len(sb.Storage) == 0 {
		return fmt.Errorf("backend %s has no storage pools", sb.Name)
	}
	var pool *storage.Pool
	for _, pool = range sb.Storage {
		// Let Golang's map iteration randomization choose a pool for us
		break
	}

	// Create the volume config
	volConfig := &storage.VolumeConfig{
		Version:  "1",
		Name:     volumeName,
		Size:     volumeSize,
		Protocol: sb.GetProtocol(),
	}

	volAttributes := make(map[string]sa.Request)

	// Create the volume on the backend
	volume, err := sb.AddVolume(volConfig, pool, volAttributes)
	if err != nil {
		return fmt.Errorf("could not create a volume on the storage backend; %v", err)
	}

	// Get the PV YAML (varies by volume protocol type)
	var pvYAML string
	switch {
	case volume.Config.AccessInfo.NfsAccessInfo.NfsServerIP != "":

		pvYAML = k8s_client.GetNFSPVYAML(pvName, volumeSize, pvcName, TridentPodNamespace,
			volume.Config.AccessInfo.NfsAccessInfo.NfsServerIP,
			volume.Config.AccessInfo.NfsAccessInfo.NfsPath,
			appLabelValue)

	case volume.Config.AccessInfo.IscsiAccessInfo.IscsiTargetPortal != "":

		if volume.Config.AccessInfo.IscsiTargetSecret != "" {

			// Validate CHAP support in Kubernetes
			if !client.Version().AtLeast(utils.MustParseSemantic("v1.7.0")) {
				return errors.New("iSCSI CHAP requires Kubernetes 1.7.0 or later")
			}

			// Using CHAP
			secretName, err := createCHAPSecret(volume)
			if err != nil {
				return err
			}

			pvYAML = k8s_client.GetCHAPISCSIPVYAML(pvName, volumeSize, pvcName, TridentPodNamespace, secretName,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiTargetPortal,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiTargetIQN,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiLunNumber,
				appLabelValue)

		} else {

			// Not using CHAP
			pvYAML = k8s_client.GetISCSIPVYAML(pvName, volumeSize, pvcName, TridentPodNamespace,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiTargetPortal,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiTargetIQN,
				volume.Config.AccessInfo.IscsiAccessInfo.IscsiLunNumber,
				appLabelValue)
		}

	default:
		return errors.New("unrecognized volume type")
	}

	// Create the PV
	err = client.CreateObjectByYAML(pvYAML)
	if err != nil {
		return fmt.Errorf("could not create PV %s; %v", pvName, err)
	}

	return nil
}

func createCHAPSecret(volume *storage.Volume) (secretName string, returnError error) {

	secretName = volume.ConstructExternal().GetCHAPSecretName()
	log.WithField("secret", secretName).Debug("Using iSCSI CHAP secret.")

	secretExists, err := client.CheckSecretExists(secretName)
	if err != nil {
		returnError = fmt.Errorf("could not check for existing iSCSI CHAP secret; %v", err)
		return
	}
	if !secretExists {
		log.WithField("secret", secretName).Debug("iSCSI CHAP secret does not exist.")

		// Create the YAML for the new secret
		secretYAML := k8s_client.GetCHAPSecretYAML(secretName,
			volume.Config.AccessInfo.IscsiUsername,
			volume.Config.AccessInfo.IscsiInitiatorSecret,
			volume.Config.AccessInfo.IscsiTargetSecret)

		// Create the secret
		err = client.CreateObjectByYAML(secretYAML)
		if err != nil {
			returnError = fmt.Errorf("could not create CHAP secret; %v", err)
			return
		}
		log.WithField("secret", secretName).Info("Created iSCSI CHAP secret.")
	} else {
		log.WithField("secret", secretName).Debug("iSCSI CHAP secret already exists.")
	}

	return
}

func waitForTridentPod() (*v1.Pod, error) {

	var pod *v1.Pod

	checkPodRunning := func() error {
		var podError error
		pod, podError = client.GetPodByLabel(appLabel, false)
		if podError != nil || pod.Status.Phase != v1.PodRunning {
			return errors.New("pod not running")
		}
		return nil
	}
	podNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"increment": duration,
		}).Debugf("Trident pod not yet running, waiting.")
	}
	podBackoff := backoff.NewExponentialBackOff()
	podBackoff.MaxElapsedTime = k8sTimeout

	log.Info("Waiting for Trident pod to start.")

	if err := backoff.RetryNotify(checkPodRunning, podBackoff, podNotify); err != nil {

		// Build up an error message with as much detail as available.
		var errMessages []string
		errMessages = append(errMessages,
			fmt.Sprintf("Trident pod was not running after %3.2f seconds.", k8sTimeout.Seconds()))

		if pod != nil {
			if pod.Status.Phase != "" {
				errMessages = append(errMessages, fmt.Sprintf("Pod status is %s.", pod.Status.Phase))
				if pod.Status.Message != "" {
					errMessages = append(errMessages, fmt.Sprintf("%s", pod.Status.Message))
				}
			}
			errMessages = append(errMessages,
				fmt.Sprintf("Use '%s describe pod %s -n %s' for more information.",
					client.CLI(), pod.Name, client.Namespace()))
		}

		log.Error(strings.Join(errMessages, " "))
		return nil, err
	}

	log.WithFields(log.Fields{
		"pod":       pod.Name,
		"namespace": TridentPodNamespace,
	}).Info("Trident pod started.")

	return pod, nil
}

func waitForRESTInterface() error {

	var version string

	checkRESTInterface := func() error {

		cliCommand := []string{"tridentctl", "-s", PodServer, "version", "-o", "json"}
		versionJSON, err := client.Exec(TridentPodName, tridentconfig.ContainerTrident, cliCommand)
		if err != nil {
			if versionJSON != nil && len(versionJSON) > 0 {
				err = fmt.Errorf("%v; %s", err, strings.TrimSpace(string(versionJSON)))
			}
			return err
		}

		var versionResponse api.VersionResponse
		err = json.Unmarshal(versionJSON, &versionResponse)
		if err != nil {
			return err
		}

		version = versionResponse.Server.Version
		return nil
	}
	restNotify := func(err error, duration time.Duration) {
		log.WithFields(log.Fields{
			"increment": duration,
		}).Debugf("REST interface not yet up, waiting.")
	}
	restBackoff := backoff.NewExponentialBackOff()
	restBackoff.MaxElapsedTime = k8sTimeout

	log.Info("Waiting for Trident REST interface.")

	if err := backoff.RetryNotify(checkRESTInterface, restBackoff, restNotify); err != nil {
		log.Errorf("Trident REST interface was not available after %3.2f seconds.", k8sTimeout.Seconds())
		return err
	}

	log.WithField("version", version).Info("Trident REST interface is up.")

	return nil
}
