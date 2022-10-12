package task

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	eventsErr "github.com/flyteorg/flytepropeller/events/errors"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/admin"
	"github.com/flyteorg/flytepropeller/pkg/apis/flyteworkflow/v1alpha1"

	"github.com/flyteorg/flytepropeller/pkg/utils"

	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/ioutils"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/event"
	pluginMachinery "github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/catalog"
	pluginCore "github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/core"
	"github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/io"
	pluginK8s "github.com/flyteorg/flyteplugins/go/tasks/pluginmachinery/k8s"
	controllerConfig "github.com/flyteorg/flytepropeller/pkg/controller/config"
	"github.com/flyteorg/flytestdlib/contextutils"
	"github.com/flyteorg/flytestdlib/logger"
	"github.com/flyteorg/flytestdlib/promutils"
	"github.com/flyteorg/flytestdlib/promutils/labeled"
	"github.com/flyteorg/flytestdlib/storage"
	"github.com/golang/protobuf/ptypes"
	regErrors "github.com/pkg/errors"

	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/task/resourcemanager"
	rmConfig "github.com/flyteorg/flytepropeller/pkg/controller/nodes/task/resourcemanager/config"

	"github.com/flyteorg/flytepropeller/pkg/controller/executors"
	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/errors"
	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/handler"
	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/task/catalog/datacatalog"
	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/task/config"
	"github.com/flyteorg/flytepropeller/pkg/controller/nodes/task/secretmanager"
)

const pluginContextKey = contextutils.Key("plugin")

type metrics struct {
	pluginPanics                   labeled.Counter
	unsupportedTaskType            labeled.Counter
	catalogPutFailureCount         labeled.Counter
	catalogGetFailureCount         labeled.Counter
	catalogPutSuccessCount         labeled.Counter
	catalogMissCount               labeled.Counter
	catalogHitCount                labeled.Counter
	pluginExecutionLatency         labeled.StopWatch
	pluginQueueLatency             labeled.StopWatch
	reservationGetSuccessCount     labeled.Counter
	reservationGetFailureCount     labeled.Counter
	reservationReleaseSuccessCount labeled.Counter
	reservationReleaseFailureCount labeled.Counter

	// TODO We should have a metric to capture custom state size
	scope promutils.Scope
}

type MetricKey = string

type taskMetrics struct {
	taskSucceeded labeled.Counter
	taskFailed    labeled.Counter
}

type pluginRequestedTransition struct {
	previouslyObserved bool
	ttype              handler.TransitionType
	pInfo              pluginCore.PhaseInfo
	execInfo           handler.ExecutionInfo
	pluginState        []byte
	pluginStateVersion uint32
}

func getPluginMetricKey(pluginID, taskType string) string {
	return taskType + "_" + pluginID
}

func (p *pluginRequestedTransition) CacheHit(outputPath storage.DataReference, deckPath *storage.DataReference, entry catalog.Entry) {
	p.ttype = handler.TransitionTypeEphemeral
	p.pInfo = pluginCore.PhaseInfoSuccess(nil)
	p.ObserveSuccess(outputPath, deckPath, &event.TaskNodeMetadata{CacheStatus: entry.GetStatus().GetCacheStatus(), CatalogKey: entry.GetStatus().GetMetadata()})
}

func (p *pluginRequestedTransition) PopulateCacheInfo(entry catalog.Entry) {
	p.execInfo.TaskNodeInfo = &handler.TaskNodeInfo{
		TaskNodeMetadata: &event.TaskNodeMetadata{
			CacheStatus: entry.GetStatus().GetCacheStatus(),
			CatalogKey:  entry.GetStatus().GetMetadata()},
	}
}

// PopulateReservationInfo sets the ReservationStatus of a requested plugin transition based on the
// provided ReservationEntry.
func (p *pluginRequestedTransition) PopulateReservationInfo(entry catalog.ReservationEntry) {
	if p.execInfo.TaskNodeInfo == nil {
		p.execInfo.TaskNodeInfo = &handler.TaskNodeInfo{
			TaskNodeMetadata: &event.TaskNodeMetadata{
				ReservationStatus: entry.GetStatus(),
			},
		}
	} else {
		p.execInfo.TaskNodeInfo.TaskNodeMetadata.ReservationStatus = entry.GetStatus()
	}
}

func (p *pluginRequestedTransition) ObservedTransitionAndState(trns pluginCore.Transition, pluginStateVersion uint32, pluginState []byte) {
	p.ttype = ToTransitionType(trns.Type())
	p.pInfo = trns.Info()
	p.pluginState = pluginState
	p.pluginStateVersion = pluginStateVersion
}

func (p *pluginRequestedTransition) ObservedExecutionError(executionError *io.ExecutionError, taskMetadata *event.TaskNodeMetadata) {
	if executionError.IsRecoverable {
		p.pInfo = pluginCore.PhaseInfoFailed(pluginCore.PhaseRetryableFailure, executionError.ExecutionError, p.pInfo.Info())
	} else {
		p.pInfo = pluginCore.PhaseInfoFailed(pluginCore.PhasePermanentFailure, executionError.ExecutionError, p.pInfo.Info())
	}

	if taskMetadata != nil {
		p.execInfo.TaskNodeInfo = &handler.TaskNodeInfo{
			TaskNodeMetadata: taskMetadata,
		}
	}
}

func (p *pluginRequestedTransition) ObservedFailure(taskMetadata *event.TaskNodeMetadata) {
	if taskMetadata != nil {
		p.execInfo.TaskNodeInfo = &handler.TaskNodeInfo{
			TaskNodeMetadata: taskMetadata,
		}
	}
}

func (p *pluginRequestedTransition) IsPreviouslyObserved() bool {
	return p.previouslyObserved
}

func (p *pluginRequestedTransition) TransitionPreviouslyRecorded() {
	p.previouslyObserved = true
}

func (p *pluginRequestedTransition) FinalTaskEvent(input ToTaskExecutionEventInputs) (*event.TaskExecutionEvent, error) {
	if p.previouslyObserved {
		return nil, nil
	}
	input.Info = p.pInfo
	return ToTaskExecutionEvent(input)
}

func (p *pluginRequestedTransition) ObserveSuccess(outputPath storage.DataReference, deckPath *storage.DataReference, taskMetadata *event.TaskNodeMetadata) {
	p.execInfo.OutputInfo = &handler.OutputInfo{
		OutputURI: outputPath,
		DeckURI:   deckPath,
	}

	p.execInfo.TaskNodeInfo = &handler.TaskNodeInfo{
		TaskNodeMetadata: taskMetadata,
	}
}

func (p *pluginRequestedTransition) FinalTransition(ctx context.Context) (handler.Transition, error) {
	switch p.pInfo.Phase() {
	case pluginCore.PhaseSuccess:
		logger.Debugf(ctx, "Transitioning to Success")
		return handler.DoTransition(p.ttype, handler.PhaseInfoSuccess(&p.execInfo)), nil
	case pluginCore.PhaseRetryableFailure:
		logger.Debugf(ctx, "Transitioning to RetryableFailure")
		return handler.DoTransition(p.ttype, handler.PhaseInfoRetryableFailureErr(p.pInfo.Err(), &p.execInfo)), nil
	case pluginCore.PhasePermanentFailure:
		logger.Debugf(ctx, "Transitioning to Failure")
		return handler.DoTransition(p.ttype, handler.PhaseInfoFailureErr(p.pInfo.Err(), &p.execInfo)), nil
	case pluginCore.PhaseUndefined:
		return handler.UnknownTransition, fmt.Errorf("error converting plugin phase, received [Undefined]")
	}

	logger.Debugf(ctx, "Task still running")
	return handler.DoTransition(p.ttype, handler.PhaseInfoRunning(nil)), nil
}

// The plugin interface available especially for testing.
type PluginRegistryIface interface {
	GetCorePlugins() []pluginCore.PluginEntry
	GetK8sPlugins() []pluginK8s.PluginEntry
}

type taskType = string
type pluginID = string

type Handler struct {
	catalog         catalog.Client
	asyncCatalog    catalog.AsyncClient
	defaultPlugins  map[pluginCore.TaskType]pluginCore.Plugin
	pluginsForType  map[pluginCore.TaskType]map[pluginID]pluginCore.Plugin
	taskMetricsMap  map[MetricKey]*taskMetrics
	defaultPlugin   pluginCore.Plugin
	metrics         *metrics
	pluginRegistry  PluginRegistryIface
	kubeClient      pluginCore.KubeClient
	secretManager   pluginCore.SecretManager
	resourceManager resourcemanager.BaseResourceManager
	barrierCache    *barrier
	cfg             *config.Config
	pluginScope     promutils.Scope
	eventConfig     *controllerConfig.EventConfig
	clusterID       string
}

func (t *Handler) FinalizeRequired() bool {
	return true
}

func (t *Handler) setDefault(ctx context.Context, p pluginCore.Plugin) error {
	if t.defaultPlugin != nil {
		logger.Errorf(ctx, "cannot set plugin [%s] as default as plugin [%s] is already configured as default", p.GetID(), t.defaultPlugin.GetID())
	} else {
		logger.Infof(ctx, "Plugin [%s] registered as default plugin", p.GetID())
		t.defaultPlugin = p
	}
	return nil
}

func (t *Handler) Setup(ctx context.Context, sCtx handler.SetupContext) error {
	tSCtx := t.newSetupContext(sCtx)

	// Create a new base resource negotiator
	resourceManagerConfig := rmConfig.GetConfig()
	newResourceManagerBuilder, err := resourcemanager.GetResourceManagerBuilderByType(ctx, resourceManagerConfig.Type, t.metrics.scope)
	if err != nil {
		return err
	}

	// Create the resource negotiator here
	// and then convert it to proxies later and pass them to plugins
	enabledPlugins, defaultForTaskTypes, err := WranglePluginsAndGenerateFinalList(ctx, &t.cfg.TaskPlugins, t.pluginRegistry)
	if err != nil {
		logger.Errorf(ctx, "Failed to finalize enabled plugins. Error: %s", err)
		return err
	}

	// Not every task type will have a default plugin specified in the flytepropeller config.
	// That's fine, we resort to using the plugins' static RegisteredTaskTypes as a fallback further below.
	fallbackTaskHandlerMap := make(map[taskType]map[pluginID]pluginCore.Plugin)

	for _, p := range enabledPlugins {
		// create a new resource registrar proxy for each plugin, and pass it into the plugin's LoadPlugin() via a setup context
		pluginResourceNamespacePrefix := pluginCore.ResourceNamespace(newResourceManagerBuilder.GetID()).CreateSubNamespace(pluginCore.ResourceNamespace(p.ID))
		sCtxFinal := newNameSpacedSetupCtx(
			tSCtx, newResourceManagerBuilder.GetResourceRegistrar(pluginResourceNamespacePrefix), p.ID)
		logger.Infof(ctx, "Loading Plugin [%s] ENABLED", p.ID)
		cp, err := pluginCore.LoadPlugin(ctx, sCtxFinal, p)
		if err != nil {
			return regErrors.Wrapf(err, "failed to load plugin - %s", p.ID)
		}
		// For every default plugin for a task type specified in flytepropeller config we validate that the plugin's
		// static definition includes that task type as something it is registered to handle.
		for _, tt := range p.RegisteredTaskTypes {
			for _, defaultTaskType := range defaultForTaskTypes[cp.GetID()] {
				if defaultTaskType == tt {
					if existingHandler, alreadyDefaulted := t.defaultPlugins[tt]; alreadyDefaulted && existingHandler.GetID() != cp.GetID() {
						logger.Errorf(ctx, "TaskType [%s] has multiple default handlers specified: [%s] and [%s]",
							tt, existingHandler.GetID(), cp.GetID())
						return regErrors.New(fmt.Sprintf("TaskType [%s] has multiple default handlers specified: [%s] and [%s]",
							tt, existingHandler.GetID(), cp.GetID()))
					}
					logger.Infof(ctx, "Plugin [%s] registered for TaskType [%s]", cp.GetID(), tt)
					t.defaultPlugins[tt] = cp
				}
			}

			pluginsForTaskType, ok := t.pluginsForType[tt]
			if !ok {
				pluginsForTaskType = make(map[pluginID]pluginCore.Plugin)
			}
			pluginsForTaskType[cp.GetID()] = cp
			t.pluginsForType[tt] = pluginsForTaskType

			fallbackMap, ok := fallbackTaskHandlerMap[tt]
			if !ok {
				fallbackMap = make(map[pluginID]pluginCore.Plugin)
			}
			fallbackMap[cp.GetID()] = cp
			fallbackTaskHandlerMap[tt] = fallbackMap
		}
		if p.IsDefault {
			if err := t.setDefault(ctx, cp); err != nil {
				return err
			}
		}
	}

	// Read from the fallback task handler map for any remaining tasks without a defaultPlugins registered handler.
	for taskType, registeredPlugins := range fallbackTaskHandlerMap {
		if _, ok := t.defaultPlugins[taskType]; ok {
			continue
		}
		if len(registeredPlugins) != 1 {
			logger.Errorf(ctx, "Multiple plugins registered to handle task type: %s. ([%+v])", taskType, registeredPlugins)
			return regErrors.New(fmt.Sprintf("Multiple plugins registered to handle task type: %s. ([%+v]). Use default-for-task-type config option to choose the desired plugin.", taskType, registeredPlugins))
		}
		for _, plugin := range registeredPlugins {
			t.defaultPlugins[taskType] = plugin
		}
	}

	rm, err := newResourceManagerBuilder.BuildResourceManager(ctx)
	if err != nil {
		logger.Errorf(ctx, "Failed to build a resource manager")
		return err
	}

	t.resourceManager = rm

	return nil
}

func (t Handler) ResolvePlugin(ctx context.Context, ttype string, executionConfig v1alpha1.ExecutionConfig) (pluginCore.Plugin, error) {
	// If the workflow specifies plugin overrides, check to see if any of the specified plugins for that type are
	// registered in this deployment of flytepropeller.
	if len(executionConfig.TaskPluginImpls[ttype].PluginIDs) > 0 {
		if len(t.pluginsForType[ttype]) > 0 {
			pluginsForType := t.pluginsForType[ttype]
			for _, pluginImplID := range executionConfig.TaskPluginImpls[ttype].PluginIDs {
				pluginImpl := pluginsForType[pluginImplID]
				if pluginImpl != nil {
					logger.Debugf(ctx, "Plugin [%s] resolved for Handler type [%s]", pluginImpl.GetID(), ttype)
					return pluginImpl, nil
				}
			}
		}

		// If we've exhausted the list of overridable plugins and no single implementation is found, fail fast if the
		// task plugin overrides specify so.
		if executionConfig.TaskPluginImpls[ttype].MissingPluginBehavior == admin.PluginOverride_FAIL {
			return nil, fmt.Errorf("no matching plugin overrides defined for Handler type [%s]. Ignoring any defaultPlugins configured", ttype)
		}
	}

	p, ok := t.defaultPlugins[ttype]
	if ok {
		logger.Debugf(ctx, "Plugin [%s] resolved for Handler type [%s]", p.GetID(), ttype)
		return p, nil
	}
	if t.defaultPlugin != nil {
		logger.Warnf(ctx, "No plugin found for Handler-type [%s], defaulting to [%s]", ttype, t.defaultPlugin.GetID())
		return t.defaultPlugin, nil
	}
	return nil, fmt.Errorf("no plugin defined for Handler type [%s] and no defaultPlugin configured", ttype)
}

func validateTransition(transition pluginCore.Transition) error {
	if info := transition.Info(); info.Err() == nil && info.Info() == nil {
		return fmt.Errorf("transition doesn't have task info nor an execution error filled [%v]", transition)
	}

	return nil
}

func (t Handler) fetchPluginTaskMetrics(pluginID, taskType string) (*taskMetrics, error) {
	metricNameKey, err := utils.GetSanitizedPrometheusKey(getPluginMetricKey(pluginID, taskType))
	if err != nil {
		return nil, err
	}
	if _, ok := t.taskMetricsMap[metricNameKey]; !ok {
		t.taskMetricsMap[metricNameKey] = &taskMetrics{
			taskSucceeded: labeled.NewCounter(metricNameKey+"_success",
				"Task "+metricNameKey+" finished successfully", t.pluginScope, labeled.EmitUnlabeledMetric),
			taskFailed: labeled.NewCounter(metricNameKey+"_failure",
				"Task "+metricNameKey+" failed", t.pluginScope, labeled.EmitUnlabeledMetric),
		}
	}
	return t.taskMetricsMap[metricNameKey], nil
}

func (t Handler) invokePlugin(ctx context.Context, p pluginCore.Plugin, tCtx *taskExecutionContext, ts handler.TaskNodeState) (*pluginRequestedTransition, error) {
	pluginTrns := &pluginRequestedTransition{}

	trns, err := func() (trns pluginCore.Transition, err error) {
		defer func() {
			if r := recover(); r != nil {
				t.metrics.pluginPanics.Inc(ctx)
				stack := debug.Stack()
				logger.Errorf(ctx, "Panic in plugin[%s]", p.GetID())
				err = fmt.Errorf("panic when executing a plugin [%s]. Stack: [%s]", p.GetID(), string(stack))
				trns = pluginCore.UnknownTransition
			}
		}()
		childCtx := context.WithValue(ctx, pluginContextKey, p.GetID())
		trns, err = p.Handle(childCtx, tCtx)
		return
	}()
	if err != nil {
		logger.Warnf(ctx, "Runtime error from plugin [%s]. Error: %s", p.GetID(), err.Error())
		return nil, regErrors.Wrapf(err, "failed to execute handle for plugin [%s]", p.GetID())
	}

	err = validateTransition(trns)
	if err != nil {
		logger.Errorf(ctx, "Invalid transition from plugin [%s]. Error: %s", p.GetID(), err.Error())
		return nil, regErrors.Wrapf(err, "Invalid transition for plugin [%s]", p.GetID())
	}

	var b []byte
	var v uint32
	if tCtx.psm.newState != nil {
		b = tCtx.psm.newState.Bytes()
		v = uint32(tCtx.psm.newStateVersion)
	} else {
		// New state was not mutated, so we should write back the existing state
		b = ts.PluginState
		v = ts.PluginPhaseVersion
	}
	pluginTrns.ObservedTransitionAndState(trns, v, b)

	// Emit the queue latency if the task has just transitioned from Queued to Running.
	if ts.PluginPhase == pluginCore.PhaseQueued &&
		(pluginTrns.pInfo.Phase() == pluginCore.PhaseInitializing || pluginTrns.pInfo.Phase() == pluginCore.PhaseRunning) {
		if !ts.LastPhaseUpdatedAt.IsZero() {
			t.metrics.pluginQueueLatency.Observe(ctx, ts.LastPhaseUpdatedAt, time.Now())
		}
	}

	if pluginTrns.pInfo.Phase() == ts.PluginPhase {
		if pluginTrns.pInfo.Version() == ts.PluginPhaseVersion {
			logger.Debugf(ctx, "p+Version previously seen .. no event will be sent")
			pluginTrns.TransitionPreviouslyRecorded()
			return pluginTrns, nil
		}
		if pluginTrns.pInfo.Version() > uint32(t.cfg.MaxPluginPhaseVersions) {
			logger.Errorf(ctx, "Too many Plugin p versions for plugin [%s]. p versions [%d/%d]", p.GetID(), pluginTrns.pInfo.Version(), t.cfg.MaxPluginPhaseVersions)
			pluginTrns.ObservedExecutionError(&io.ExecutionError{
				ExecutionError: &core.ExecutionError{
					Code: "TooManyPluginPhaseVersions",
					Message: fmt.Sprintf("Total number of phase versions exceeded for phase [%s] in Plugin "+
						"[%s]. Attempted to set version to [%v], max allowed [%d]",
						pluginTrns.pInfo.Phase().String(), p.GetID(), pluginTrns.pInfo.Version(), t.cfg.MaxPluginPhaseVersions),
				},
				IsRecoverable: false,
			}, nil)
			return pluginTrns, nil
		}
	}

	if !pluginTrns.IsPreviouslyObserved() {
		taskType := fmt.Sprintf("%v", ctx.Value(contextutils.TaskTypeKey))
		taskMetric, err := t.fetchPluginTaskMetrics(p.GetID(), taskType)
		if err != nil {
			return nil, err
		}
		if pluginTrns.pInfo.Phase() == pluginCore.PhaseSuccess {
			taskMetric.taskSucceeded.Inc(ctx)
		}
		if pluginTrns.pInfo.Phase() == pluginCore.PhasePermanentFailure || pluginTrns.pInfo.Phase() == pluginCore.PhaseRetryableFailure {
			taskMetric.taskFailed.Inc(ctx)
		}
	}

	switch pluginTrns.pInfo.Phase() {
	case pluginCore.PhaseSuccess:
		// -------------------------------------
		// TODO: @kumare create Issue# Remove the code after we use closures to handle dynamic nodes
		// This code only exists to support Dynamic tasks. Eventually dynamic tasks will use closure nodes to execute
		// Until then we have to check if the Handler executed resulted in a dynamic node being generated, if so, then
		// we will not check for outputs or call onTaskSuccess. The reason is that outputs have not yet been materialized.
		// Output for the parent node will only get generated after the subtasks complete. We have to wait for the completion
		// the dynamic.handler will call onTaskSuccess for the parent node

		f, err := NewRemoteFutureFileReader(ctx, tCtx.ow.GetOutputPrefixPath(), tCtx.DataStore())
		if err != nil {
			return nil, regErrors.Wrapf(err, "failed to create remote file reader")
		}
		if ok, err := f.Exists(ctx); err != nil {
			logger.Errorf(ctx, "failed to check existence of futures file")
			return nil, regErrors.Wrapf(err, "failed to check existence of futures file")
		} else if ok {
			logger.Infof(ctx, "Futures file exists, this is a dynamic parent-Handler will not run onTaskSuccess")
			return pluginTrns, nil
		}
		// End TODO
		// -------------------------------------
		logger.Debugf(ctx, "Task success detected, calling on Task success")
		outputCommitter := ioutils.NewRemoteFileOutputWriter(ctx, tCtx.DataStore(), tCtx.OutputWriter())
		execID := tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetID()
		cacheStatus, ee, err := t.ValidateOutputAndCacheAdd(ctx, tCtx.NodeID(), tCtx.InputReader(), tCtx.ow.GetReader(),
			outputCommitter, tCtx.ExecutionContext().GetExecutionConfig(), tCtx.tr, catalog.Metadata{
				TaskExecutionIdentifier: &execID,
			})
		if err != nil {
			return nil, err
		}

		if ee != nil {
			pluginTrns.ObservedExecutionError(ee,
				&event.TaskNodeMetadata{
					CheckpointUri: tCtx.ow.GetCheckpointPrefix().String(),
				})
		} else {
			var deckURI *storage.DataReference
			if tCtx.ow.GetReader() != nil {
				exists, err := tCtx.ow.GetReader().DeckExists(ctx)
				if err != nil {
					logger.Errorf(ctx, "Failed to check deck file existence. Error: %v", err)
					return pluginTrns, regErrors.Wrapf(err, "failed to check existence of deck file")
				} else if exists {
					deckURIValue := tCtx.ow.GetDeckPath()
					deckURI = &deckURIValue
				}
			}
			pluginTrns.ObserveSuccess(tCtx.ow.GetOutputPath(), deckURI,
				&event.TaskNodeMetadata{
					CacheStatus:   cacheStatus.GetCacheStatus(),
					CatalogKey:    cacheStatus.GetMetadata(),
					CheckpointUri: tCtx.ow.GetCheckpointPrefix().String(),
				})
		}
	case pluginCore.PhaseRetryableFailure:
		fallthrough
	case pluginCore.PhasePermanentFailure:
		pluginTrns.ObservedFailure(
			&event.TaskNodeMetadata{
				CheckpointUri: tCtx.ow.GetCheckpointPrefix().String(),
			})
	}

	return pluginTrns, nil
}

func (t Handler) Handle(ctx context.Context, nCtx handler.NodeExecutionContext) (handler.Transition, error) {
	ttype := nCtx.TaskReader().GetTaskType()
	ctx = contextutils.WithTaskType(ctx, ttype)
	p, err := t.ResolvePlugin(ctx, ttype, nCtx.ExecutionContext().GetExecutionConfig())
	if err != nil {
		return handler.UnknownTransition, errors.Wrapf(errors.UnsupportedTaskTypeError, nCtx.NodeID(), err, "unable to resolve plugin")
	}

	checkCatalog := !p.GetProperties().DisableNodeLevelCaching
	if !checkCatalog {
		logger.Infof(ctx, "Node level caching is disabled. Skipping catalog read.")
	}

	tCtx, err := t.newTaskExecutionContext(ctx, nCtx, p)
	if err != nil {
		return handler.UnknownTransition, errors.Wrapf(errors.IllegalStateError, nCtx.NodeID(), err, "unable to create Handler execution context")
	}

	ts := nCtx.NodeStateReader().GetTaskNodeState()

	pluginTrns := &pluginRequestedTransition{}
	// We will start with the assumption that catalog is disabled
	pluginTrns.PopulateCacheInfo(catalog.NewFailedCatalogEntry(catalog.NewStatus(core.CatalogCacheStatus_CACHE_DISABLED, nil)))

	// NOTE: Ideally we should use a taskExecution state for this handler. But, doing that will make it completely backwards incompatible
	// So now we will derive this from the plugin phase
	// TODO @kumare re-evaluate this decision

	// STEP 1: Check Cache
	if (ts.PluginPhase == pluginCore.PhaseUndefined || ts.PluginPhase == pluginCore.PhaseWaitingForCache) && checkCatalog {
		// This is assumed to be first time. we will check catalog and call handle
		entry, err := t.CheckCatalogCache(ctx, tCtx.tr, nCtx.InputReader(), tCtx.ow)
		if err != nil {
			logger.Errorf(ctx, "failed to check catalog cache with error")
			return handler.UnknownTransition, err
		}

		if entry.GetStatus().GetCacheStatus() == core.CatalogCacheStatus_CACHE_HIT {
			r := tCtx.ow.GetReader()
			if r == nil {
				return handler.UnknownTransition, errors.Errorf(errors.IllegalStateError, nCtx.NodeID(), "failed to reader outputs from a CacheHIT. Unexpected!")
			}

			// TODO @kumare this can be optimized, if we have paths then the reader could be pipelined to a sink
			o, ee, err := r.Read(ctx)
			if err != nil {
				logger.Errorf(ctx, "failed to read from catalog, err: %s", err.Error())
				return handler.UnknownTransition, err
			}

			if ee != nil {
				logger.Errorf(ctx, "got execution error from catalog output reader? This should not happen, err: %s", ee.String())
				return handler.UnknownTransition, errors.Errorf(errors.IllegalStateError, nCtx.NodeID(), "execution error from a cache output, bad state: %s", ee.String())
			}

			if err := nCtx.DataStore().WriteProtobuf(ctx, tCtx.ow.GetOutputPath(), storage.Options{}, o); err != nil {
				logger.Errorf(ctx, "failed to write cached value to datastore, err: %s", err.Error())
				return handler.UnknownTransition, err
			}
			deckPathValue, ok := tCtx.ow.GetReader().GetOutputMetadata(ctx)[datacatalog.DeckURIKey]
			if ok {
				deckPath := storage.DataReference(deckPathValue)
				pluginTrns.CacheHit(tCtx.ow.GetOutputPath(), &deckPath, entry)
			} else {
				pluginTrns.CacheHit(tCtx.ow.GetOutputPath(), nil, entry)
			}
		} else {
			logger.Infof(ctx, "No CacheHIT. Status [%s]", entry.GetStatus().GetCacheStatus().String())
			pluginTrns.PopulateCacheInfo(entry)
		}
	}

	// Check catalog for cache reservation and acquire if none exists
	if checkCatalog && (pluginTrns.execInfo.TaskNodeInfo == nil || pluginTrns.execInfo.TaskNodeInfo.TaskNodeMetadata.CacheStatus != core.CatalogCacheStatus_CACHE_HIT) {
		ownerID := tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName()
		reservation, err := t.GetOrExtendCatalogReservation(ctx, ownerID, controllerConfig.GetConfig().WorkflowReEval.Duration, tCtx.tr, nCtx.InputReader())
		if err != nil {
			logger.Errorf(ctx, "failed to get or extend catalog reservation with error")
			return handler.UnknownTransition, err
		}

		pluginTrns.PopulateReservationInfo(reservation)

		if reservation.GetStatus() == core.CatalogReservation_RESERVATION_ACQUIRED &&
			(ts.PluginPhase == pluginCore.PhaseUndefined || ts.PluginPhase == pluginCore.PhaseWaitingForCache) {
			logger.Infof(ctx, "Acquired cache reservation")
		}

		// If we do not own the reservation then we transition to WaitingForCache phase. If we are
		// already running (ie. in a phase other than PhaseUndefined or PhaseWaitingForCache) and
		// somehow lost the reservation (ex. by expiration), continue to execute until completion.
		if reservation.GetStatus() == core.CatalogReservation_RESERVATION_EXISTS {
			if ts.PluginPhase == pluginCore.PhaseUndefined || ts.PluginPhase == pluginCore.PhaseWaitingForCache {
				pluginTrns.ttype = handler.TransitionTypeEphemeral
				pluginTrns.pInfo = pluginCore.PhaseInfoWaitingForCache(pluginCore.DefaultPhaseVersion, nil)
			}

			if ts.PluginPhase == pluginCore.PhaseWaitingForCache {
				logger.Debugf(ctx, "No state change for Task, previously observed same transition. Short circuiting.")
				return pluginTrns.FinalTransition(ctx)
			}
		}
	}

	barrierTick := uint32(0)
	// STEP 2: If no cache-hit and not transitioning to PhaseWaitingForCache, then lets invoke the plugin and wait for a transition out of undefined
	if pluginTrns.execInfo.TaskNodeInfo == nil || (pluginTrns.pInfo.Phase() != pluginCore.PhaseWaitingForCache &&
		pluginTrns.execInfo.TaskNodeInfo.TaskNodeMetadata.CacheStatus != core.CatalogCacheStatus_CACHE_HIT) {
		prevBarrier := t.barrierCache.GetPreviousBarrierTransition(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName())
		// Lets start with the current barrierTick (the value to be stored) same as the barrierTick in the cache
		barrierTick = prevBarrier.BarrierClockTick
		// Lets check if this value in cache is less than or equal to one in the store
		if barrierTick <= ts.BarrierClockTick {
			var err error
			pluginTrns, err = t.invokePlugin(ctx, p, tCtx, ts)
			if err != nil {
				return handler.UnknownTransition, errors.Wrapf(errors.RuntimeExecutionError, nCtx.NodeID(), err, "failed during plugin execution")
			}
			if pluginTrns.IsPreviouslyObserved() {
				logger.Debugf(ctx, "No state change for Task, previously observed same transition. Short circuiting.")
				return pluginTrns.FinalTransition(ctx)
			}
			// Now no matter what we should update the barrierTick (stored in state)
			// This is because the state is ahead of the inmemory representation
			// This can happen in the case where the process restarted or the barrier cache got reset
			barrierTick = ts.BarrierClockTick
			// Now if the transition is of type barrier, lets tick the clock by one from the prev known value
			// store that in the cache
			if pluginTrns.ttype == handler.TransitionTypeBarrier {
				logger.Infof(ctx, "Barrier transition observed for Plugin [%s], TaskExecID [%s]. recording: [%s]", p.GetID(), tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), pluginTrns.pInfo.String())
				barrierTick = barrierTick + 1
				t.barrierCache.RecordBarrierTransition(ctx, tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), BarrierTransition{
					BarrierClockTick: barrierTick,
					CallLog: PluginCallLog{
						PluginTransition: pluginTrns,
					},
				})

			}
		} else {
			// Barrier tick will remain to be the one in cache.
			// Now it may happen that the cache may get reset before we store the barrier tick
			// this will cause us to lose that information and potentially replaying.
			logger.Infof(ctx, "Replaying Barrier transition for cache tick [%d] < stored tick [%d], Plugin [%s], TaskExecID [%s]. recording: [%s]", barrierTick, ts.BarrierClockTick, p.GetID(), tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName(), prevBarrier.CallLog.PluginTransition.pInfo.String())
			pluginTrns = prevBarrier.CallLog.PluginTransition
		}
	}

	// STEP 3: Sanity check
	if pluginTrns == nil {
		// Still nil, this should never happen!!!
		return handler.UnknownTransition, errors.Errorf(errors.IllegalStateError, nCtx.NodeID(), "plugin transition is not observed and no error as well.")
	}

	// STEP 4: Send buffered events!
	logger.Debugf(ctx, "Sending buffered Task events.")
	for _, ev := range tCtx.ber.GetAll(ctx) {
		evInfo, err := ToTaskExecutionEvent(ToTaskExecutionEventInputs{
			TaskExecContext:       tCtx,
			InputReader:           nCtx.InputReader(),
			OutputWriter:          tCtx.ow,
			Info:                  ev,
			NodeExecutionMetadata: nCtx.NodeExecutionMetadata(),
			ExecContext:           nCtx.ExecutionContext(),
			TaskType:              ttype,
			PluginID:              p.GetID(),
			ResourcePoolInfo:      tCtx.rm.GetResourcePoolInfo(),
			ClusterID:             t.clusterID,
		})
		if err != nil {
			return handler.UnknownTransition, err
		}
		if err := nCtx.EventsRecorder().RecordTaskEvent(ctx, evInfo, t.eventConfig); err != nil {
			logger.Errorf(ctx, "Event recording failed for Plugin [%s], eventPhase [%s], error :%s", p.GetID(), evInfo.Phase.String(), err.Error())
			// Check for idempotency
			// Check for terminate state error
			return handler.UnknownTransition, err
		}
	}

	// STEP 5: Send Transition events
	logger.Debugf(ctx, "Sending transition event for plugin phase [%s]", pluginTrns.pInfo.Phase().String())
	evInfo, err := pluginTrns.FinalTaskEvent(ToTaskExecutionEventInputs{
		TaskExecContext:       tCtx,
		InputReader:           nCtx.InputReader(),
		OutputWriter:          tCtx.ow,
		NodeExecutionMetadata: nCtx.NodeExecutionMetadata(),
		ExecContext:           nCtx.ExecutionContext(),
		TaskType:              ttype,
		PluginID:              p.GetID(),
		ResourcePoolInfo:      tCtx.rm.GetResourcePoolInfo(),
		ClusterID:             t.clusterID,
	})
	if err != nil {
		logger.Errorf(ctx, "failed to convert plugin transition to TaskExecutionEvent. Error: %s", err.Error())
		return handler.UnknownTransition, err
	}
	if evInfo != nil {
		if err := nCtx.EventsRecorder().RecordTaskEvent(ctx, evInfo, t.eventConfig); err != nil {
			// Check for idempotency
			// Check for terminate state error
			logger.Errorf(ctx, "failed to send event to Admin. error: %s", err.Error())
			return handler.UnknownTransition, err
		}
	} else {
		logger.Debugf(ctx, "Received no event to record.")
	}

	// STEP 6: Persist the plugin state
	err = nCtx.NodeStateWriter().PutTaskNodeState(handler.TaskNodeState{
		PluginState:                        pluginTrns.pluginState,
		PluginStateVersion:                 pluginTrns.pluginStateVersion,
		PluginPhase:                        pluginTrns.pInfo.Phase(),
		PluginPhaseVersion:                 pluginTrns.pInfo.Version(),
		BarrierClockTick:                   barrierTick,
		LastPhaseUpdatedAt:                 time.Now(),
		PreviousNodeExecutionCheckpointURI: ts.PreviousNodeExecutionCheckpointURI,
	})
	if err != nil {
		logger.Errorf(ctx, "Failed to store TaskNode state, err :%s", err.Error())
		return handler.UnknownTransition, err
	}

	if !pluginTrns.pInfo.Phase().IsTerminal() {
		eCtx := nCtx.ExecutionContext()
		logger.Infof(ctx, "Parallelism now set to [%d].", eCtx.IncrementParallelism())
	}
	return pluginTrns.FinalTransition(ctx)
}

func (t Handler) Abort(ctx context.Context, nCtx handler.NodeExecutionContext, reason string) error {
	currentPhase := nCtx.NodeStateReader().GetTaskNodeState().PluginPhase
	logger.Debugf(ctx, "Abort invoked with phase [%v]", currentPhase)

	if currentPhase.IsTerminal() {
		logger.Debugf(ctx, "Returning immediately from Abort since task is already in terminal phase.", currentPhase)
		return nil
	}

	ttype := nCtx.TaskReader().GetTaskType()
	p, err := t.ResolvePlugin(ctx, ttype, nCtx.ExecutionContext().GetExecutionConfig())
	if err != nil {
		return errors.Wrapf(errors.UnsupportedTaskTypeError, nCtx.NodeID(), err, "unable to resolve plugin")
	}

	tCtx, err := t.newTaskExecutionContext(ctx, nCtx, p)
	if err != nil {
		return errors.Wrapf(errors.IllegalStateError, nCtx.NodeID(), err, "unable to create Handler execution context")
	}

	err = func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.metrics.pluginPanics.Inc(ctx)
				stack := debug.Stack()
				logger.Errorf(ctx, "Panic in plugin.Abort for TaskType [%s]", ttype)
				err = fmt.Errorf("panic when executing a plugin for TaskType [%s]. Stack: [%s]", ttype, string(stack))
			}
		}()

		childCtx := context.WithValue(ctx, pluginContextKey, p.GetID())
		err = p.Abort(childCtx, tCtx)
		return
	}()

	if err != nil {
		logger.Errorf(ctx, "Abort failed when calling plugin abort.")
		return err
	}
	taskExecID := tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetID()
	evRecorder := nCtx.EventsRecorder()
	nodeExecutionID, err := getParentNodeExecIDForTask(&taskExecID, nCtx.ExecutionContext())
	if err != nil {
		return err
	}
	if err := evRecorder.RecordTaskEvent(ctx, &event.TaskExecutionEvent{
		TaskId:                taskExecID.TaskId,
		ParentNodeExecutionId: nodeExecutionID,
		RetryAttempt:          nCtx.CurrentAttempt(),
		Phase:                 core.TaskExecution_ABORTED,
		OccurredAt:            ptypes.TimestampNow(),
		OutputResult: &event.TaskExecutionEvent_Error{
			Error: &core.ExecutionError{
				Code:    "Task Aborted",
				Message: reason,
			}},
	}, t.eventConfig); err != nil && !eventsErr.IsNotFound(err) && !eventsErr.IsEventIncompatibleClusterError(err) {
		// If a prior workflow/node/task execution event has failed because of an invalid cluster error, don't stall the abort
		// at this point in the clean-up.
		logger.Errorf(ctx, "failed to send event to Admin. error: %s", err.Error())
		return err
	}
	return nil
}

func (t Handler) Finalize(ctx context.Context, nCtx handler.NodeExecutionContext) error {
	logger.Debugf(ctx, "Finalize invoked.")
	ttype := nCtx.TaskReader().GetTaskType()
	p, err := t.ResolvePlugin(ctx, ttype, nCtx.ExecutionContext().GetExecutionConfig())
	if err != nil {
		return errors.Wrapf(errors.UnsupportedTaskTypeError, nCtx.NodeID(), err, "unable to resolve plugin")
	}

	tCtx, err := t.newTaskExecutionContext(ctx, nCtx, p)
	if err != nil {
		return errors.Wrapf(errors.IllegalStateError, nCtx.NodeID(), err, "unable to create Handler execution context")
	}

	return func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.metrics.pluginPanics.Inc(ctx)
				stack := debug.Stack()
				logger.Errorf(ctx, "Panic in plugin.Finalize for TaskType [%s]", ttype)
				err = fmt.Errorf("panic when executing a plugin for TaskType [%s]. Stack: [%s]", ttype, string(stack))
			}
		}()

		// release catalog reservation (if exists)
		ownerID := tCtx.TaskExecutionMetadata().GetTaskExecutionID().GetGeneratedName()
		_, err = t.ReleaseCatalogReservation(ctx, ownerID, tCtx.tr, tCtx.InputReader())
		if err != nil {
			return errors.Wrapf(errors.CatalogCallFailed, nCtx.NodeID(), err, "failed to release reservation")
		}

		childCtx := context.WithValue(ctx, pluginContextKey, p.GetID())
		err = p.Finalize(childCtx, tCtx)
		return
	}()
}

func New(ctx context.Context, kubeClient executors.Client, client catalog.Client, eventConfig *controllerConfig.EventConfig, clusterID string, scope promutils.Scope) (*Handler, error) {
	// TODO New should take a pointer
	async, err := catalog.NewAsyncClient(client, *catalog.GetConfig(), scope.NewSubScope("async_catalog"))
	if err != nil {
		return nil, err
	}

	if err = async.Start(ctx); err != nil {
		return nil, err
	}

	cfg := config.GetConfig()
	return &Handler{
		pluginRegistry: pluginMachinery.PluginRegistry(),
		defaultPlugins: make(map[pluginCore.TaskType]pluginCore.Plugin),
		pluginsForType: make(map[pluginCore.TaskType]map[pluginID]pluginCore.Plugin),
		taskMetricsMap: make(map[MetricKey]*taskMetrics),
		metrics: &metrics{
			pluginPanics:                   labeled.NewCounter("plugin_panic", "Task plugin paniced when trying to execute a Handler.", scope),
			unsupportedTaskType:            labeled.NewCounter("unsupported_tasktype", "No Handler plugin configured for Handler type", scope),
			catalogHitCount:                labeled.NewCounter("discovery_hit_count", "Task cached in Discovery", scope),
			catalogMissCount:               labeled.NewCounter("discovery_miss_count", "Task not cached in Discovery", scope),
			catalogPutSuccessCount:         labeled.NewCounter("discovery_put_success_count", "Discovery Put success count", scope),
			catalogPutFailureCount:         labeled.NewCounter("discovery_put_failure_count", "Discovery Put failure count", scope),
			catalogGetFailureCount:         labeled.NewCounter("discovery_get_failure_count", "Discovery Get faillure count", scope),
			pluginExecutionLatency:         labeled.NewStopWatch("plugin_exec_latency", "Time taken to invoke plugin for one round", time.Microsecond, scope),
			pluginQueueLatency:             labeled.NewStopWatch("plugin_queue_latency", "Time spent by plugin in queued phase", time.Microsecond, scope),
			reservationGetFailureCount:     labeled.NewCounter("reservation_get_failure_count", "Reservation GetOrExtend failure count", scope),
			reservationGetSuccessCount:     labeled.NewCounter("reservation_get_success_count", "Reservation GetOrExtend success count", scope),
			reservationReleaseFailureCount: labeled.NewCounter("reservation_release_failure_count", "Reservation Release failure count", scope),
			reservationReleaseSuccessCount: labeled.NewCounter("reservation_release_success_count", "Reservation Release success count", scope),
			scope:                          scope,
		},
		pluginScope:     scope.NewSubScope("plugin"),
		kubeClient:      kubeClient,
		catalog:         client,
		asyncCatalog:    async,
		resourceManager: nil,
		secretManager:   secretmanager.NewFileEnvSecretManager(secretmanager.GetConfig()),
		barrierCache:    newLRUBarrier(ctx, cfg.BarrierConfig),
		cfg:             cfg,
		eventConfig:     eventConfig,
		clusterID:       clusterID,
	}, nil
}
