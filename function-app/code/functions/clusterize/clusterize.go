package clusterize

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"weka-deployment/common"
	"weka-deployment/functions/azure_functions_def"

	"github.com/lithammer/dedent"

	"github.com/weka/go-cloud-lib/clusterize"
	cloudCommon "github.com/weka/go-cloud-lib/common"
	"github.com/weka/go-cloud-lib/functions_def"
	"github.com/weka/go-cloud-lib/logging"
	"github.com/weka/go-cloud-lib/protocol"
)

type AzureObsParams struct {
	Name              string
	ContainerName     string
	AccessKey         string
	TieringSsdPercent string
}

func GetObsScript(obsParams AzureObsParams) string {
	template := `
	TIERING_SSD_PERCENT=%s
	OBS_NAME=%s
	OBS_CONTAINER_NAME=%s
	OBS_BLOB_KEY=%s

	weka fs tier s3 add azure-obs --site local --obs-name default-local --obs-type AZURE --hostname $OBS_NAME.blob.core.windows.net --port 443 --bucket $OBS_CONTAINER_NAME --access-key-id $OBS_NAME --secret-key $OBS_BLOB_KEY --protocol https --auth-method AWSSignature4
	weka fs tier s3 attach default azure-obs
	tiering_percent=$(echo "$full_capacity * 100 / $TIERING_SSD_PERCENT" | bc)
	weka fs update default --total-capacity "$tiering_percent"B
	`
	return fmt.Sprintf(
		dedent.Dedent(template), obsParams.TieringSsdPercent, obsParams.Name, obsParams.ContainerName, obsParams.AccessKey,
	)
}

func GetWekaDebugOverrideCmds() string {
	s := `
	weka debug override add --key allow_uncomputed_backend_checksum
	weka debug override add --key allow_azure_auto_detection
	`
	return dedent.Dedent(s)
}

type ClusterizationParams struct {
	SubscriptionId    string
	ResourceGroupName string
	Location          string
	Prefix            string
	KeyVaultUri       string

	StateContainerName string
	StateStorageName   string
	InstallDpdk        bool

	VmName  string
	Cluster clusterize.ClusterParams
	Obs     AzureObsParams

	FunctionAppName string
}

type RequestBody struct {
	Vm string `json:"vm"`
}

func GetErrorScript(err error) string {
	return fmt.Sprintf(`
#!/bin/bash
<<'###ERROR'
%s
###ERROR
exit 1
	`, err.Error())
}

func GetShutdownScript() string {
	s := `
	#!/bin/bash
	shutdown now
	`
	return dedent.Dedent(s)
}

func HandleLastClusterVm(ctx context.Context, state protocol.ClusterState, p ClusterizationParams, funcDef functions_def.FunctionDef) (clusterizeScript string, err error) {
	logger := logging.LoggerFromCtx(ctx)
	logger.Info().Msg("This is the last instance in the cluster, creating obs and clusterization script")

	vmScaleSetName := common.GetVmScaleSetName(p.Prefix, p.Cluster.ClusterName)

	if p.Cluster.SetObs {
		if p.Obs.AccessKey == "" {
			p.Obs.AccessKey, err = common.CreateStorageAccount(
				ctx, p.SubscriptionId, p.ResourceGroupName, p.Obs.Name, p.Location,
			)
			if err != nil {
				err = fmt.Errorf("failed to create storage account: %w", err)
				logger.Error().Err(err).Send()
				return
			}

			err = common.CreateContainer(ctx, p.Obs.Name, p.Obs.ContainerName)
			if err != nil {
				err = fmt.Errorf("failed to create container: %w", err)
				logger.Error().Err(err).Send()
				return
			}
		}

		_, err = common.AssignStorageBlobDataContributorRoleToScaleSet(
			ctx, p.SubscriptionId, p.ResourceGroupName, vmScaleSetName, p.Obs.Name, p.Obs.ContainerName,
		)
		if err != nil {
			err = fmt.Errorf("failed to assign storage blob data contributor role to scale set: %w", err)
			logger.Error().Err(err).Send()
			return
		}
	}

	wekaPassword, err := common.GetWekaClusterPassword(ctx, p.KeyVaultUri)
	if err != nil {
		err = fmt.Errorf("failed to get weka cluster password: %w", err)
		logger.Error().Err(err).Send()
		return
	}

	vmsPrivateIps, err := common.GetVmsPrivateIps(ctx, p.SubscriptionId, p.ResourceGroupName, vmScaleSetName)
	if err != nil {
		err = fmt.Errorf("failed to get vms private ips: %w", err)
		logger.Error().Err(err).Send()
		return
	}

	var vmNamesList []string
	// we make the ips list compatible to vmNames
	var ipsList []string
	for _, instance := range state.Instances {
		vm := strings.Split(instance, ":")
		ipsList = append(ipsList, vmsPrivateIps[vm[0]])
		vmNamesList = append(vmNamesList, vm[1])
	}

	logger.Info().Msg("Generating clusterization script")

	clusterParams := p.Cluster
	clusterParams.VMNames = vmNamesList
	clusterParams.IPs = ipsList
	clusterParams.ObsScript = GetObsScript(p.Obs)
	clusterParams.DebugOverrideCmds = GetWekaDebugOverrideCmds()
	clusterParams.WekaPassword = wekaPassword
	clusterParams.WekaUsername = "admin"
	clusterParams.InstallDpdk = p.InstallDpdk
	clusterParams.FindDrivesScript = common.FindDrivesScript

	scriptGenerator := clusterize.ClusterizeScriptGenerator{
		Params:  clusterParams,
		FuncDef: funcDef,
	}
	clusterizeScript = scriptGenerator.GetClusterizeScript()

	logger.Info().Msg("Clusterization script generated")
	return
}

func Clusterize(ctx context.Context, p ClusterizationParams) (clusterizeScript string) {
	logger := logging.LoggerFromCtx(ctx)

	instanceName := strings.Split(p.VmName, ":")[0]
	instanceId := common.GetScaleSetVmIndex(instanceName)
	vmScaleSetName := common.GetVmScaleSetName(p.Prefix, p.Cluster.ClusterName)
	vmName := p.VmName

	ip, err := common.GetPublicIp(ctx, p.SubscriptionId, p.ResourceGroupName, vmScaleSetName, p.Prefix, p.Cluster.ClusterName, instanceId)
	if err != nil {
		logger.Error().Msg("Failed to fetch public ip")
	} else {
		vmName = fmt.Sprintf("%s:%s", vmName, ip)
	}

	state, err := common.AddInstanceToState(
		ctx, p.SubscriptionId, p.ResourceGroupName, p.StateStorageName, p.StateContainerName, vmName,
	)

	if err != nil {
		if _, ok := err.(*common.ShutdownRequired); ok {
			clusterizeScript = GetShutdownScript()
		} else {
			clusterizeScript = GetErrorScript(err)
		}
		return
	}

	functionAppKey, err := common.GetKeyVaultValue(ctx, p.KeyVaultUri, "function-app-default-key")
	if err != nil {
		clusterizeScript = GetErrorScript(err)
		return
	}

	baseFunctionUrl := fmt.Sprintf("https://%s.azurewebsites.net/api/", p.FunctionAppName)
	funcDef := azure_functions_def.NewFuncDef(baseFunctionUrl, functionAppKey)
	reportFunction := funcDef.GetFunctionCmdDefinition(functions_def.Report)

	if len(state.Instances) == p.Cluster.HostsNum {
		clusterizeScript, err = HandleLastClusterVm(ctx, state, p, funcDef)
		if err != nil {
			clusterizeScript = cloudCommon.GetErrorScript(err, reportFunction)
		}
	} else {
		msg := fmt.Sprintf("This (%s) is instance %d/%d that is ready for clusterization", instanceName, len(state.Instances), p.Cluster.HostsNum)
		logger.Info().Msgf(msg)
		clusterizeScript = cloudCommon.GetScriptWithReport(msg, reportFunction)
	}
	return
}

func Handler(w http.ResponseWriter, r *http.Request) {
	stateContainerName := os.Getenv("STATE_CONTAINER_NAME")
	stateStorageName := os.Getenv("STATE_STORAGE_NAME")
	hostsNum, _ := strconv.Atoi(os.Getenv("HOSTS_NUM"))
	clusterName := os.Getenv("CLUSTER_NAME")
	subscriptionId := os.Getenv("SUBSCRIPTION_ID")
	resourceGroupName := os.Getenv("RESOURCE_GROUP_NAME")
	setObs, _ := strconv.ParseBool(os.Getenv("SET_OBS"))
	smbwEnabled, _ := strconv.ParseBool(os.Getenv("SMBW_ENABLED"))
	obsName := os.Getenv("OBS_NAME")
	obsContainerName := os.Getenv("OBS_CONTAINER_NAME")
	obsAccessKey := os.Getenv("OBS_ACCESS_KEY")
	location := os.Getenv("LOCATION")
	nvmesNum, _ := strconv.Atoi(os.Getenv("NVMES_NUM"))
	tieringSsdPercent := os.Getenv("TIERING_SSD_PERCENT")
	prefix := os.Getenv("PREFIX")
	keyVaultUri := os.Getenv("KEY_VAULT_URI")
	// data protection-related vars
	stripeWidth, _ := strconv.Atoi(os.Getenv("STRIPE_WIDTH"))
	protectionLevel, _ := strconv.Atoi(os.Getenv("PROTECTION_LEVEL"))
	hotspare, _ := strconv.Atoi(os.Getenv("HOTSPARE"))
	installDpdk, _ := strconv.ParseBool(os.Getenv("INSTALL_DPDK"))
	addFrontendNum, _ := strconv.Atoi(os.Getenv("NUM_FRONTEND_CONTAINERS"))
	functionAppName := os.Getenv("FUNCTION_APP_NAME")
	proxyUrl := os.Getenv("PROXY_URL")
	wekaHomeUrl := os.Getenv("WEKA_HOME_URL")

	addFrontend := false
	if addFrontendNum > 0 {
		addFrontend = true
	}

	outputs := make(map[string]interface{})
	resData := make(map[string]interface{})
	var invokeRequest common.InvokeRequest

	ctx := r.Context()
	logger := logging.LoggerFromCtx(ctx)

	d := json.NewDecoder(r.Body)
	err := d.Decode(&invokeRequest)
	if err != nil {
		logger.Error().Msg("Bad request")
		return
	}

	var reqData map[string]interface{}
	err = json.Unmarshal(invokeRequest.Data["req"], &reqData)
	if err != nil {
		logger.Error().Msg("Bad request")
		return
	}

	var data RequestBody

	if json.Unmarshal([]byte(reqData["Body"].(string)), &data) != nil {
		logger.Error().Msg("Bad request")
		return
	}

	params := ClusterizationParams{
		SubscriptionId:     subscriptionId,
		ResourceGroupName:  resourceGroupName,
		Location:           location,
		Prefix:             prefix,
		KeyVaultUri:        keyVaultUri,
		StateContainerName: stateContainerName,
		StateStorageName:   stateStorageName,
		VmName:             data.Vm,
		InstallDpdk:        installDpdk,
		Cluster: clusterize.ClusterParams{
			HostsNum:    hostsNum,
			ClusterName: clusterName,
			NvmesNum:    nvmesNum,
			SetObs:      setObs,
			SmbwEnabled: smbwEnabled,
			AddFrontend: addFrontend,
			ProxyUrl:    proxyUrl,
			WekaHomeUrl: wekaHomeUrl,
			DataProtection: clusterize.DataProtectionParams{
				StripeWidth:     stripeWidth,
				ProtectionLevel: protectionLevel,
				Hotspare:        hotspare,
			},
		},
		Obs: AzureObsParams{
			Name:              obsName,
			ContainerName:     obsContainerName,
			AccessKey:         obsAccessKey,
			TieringSsdPercent: tieringSsdPercent,
		},
		FunctionAppName: functionAppName,
	}

	if data.Vm == "" {
		msg := "Cluster name wasn't supplied"
		logger.Error().Msgf(msg)
		resData["body"] = msg
	} else {
		clusterizeScript := Clusterize(ctx, params)
		resData["body"] = clusterizeScript
	}
	outputs["res"] = resData
	invokeResponse := common.InvokeResponse{Outputs: outputs, Logs: nil, ReturnValue: nil}

	responseJson, _ := json.Marshal(invokeResponse)

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}
