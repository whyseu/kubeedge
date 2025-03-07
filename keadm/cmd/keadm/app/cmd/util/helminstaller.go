package util

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	"helm.sh/helm/v3/pkg/strvals"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"sigs.k8s.io/yaml"

	keCharts "github.com/kubeedge/kubeedge/build/helm/charts"
	"github.com/kubeedge/kubeedge/common/constants"
	types "github.com/kubeedge/kubeedge/keadm/cmd/keadm/app/cmd/common"
)

const (
	CloudCoreHelmComponent = "cloudcore"
	CloudCoreSubDir        = "cloudcore"

	DefaultBaseHelmDir   = ""
	DefaultAddonsHelmDir = "addons"
	DefaultProfilesDir   = "profiles"
	// DefaultProfileFilename is the name of the default profile yaml file.
	DefaultProfileFilename = "version.yaml"
	DefaultHelmValuesPath  = "values.yaml"

	DefaultHelmTimeout = time.Duration(60 * time.Second)

	DefaultHelmInstall  = true
	DefaultHelmWait     = true
	DefaultHelmCreateNs = true
)

var (
	ErrListProfiles = errors.New("can not list profiles")
)

// KubeCloudHelmInstTool embeds Common struct
// It implements ToolsInstaller interface
type KubeCloudHelmInstTool struct {
	Common
	AdvertiseAddress string
	Manifests        string
	Namespace        string
	CloudcoreImage   string
	CloudcoreTag     string
	IptablesMgrImage string
	IptablesMgrTag   string
	Sets             []string
	Profile          string
	ProfileKey       string
	ExternalHelmRoot string
	Force            bool
	SkipCRDs         bool
	DryRun           bool
	Action           string
	existsProfile    bool
}

// InstallTools downloads KubeEdge for the specified version
// and makes the required configuration changes and initiates cloudcore.
func (cu *KubeCloudHelmInstTool) InstallTools() error {
	cu.SetOSInterface(GetOSInterface())
	cu.SetKubeEdgeVersion(cu.ToolVersion)

	baseHelmRoot := DefaultBaseHelmDir
	if cu.ExternalHelmRoot != "" {
		baseHelmRoot = cu.ExternalHelmRoot
	}

	switch cu.Action {
	case types.HelmInstallAction:
		if err := cu.RunHelmInstall(baseHelmRoot); err != nil {
			return err
		}
	case types.HelmManifestAction:
		if err := cu.RunHelmManifest(baseHelmRoot); err != nil {
			return err
		}
	default:
		fmt.Println("Not support this action")
	}

	return nil
}

// RunHelmInstall renders the Charts with the given values, then installs the Charts to the cluster.
func (cu *KubeCloudHelmInstTool) RunHelmInstall(baseHelmRoot string) error {
	// --force would not care about whether the cloud components exist or not
	// Also, if gives a external helm root, no need to check and verify. Because it is always not a cloudcore.
	if !cu.Force && cu.ExternalHelmRoot == "" {
		cloudCoreRunning, err := cu.IsKubeEdgeProcessRunning(KubeCloudBinaryName)
		if err != nil {
			return err
		}
		if cloudCoreRunning {
			return fmt.Errorf("CloudCore is already running on this node, please run reset to clean up first")
		}
	}

	err := cu.IsK8SComponentInstalled(cu.KubeConfig, cu.Master)
	if err != nil {
		return err
	}

	fmt.Println("Kubernetes version verification passed, KubeEdge installation will start...")

	// prepare to render
	if err := cu.beforeRenderer(baseHelmRoot); err != nil {
		return err
	}

	// build a renderer instance with the given values and flagvals
	renderer, err := cu.buildRenderer(baseHelmRoot)
	if err != nil {
		return fmt.Errorf("cannot build renderer: %s", err.Error())
	}

	release, err := cu.runHelmInstall(renderer)
	if err != nil {
		return err
	}

	if release == nil {
		return fmt.Errorf("release is empty point")
	}

	fmt.Printf("%s started\n", strings.ToTitle(renderer.componentName))

	fmt.Printf("=========CHART DETAILS=======\n")
	fmt.Printf("NAME: %s\n", release.Name)
	if !release.Info.LastDeployed.IsZero() {
		fmt.Printf("LAST DEPLOYED: %s\n", release.Info.LastDeployed.Format(time.ANSIC))
	}
	fmt.Printf("NAMESPACE: %s\n", release.Namespace)
	fmt.Printf("STATUS: %s\n", release.Info.Status.String())
	fmt.Printf("REVISION: %d\n", release.Version)

	return nil
}

func (cu *KubeCloudHelmInstTool) RunHelmManifest(baseHelmRoot string) error {
	// prepare to render
	if err := cu.beforeRenderer(baseHelmRoot); err != nil {
		return err
	}

	// build a renderer instance with the given values and flagvals
	renderer, err := cu.buildRenderer(baseHelmRoot)
	if err != nil {
		return fmt.Errorf("cannot build renderer: %s", err.Error())
	}

	if err := cu.runHelmManifest(renderer, os.Stdout); err != nil {
		return err
	}

	return nil
}

// beforeRenderer handles the value of the profile.
func (cu *KubeCloudHelmInstTool) beforeRenderer(baseHelmRoot string) error {
	if cu.Profile == "" {
		cu.Profile = fmt.Sprintf("%s=%s", types.VersionProfileKey, types.HelmDefaultVersion)
	}
	// profile must be invalid
	p := strings.Split(cu.Profile, "=")
	cu.ProfileKey = p[0]

	// check profile if the {baseHelmRoot}/profiles/{profileKey}.yaml exists
	if err := cu.checkProfile(baseHelmRoot); err != nil {
		if errors.Is(err, ErrListProfiles) {
			cu.existsProfile = false
			return nil
		}

		return fmt.Errorf("invalid profile key %s, err: %s", cu.ProfileKey, err.Error())
	}

	cu.existsProfile = true

	// Only handle profiles when cu.ExternalHelmRoot is empty.
	if cu.ExternalHelmRoot == "" {
		var profileValue string
		if len(p) >= 2 {
			profileValue = p[1]
		}
		if err := cu.handleProfile(profileValue); err != nil {
			return fmt.Errorf("can not handle profile %s", cu.Profile)
		}

		// combine the flag values
		if cu.AdvertiseAddress != "" {
			for index, addr := range strings.Split(cu.AdvertiseAddress, ",") {
				cu.Sets = append(cu.Sets, fmt.Sprintf("%s[%d]=%s", "cloudCore.modules.cloudHub.advertiseAddress", index, addr))
			}
		}
		if cu.CloudcoreImage != "" {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "cloudCore.image.repository", cu.CloudcoreImage))
		}
		if cu.CloudcoreTag != "" {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "cloudCore.image.tag", cu.CloudcoreTag))
		}
		if cu.IptablesMgrImage != "" {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "iptablesManager.image.repository", cu.IptablesMgrImage))
		}
		if cu.IptablesMgrTag != "" {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "iptablesManager.image.tag", cu.IptablesMgrTag))
		}
	}

	// rebuild flag values
	if err := cu.rebuildFlagVals(); err != nil {
		return err
	}

	return nil
}

// buildRenderer returns a renderer instance
func (cu *KubeCloudHelmInstTool) buildRenderer(baseHelmRoot string) (*Renderer, error) {
	profileValsMap, err := cu.combineProfVals()
	if err != nil {
		return nil, err
	}
	// confirm which chart to load
	var componentName string
	var subDir string
	if cu.existsProfile && cu.isInnerProfile() {
		switch cu.ProfileKey {
		case types.VersionProfileKey, types.IptablesMgrProfileKey:
			componentName = CloudCoreHelmComponent
			subDir = CloudCoreSubDir
		// we can implement edgemesh here later.
		default:
			// By default, will search charts in addons dir.
			componentName = cu.ProfileKey
			subDir = fmt.Sprintf("%s/%s", DefaultAddonsHelmDir, cu.ProfileKey)
		}
	} else {
		// handle external chart
		componentName = cu.ProfileKey
		subDir = cu.ProfileKey
	}

	// returns the renderer instance
	renderer := NewGenericRenderer(keCharts.BuiltinOrDir(baseHelmRoot), subDir, componentName, cu.Namespace, profileValsMap, cu.SkipCRDs)

	// load the charts to this renderer
	if err := renderer.LoadChart(); err != nil {
		return nil, fmt.Errorf("cannot load the given charts %s, error: %s", renderer.componentName, err.Error())
	}

	return renderer, nil
}

// runHelmManifest renders k8s manifests with the given flags
func (cu *KubeCloudHelmInstTool) runHelmManifest(r *Renderer, stdout io.Writer) error {
	manifests, err := r.RenderManifest()
	if err != nil {
		return fmt.Errorf("cannot render the given compoent %s, error: %s", r.componentName, err.Error())
	}

	// combine the given manifests and the rendered manifests
	var buf bytes.Buffer
	if cu.Manifests != "" {
		for _, manifest := range strings.Split(cu.Manifests, ",") {
			body, err := ioutil.ReadFile(manifest)
			if err != nil {
				return fmt.Errorf("cannot open file %s, error: %s", manifest, err.Error())
			}
			buf.WriteString(fmt.Sprintf("%b%s", body, YAMLSeparator))
		}
	}
	buf.WriteString(manifests)

	stdout.Write(buf.Bytes())
	return nil
}

// runHelmInstall starts cloudcore deployment with the given flags
func (cu *KubeCloudHelmInstTool) runHelmInstall(r *Renderer) (*release.Release, error) {
	cf := genericclioptions.NewConfigFlags(true)
	cf.KubeConfig = &cu.KubeConfig
	cf.Namespace = &cu.Namespace

	cfg := &action.Configuration{}
	// let the os.Stdout not print the details
	logFunc := func(format string, v ...interface{}) {}
	if err := cfg.Init(cf, cu.Namespace, "", logFunc); err != nil {
		return nil, err
	}

	// a flag to confirm the install/upgrade action
	var performInstall bool
	_, err := cfg.Releases.Last(r.componentName)
	if err != nil && errors.Is(err, driver.ErrReleaseNotFound) {
		performInstall = true
	}

	if performInstall {
		helmInstall := action.NewInstall(cfg)
		helmInstall.DryRun = cu.DryRun
		helmInstall.Namespace = cu.Namespace
		// --force would not wait.
		if !cu.Force {
			helmInstall.Wait = DefaultHelmWait
			helmInstall.Timeout = DefaultHelmTimeout
		}
		helmInstall.CreateNamespace = DefaultHelmCreateNs
		helmInstall.ReleaseName = r.componentName

		rel, err := helmInstall.Run(r.chart, r.profileValsMap)
		if err != nil {
			return nil, err
		}
		return rel, nil
	}

	// try to update a version
	helmUpgrade := action.NewUpgrade(cfg)
	helmUpgrade.DryRun = cu.DryRun
	helmUpgrade.Namespace = cu.Namespace
	// --force would not wait.
	if !cu.Force {
		helmUpgrade.Wait = DefaultHelmWait
		helmUpgrade.Timeout = DefaultHelmTimeout
	}

	rel, err := helmUpgrade.Run(r.componentName, r.chart, r.profileValsMap)
	if err != nil {
		return nil, err
	}
	return rel, nil
}

// TearDown method will remove the edge node from api-server and stop cloudcore process
func (cu *KubeCloudHelmInstTool) TearDown() error {
	// clean kubeedge namespace
	err := cu.cleanNameSpace(constants.SystemNamespace, cu.KubeConfig)
	if err != nil {
		return fmt.Errorf("fail to clean kubeedge namespace, err:%v", err)
	}
	return nil
}

func (cu *KubeCloudHelmInstTool) checkProfile(baseHelmRoot string) error {
	// read external profiles
	validProfiles, err := cu.readProfiles(baseHelmRoot, DefaultProfilesDir)
	if err != nil {
		return ErrListProfiles
	}

	// iptalesmgr is also an valid profile key.
	validProfiles[types.IptablesMgrProfileKey] = true
	if ok := validProfiles[cu.ProfileKey]; !ok {
		validKeys := make([]string, len(validProfiles))
		for k := range validProfiles {
			validKeys = append(validKeys, k)
		}
		return fmt.Errorf(fmt.Sprintf("profile %s not in %s", cu.ProfileKey, strings.Join(validKeys, ",")))
	}

	return nil
}

// handleProfile only handles inner profile
func (cu *KubeCloudHelmInstTool) handleProfile(profileValue string) error {
	switch cu.ProfileKey {
	case types.VersionProfileKey:
		if profileValue == "" {
			profileValue = types.HelmDefaultVersion
		}
		profileValueSuffix := strings.TrimPrefix(profileValue, "v")
		// confirm it startswith "v"
		if profileValue != profileValueSuffix {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=v%s", "cloudCore.image.tag", profileValueSuffix))
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=v%s", "iptablesManager.image.tag", profileValueSuffix))
		} else {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "cloudCore.image.tag", profileValue))
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "iptablesManager.image.tag", profileValue))
		}
	case types.IptablesMgrProfileKey:
		if profileValue == "" {
			profileValue = types.ExternalIptablesMgrMode
		}

		if profileValue == types.InternalIptablesMgrMode || profileValue == types.ExternalIptablesMgrMode {
			cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", "iptablesManager.mode", profileValue))
			return nil
		}

		return fmt.Errorf("the given mode of iptablesmgr %s is not supported, only support internal or external", profileValue)
	default:
	}

	return nil
}

func (cu *KubeCloudHelmInstTool) rebuildFlagVals() error {
	unDuplicatedStore := make(map[string]string)

	for _, s := range cu.Sets {
		p := strings.Split(s, "=")

		if len(p) < 2 {
			fmt.Println("Unsupported flags:", s)
			continue
		}

		unDuplicatedStore[p[0]] = p[1]
	}

	for k, v := range unDuplicatedStore {
		cu.Sets = append(cu.Sets, fmt.Sprintf("%s=%s", k, v))
	}

	return nil
}

func (cu *KubeCloudHelmInstTool) isInnerProfile() bool {
	return cu.ProfileKey == "" || cu.ProfileKey == DefaultProfileString || cu.ProfileKey == types.IptablesMgrProfileKey
}

// combineProfVals combines the values of the given manifests and flags into a map.
func (cu *KubeCloudHelmInstTool) combineProfVals() (map[string]interface{}, error) {
	profileValsMap := map[string]interface{}{}

	profilekey := cu.ProfileKey
	if profilekey == types.IptablesMgrProfileKey {
		profilekey = types.VersionProfileKey
	}
	profileValue, err := loadValues(cu.ExternalHelmRoot, profilekey, cu.existsProfile)
	if err != nil {
		return nil, fmt.Errorf("cannot load profile yaml:%s", err.Error())
	}

	if err := yaml.Unmarshal([]byte(profileValue), &profileValsMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal values: %v", err)
	}
	// User specified a value via --set
	for _, value := range cu.Sets {
		if err := strvals.ParseInto(value, profileValsMap); err != nil {
			return nil, fmt.Errorf("failed parsing --set data:%s", err.Error())
		}
	}

	return profileValsMap, nil
}

func (cu *KubeCloudHelmInstTool) readProfiles(baseHelmDir, profilesDir string) (map[string]bool, error) {
	validProfiles := make(map[string]bool)

	f := keCharts.BuiltinOrDir(baseHelmDir)
	dir, err := fs.ReadDir(f, profilesDir)
	if err != nil {
		return nil, err
	}
	for _, f := range dir {
		trimmedString := strings.TrimSuffix(f.Name(), ".yaml")
		if f.Name() != trimmedString && trimmedString != "" {
			validProfiles[trimmedString] = true
		}
	}

	return validProfiles, nil
}

func builtinProfileToFilename(name string) string {
	if name == "" {
		return DefaultProfileFilename
	}
	return name + ".yaml"
}

func loadValues(chartsDir string, profileKey string, existsProfile bool) (string, error) {
	var path string
	if existsProfile {
		path = strings.Join([]string{DefaultProfilesDir, builtinProfileToFilename(profileKey)}, "/")
	} else {
		path = strings.Join([]string{profileKey, DefaultHelmValuesPath}, "/")
	}
	by, err := fs.ReadFile(keCharts.BuiltinOrDir(chartsDir), path)
	if err != nil {
		return "", err
	}
	return string(by), nil
}
