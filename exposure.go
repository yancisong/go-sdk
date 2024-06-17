// Package abc provides a set of APIs for external use, including APIs for ABC system initialization.
// It also encompasses functionalities such as traffic distribution for A/B experiments,
// user configuration data retrieval, user feature flag management, exposure data reporting, and logger registration.
package abc

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/abetterchoice/go-sdk/env"
	"github.com/abetterchoice/go-sdk/internal"
	"github.com/abetterchoice/go-sdk/internal/cache"
	"github.com/abetterchoice/go-sdk/plugin/log"
	"github.com/abetterchoice/go-sdk/plugin/metrics"
	"github.com/abetterchoice/protoc_event_server"
)

const (
	// When unitID has the alias newUnitID,
	// it will be reported to the extended field of the exposure record and stored in kv format.
	// The key is newIDKey and the value is newUnitID.
	newIDKey = "new_id"
)

// LogExperimentsExposure When automatic exposure-logging is disabled,
// // this API can be utilized to manually log exposure.
// In certain scenarios where it is necessary to first call the experiment,
// // managing exposure logging in this manner can assist in preventing the potential over-exposure issue
// that may arise from automatic exposure logging.
func LogExperimentsExposure(ctx context.Context, projectID string, list *ExperimentList) error {
	// User records exposure manually
	return exposureExperiments(ctx, projectID, list, protoc_event_server.ExposureType_EXPOSURE_TYPE_MANUAL)
}

// LogExperimentExposure When automatic exposure-logging is disabled,
// this API can be utilized to manually log exposure. In certain scenarios
// where it is necessary to first call the experiment,
// managing exposure logging in this manner can assist in preventing the potential
// over-exposure issue that may arise from automatic exposure logging.
func LogExperimentExposure(ctx context.Context, projectID string, result *ExperimentResult) error {
	if result == nil || result.userCtx == nil || result.Group == nil {
		return nil
	}
	return exposureExperiments(ctx, projectID, &ExperimentList{
		userCtx: result.userCtx,
		Data: map[string]*Group{
			result.LayerKey: result.Group,
		},
	}, protoc_event_server.ExposureType_EXPOSURE_TYPE_MANUAL)
}

// LogFeatureFlagExposure The incoming featureFlag is generated by GetFeatureFlag.
func LogFeatureFlagExposure(ctx context.Context, projectID string, featureFlag *FeatureFlag) error {
	return exposureFeatureFlag(ctx, projectID, featureFlag, protoc_event_server.ExposureType_EXPOSURE_TYPE_MANUAL)
}

// LogRemoteConfigExposure The incoming config is generated by GetRemoteConfig.
func LogRemoteConfigExposure(ctx context.Context, projectID string, config *ConfigResult) error {
	return exposureRemoteConfig(ctx, projectID, config, protoc_event_server.ExposureType_EXPOSURE_TYPE_MANUAL)
}

// exposureExperimentEvent experimental diversion events
func exposureExperimentEvent(ctx context.Context, projectID string, list *ExperimentList,
	latency time.Duration, optionStr string, err error) error {
	// Get monitoring and reporting plug-in information
	application := cache.GetApplication(projectID)
	if application == nil {
		return nil
	}
	metricsConfig := application.TabConfig.ControlData.EventMetricsConfig
	if metricsConfig == nil || !metricsConfig.IsEnable || metricsConfig.Metadata == nil {
		return nil
	}
	// Sampling first, the frequency of event reporting is not high, sampling first improves efficiency
	if !metrics.SamplingResult(env.SamplingInterval(metricsConfig, err)) {
		return nil // 采样不通过
	}
	return metrics.LogMonitorEvent(ctx, &metrics.Metadata{
		MetricsPluginName: metricsConfig.PluginName,
		TableName:         metricsConfig.Metadata.Name,
		TableID:           metricsConfig.Metadata.Id,
		Token:             metricsConfig.Metadata.Token,
		SamplingInterval:  1, // 已经先采样了，这里恒上报
	}, &protoc_event_server.MonitorEventGroup{Events: []*protoc_event_server.MonitorEvent{
		{
			Time:       time.Now().Unix(),
			Ip:         env.LocalIP(),
			ProjectId:  projectID,
			EventName:  "exp",
			Latency:    float32(latency.Microseconds()), // us
			StatusCode: env.EventStatus(err),
			Message:    env.ErrMsg(err),
			SdkType:    env.SDKType,
			SdkVersion: env.Version,
			InvokePath: env.InvokePath(4), // 跳过 4 层调用栈
			InputData:  optionStr,
			OutputData: experimentIDList(list),
			ExtInfo:    nil,
		},
	}})
}

// exposureRemoteConfigEvent Report remote configuration acquisition events
func exposureRemoteConfigEvent(ctx context.Context, projectID string, config *ConfigResult,
	latency time.Duration, optionStr string, err error) error {
	// Get monitoring and reporting plug-in information
	application := cache.GetApplication(projectID)
	if application == nil {
		return nil
	}
	metricsConfig := application.TabConfig.ControlData.EventMetricsConfig
	if metricsConfig == nil || !metricsConfig.IsEnable || metricsConfig.Metadata == nil {
		return nil
	}
	if !metrics.SamplingResult(env.SamplingInterval(metricsConfig, err)) {
		return nil
	}
	// Report data
	var resultData string
	if config != nil {
		resultData = string(config.data)
	}
	return metrics.LogMonitorEvent(ctx, &metrics.Metadata{
		MetricsPluginName: metricsConfig.PluginName,
		TableName:         metricsConfig.Metadata.Name,
		TableID:           metricsConfig.Metadata.Id,
		Token:             metricsConfig.Metadata.Token,
		SamplingInterval:  1, // 已经先采样了，这里恒上报
	}, &protoc_event_server.MonitorEventGroup{Events: []*protoc_event_server.MonitorEvent{
		{
			Time:       time.Now().Unix(),
			Ip:         env.LocalIP(),
			ProjectId:  projectID,
			EventName:  "rc",
			Latency:    float32(latency.Microseconds()), // us
			StatusCode: env.EventStatus(err),
			Message:    env.ErrMsg(err),
			SdkType:    env.SDKType,
			SdkVersion: env.Version,
			InvokePath: env.InvokePath(4), // 跳过 4 层调用栈
			InputData:  optionStr,
			OutputData: resultData,
			ExtInfo:    nil,
		},
	}})
}

// experimentIDList of experimental group IDs, separated by ; sign
func experimentIDList(list *ExperimentList) string {
	if list == nil { // 有可能是错误上报，list 可能为空
		return ""
	}
	var idList = make([]int64, 0, len(list.Data))
	for _, e := range list.Data {
		idList = append(idList, e.ID)
	}
	return int64ListJoin(idList, ";")
}

// exposureExperiments TODO
// Specific implementation of experimental exposure reporting
func exposureExperiments(ctx context.Context, projectID string, list *ExperimentList,
	exposureType protoc_event_server.ExposureType) error {
	// Whether to disable
	if internal.C.IsDisableReport {
		return nil
	}
	if list == nil || len(list.Data) == 0 { // 没有数据
		return nil
	}
	// Get local cache
	application := cache.GetApplication(projectID)
	if application == nil { // 理论上不为 nil
		return nil
	}
	experimentMetricsConfigList := application.TabConfig.ControlData.ExperimentMetricsConfig
	defaultExperimentMetricsConfig := application.TabConfig.ControlData.DefaultExperimentMetricsConfig
	if len(experimentMetricsConfigList) == 0 && defaultExperimentMetricsConfig == nil { // 没有监控上报配置
		return nil
	}
	ignoreReportGroupID := application.TabConfig.ControlData.IgnoreReportGroupId
	// Get reported data
	sceneDataList, defaultDataList := convertExperimentList(projectID, list, exposureType, ignoreReportGroupID)
	for sceneID, dataList := range sceneDataList {
		metricsConfig, ok := experimentMetricsConfigList[sceneID]
		if !ok || metricsConfig == nil {
			defaultDataList.Exposures = append(defaultDataList.Exposures, dataList.Exposures...)
			continue
		}
		if !metricsConfig.IsEnable || metricsConfig.Metadata == nil {
			continue
		}
		err := metrics.LogExposure(ctx, &metrics.Metadata{
			MetricsPluginName: metricsConfig.PluginName,
			TableName:         metricsConfig.Metadata.Name,
			TableID:           metricsConfig.Metadata.Id,
			Token:             metricsConfig.Metadata.Token,
			SamplingInterval:  metricsConfig.SamplingInterval,
		}, dataList)
		if err != nil {
			log.Errorf("sendData fail:%v", err)
			return err
		}
	}
	if defaultExperimentMetricsConfig == nil || !defaultExperimentMetricsConfig.IsEnable ||
		defaultExperimentMetricsConfig.Metadata == nil {
		return nil
	}
	return metrics.LogExposure(ctx, &metrics.Metadata{
		MetricsPluginName: defaultExperimentMetricsConfig.PluginName,
		TableName:         defaultExperimentMetricsConfig.Metadata.Name,
		TableID:           defaultExperimentMetricsConfig.Metadata.Id,
		Token:             defaultExperimentMetricsConfig.Metadata.Token,
		SamplingInterval:  defaultExperimentMetricsConfig.SamplingInterval,
	}, defaultDataList)
}

// exposureFeatureFlag TODO
// Specific implementation of remote configuration exposure reporting
func exposureFeatureFlag(ctx context.Context, projectID string, featureFlag *FeatureFlag,
	exposureType protoc_event_server.ExposureType) error {
	// Whether to disable
	if internal.C.IsDisableReport {
		return nil
	}
	if featureFlag == nil || featureFlag.ConfigResult == nil { // 没有数据
		return nil
	}
	config := featureFlag.ConfigResult
	// Get local cache
	application := cache.GetApplication(projectID)
	if application == nil { // 理论上不为 nil
		return nil
	}
	metricsConfigList := application.TabConfig.ControlData.FeatureFlagMetricsConfig
	defaultMetricsConfig := application.TabConfig.ControlData.DefaultFeatureFlagMetricsConfig
	if len(metricsConfigList) == 0 && defaultMetricsConfig == nil {
		return nil
	}
	// Whether it has been reported through the specified scenario
	isSent := false
	data := convertRemoteConfig(projectID, config, exposureType) // Reuse remote configuration exposure reporting
	for _, sceneID := range config.remoteConfig.SceneIdList {
		metricsConfig, ok := metricsConfigList[sceneID]
		if !ok || metricsConfig == nil || metricsConfig.Metadata == nil {
			continue
		}
		if !metricsConfig.IsEnable {
			continue
		}
		err := metrics.SendData(ctx, &metrics.Metadata{
			MetricsPluginName: metricsConfig.PluginName,
			TableName:         metricsConfig.Metadata.Name,
			TableID:           metricsConfig.Metadata.Id,
			Token:             metricsConfig.Metadata.Token,
			SamplingInterval:  metricsConfig.SamplingInterval,
		}, [][]string{data})
		if err != nil {
			log.Errorf("sendData fail:%v", err)
			return err
		}
		// If you have reported through specified scenarios, you will no longer need to use default metrics to report.
		isSent = true
	}
	if isSent || defaultMetricsConfig == nil || !defaultMetricsConfig.IsEnable || defaultMetricsConfig.Metadata == nil {
		return nil
	}
	return metrics.SendData(ctx, &metrics.Metadata{
		MetricsPluginName: defaultMetricsConfig.PluginName,
		TableName:         defaultMetricsConfig.Metadata.Name,
		TableID:           defaultMetricsConfig.Metadata.Id,
		Token:             defaultMetricsConfig.Metadata.Token,
		SamplingInterval:  defaultMetricsConfig.SamplingInterval,
	}, [][]string{data})
}

// exposureRemoteConfig 远程配置曝光上报具体实现
func exposureRemoteConfig(ctx context.Context, projectID string, config *ConfigResult,
	exposureType protoc_event_server.ExposureType) error {
	// Whether to disable
	if internal.C.IsDisableReport {
		return nil
	}
	if config == nil { // 没有数据
		return nil
	}
	// Get local cache
	application := cache.GetApplication(projectID)
	if application == nil { // 理论上不为 nil
		return nil
	}
	metricsConfigList := application.TabConfig.ControlData.RemoteConfigMetricsConfig
	defaultMetricsConfig := application.TabConfig.ControlData.DefaultRemoteConfigMetricsConfig
	if len(metricsConfigList) == 0 && defaultMetricsConfig == nil { // 没有监控上报配置
		return nil
	}
	// get reported data
	isSent := false // Whether it has been reported through the specified scenario
	data := convertRemoteConfig(projectID, config, exposureType)
	for _, sceneID := range config.remoteConfig.SceneIdList {
		metricsConfig, ok := metricsConfigList[sceneID]
		if !ok || metricsConfig == nil || metricsConfig.Metadata == nil {
			continue
		}
		if !metricsConfig.IsEnable {
			continue
		}
		err := metrics.SendData(ctx, &metrics.Metadata{
			MetricsPluginName: metricsConfig.PluginName,
			TableName:         metricsConfig.Metadata.Name,
			TableID:           metricsConfig.Metadata.Id,
			SamplingInterval:  metricsConfig.SamplingInterval,
			Token:             metricsConfig.Metadata.Token,
		}, [][]string{data})
		if err != nil {
			log.Errorf("sendData fail:%v", err)
			return err
		}
		isSent = true
	}
	if isSent || defaultMetricsConfig == nil || !defaultMetricsConfig.IsEnable || defaultMetricsConfig.Metadata == nil {
		return nil
	}
	return metrics.SendData(ctx, &metrics.Metadata{
		MetricsPluginName: defaultMetricsConfig.PluginName,
		TableName:         defaultMetricsConfig.Metadata.Name,
		TableID:           defaultMetricsConfig.Metadata.Id,
		Token:             defaultMetricsConfig.Metadata.Token,
		SamplingInterval:  defaultMetricsConfig.SamplingInterval,
	}, [][]string{data})
}

// convertExperimentList TODO
// Return the data to be reported in each scenario and the data to be reported without scenario
func convertExperimentList(projectID string, list *ExperimentList, exposureType protoc_event_server.ExposureType,
	ignoreReportGroupID map[int64]bool) (map[int64]*protoc_event_server.ExposureGroup,
	*protoc_event_server.ExposureGroup) {
	var result = make(map[int64]*protoc_event_server.ExposureGroup)
	var defaultDataList = &protoc_event_server.ExposureGroup{}
	uploadTime := time.Now().Unix()
	for _, e := range list.Data {
		// Filter experimental groups that are not reported
		if flag, ok := ignoreReportGroupID[e.ID]; ok && flag { // Filter and ignore reported experimental group IDs
			continue
		}
		if len(e.sceneIDList) == 0 {
			defaultDataList.Exposures = append(defaultDataList.Exposures, convertExperimentV2(projectID, e, list.userCtx,
				exposureType, uploadTime))
			continue
		}
		for _, sceneID := range e.sceneIDList {
			if result[sceneID] == nil {
				result[sceneID] = &protoc_event_server.ExposureGroup{}
			}
			result[sceneID].Exposures = append(result[sceneID].Exposures, convertExperimentV2(projectID, e, list.userCtx,
				exposureType, uploadTime))
		}
	}
	return result, defaultDataList
}

func convertExperimentV2(projectID string, experiment *Group, userCtx *userContext,
	exposureType protoc_event_server.ExposureType, uploadTime int64) *protoc_event_server.Exposure {
	return &protoc_event_server.Exposure{
		UnitId:       userCtx.unitID,
		GroupId:      experiment.ID,
		ProjectId:    projectID,
		Time:         uploadTime,
		LayerKey:     experiment.LayerKey,
		ExpKey:       experiment.ExperimentKey,
		UnitType:     strconv.FormatInt(int64(experiment.UnitIDType), 10),
		ClusterId:    userCtx.decisionID,
		SdkType:      env.SDKType,
		SdkVersion:   env.Version,
		ExposureType: exposureType,
		ExtraData:    extraDataFromUserCtx(userCtx),
	}
}

func convertRemoteConfig(projectID string, config *ConfigResult,
	exposureType protoc_event_server.ExposureType) []string {
	return []string{
		config.userCtx.unitID,                    // unitID
		projectID,                                // Business unique identifier
		config.Key,                               // Configuration name
		env.SDKVersion,                           // sdk version information
		string(config.data),                      // configuration value
		time.Now().Format("2006-01-02 15:04:05"), // upload time
		internal.C.EnvType,                       // environmental information
		fmt.Sprintf("%v", config.unitIDType),     // unitID type
		int64ListJoin(config.remoteConfig.SceneIdList, "#"), // Scene ID list
		exposureType.String(),                               // Recording exposure mode: manual, automatic
		marshalExpandedData(config.userCtx),                 // Expand information
	}
}

func marshalExpandedData(userCtx *userContext) string {
	if len(userCtx.expandedData) == 0 && len(userCtx.newUnitID) == 0 {
		return ""
	}
	var expandedData = make(map[string]string, len(userCtx.expandedData)+1)
	if len(userCtx.newUnitID) != 0 {
		expandedData[newIDKey] = userCtx.newUnitID
	}
	for key, value := range userCtx.expandedData {
		expandedData[key] = value
	}
	var keys = make([]string, 0, len(expandedData))
	for key := range expandedData {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sb := bytes.Buffer{}
	for _, key := range keys {
		if sb.Len() > 0 {
			sb.WriteString(";")
		}
		sb.WriteString(key + "=" + expandedData[key])
	}
	return sb.String()
}

func extraDataFromUserCtx(userCtx *userContext) map[string]string {
	if len(userCtx.expandedData) == 0 && len(userCtx.newUnitID) == 0 {
		return nil
	}
	var extraData = make(map[string]string, len(userCtx.expandedData)+1)
	if len(userCtx.newUnitID) != 0 {
		extraData[newIDKey] = userCtx.newUnitID
	}
	for key, value := range userCtx.expandedData {
		extraData[key] = value
	}
	return extraData
}

// int64ListJoin slice to string
func int64ListJoin(elems []int64, sep string) string {
	if len(elems) == 0 {
		return ""
	}
	var sb bytes.Buffer
	sb.WriteString(strconv.FormatInt(elems[0], 10))
	for _, elem := range elems[1:] {
		sb.WriteString(sep)
		sb.WriteString(strconv.FormatInt(elem, 10))
	}
	return sb.String()
}

// manualInitEvent TODO
// Record initialization failure event
func manualInitEvent(projectIDList []string, latency time.Duration, err error) {
	for _, projectID := range projectIDList {
		application := cache.GetApplication(projectID)
		if application == nil {
			continue
		}
		metricsConfig := application.TabConfig.ControlData.EventMetricsConfig
		if metricsConfig == nil || !metricsConfig.IsEnable || metricsConfig.Metadata == nil {
			continue
		}
		interval := metricsConfig.SamplingInterval
		if err != nil {
			interval = metricsConfig.ErrSamplingInterval
		}
		sendDataErr := metrics.LogMonitorEvent(context.Background(), &metrics.Metadata{
			MetricsPluginName: metricsConfig.PluginName,
			TableName:         metricsConfig.Metadata.Name,
			TableID:           metricsConfig.Metadata.Id,
			Token:             metricsConfig.Metadata.Token,
			SamplingInterval:  interval,
		}, &protoc_event_server.MonitorEventGroup{Events: []*protoc_event_server.MonitorEvent{
			{
				Time:       time.Now().Unix(),
				Ip:         env.LocalIP(),
				ProjectId:  projectID,
				EventName:  "init",
				Latency:    float32(latency.Microseconds()), // us
				StatusCode: env.EventStatus(err),
				Message:    env.ErrMsg(err),
				SdkType:    env.SDKType,
				SdkVersion: env.Version,
				InvokePath: env.InvokePath(4), // 跳过 4 层调用栈
				InputData:  "",
				OutputData: "",
				ExtInfo:    nil,
			},
		}})
		if sendDataErr != nil {
			log.Errorf("sendData fail:%v", sendDataErr)
		}
	}
}
