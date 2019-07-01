package driver

import (
	"context"
	"fmt"
	"log"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"

	"github.com/tombuildsstuff/golang-iis/iis"
)

const (
	// pluginName is the name of the plugin
	pluginName = "iis"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1
)

var (
	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{"0.1.0"},
		PluginVersion:     "0.0.1",
		Name:              pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a taskConfig within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"name": hclspec.NewAttr("name", "string", true),
		"pool": hclspec.NewAttr("pool", "string", false),
		"path": hclspec.NewAttr("path", "string", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: false,
		Exec:        true,
		FSIsolation: drivers.FSIsolationNone,
	}
)

// Driver is a driver for running IIS websites
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to rawExecDriverHandles
	// tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the IIS driver
	Enabled bool `codec:"enabled"`
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Name string `codec:"name"`
	Pool string `codec:"pool"`
	Path string `codec:"path"`
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	TaskConfig *drivers.TaskConfig
	Name       string
	Pool       string
	StartedAt  time.Time
	PID        int
}

// NewIISDriver returns a new DriverPlugin implementation
func NewIISDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &Driver{
		eventer: eventer.NewEventer(ctx, logger),
		config:  &Config{},
		// tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

// PluginInfo return a base.PluginInfoResponse struct
func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema return a hclspec.Spec struct
func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig set the nomad agent config based on base.Config
func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	return nil
}

// Shutdown the plugin
func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}

// TaskConfigSchema returns a hclspec.Spec struct
func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities a drivers.Capabilities struct
func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint return the plugin fingerprint
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	var health drivers.HealthState
	var desc string
	attrs := map[string]*pstructs.Attribute{}

	// Get the IIS version.
	iisVersion := "1"

	if d.config.Enabled && iisVersion != "" {
		health = drivers.HealthStateHealthy
		desc = "healthy"
		attrs["driver.iis"] = pstructs.NewBoolAttribute(true)
		attrs["driver.iis.version"] = pstructs.NewStringAttribute(iisVersion)
	} else {
		health = drivers.HealthStateUndetected
		desc = "disabled"
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}

// RecoverTask try to recover a failed task, if not return error
func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	// if handle == nil {
	// 	return fmt.Errorf("error: handle cannot be nil")
	// }

	// if _, ok := d.tasks.Get(handle.Config.ID); ok {
	// 	return nil
	// }

	// var taskState TaskState
	// if err := handle.GetDriverState(&taskState); err != nil {
	// 	return fmt.Errorf("failed to decode task state from handle: %v", err)
	// }

	// var driverConfig TaskConfig
	// if err := taskState.TaskConfig.DecodeDriverConfig(&driverConfig); err != nil {
	// 	return fmt.Errorf("failed to decode driver config: %v", err)
	// }

	// se := prepareContainer(handle.Config, driverConfig)
	// se.cachedir = d.config.IISCache

	// if err := se.startContainer(taskState.TaskConfig); err != nil {
	// 	return fmt.Errorf("unable to start container: %v", err)
	// }

	// h := &taskHandle{
	// 	syexec:     se,
	// 	pid:        se.containerPid,
	// 	taskConfig: taskState.TaskConfig,
	// 	procState:  drivers.TaskStateRunning,
	// 	startedAt:  time.Now().Round(time.Millisecond),
	// 	logger:     d.logger,
	// }
	// d.tasks.Set(taskState.TaskConfig.ID, h)

	// go h.run()
	return nil
}

// StartTask setup the task exec and calls the container excecutor
func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	// if _, ok := d.tasks.Get(cfg.ID); ok {
	// 	return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	// }

	// var driverConfig TaskConfig
	// if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
	// 	return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	// }

	// d.logger.Info("starting IIS task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	// handle := drivers.NewTaskHandle(taskHandleVersion)
	// handle.Config = cfg

	log.Printf("Creating CLient..")
	client, err := iis.NewClient()
	if err != nil {
		// return fmt.Errorf("Error building client: %s", err)
	}

	// rInt := helpers.RandomInt()
	name := fmt.Sprintf("app-pool-%s", cfg.ID)

	log.Printf("Creating the App Pool (with Name %q)..", name)
	err = client.AppPools.Create(name)
	if err != nil {
		// return fmt.Errorf("Error creating App Pool %q: %+v", name, err)
	}

	// se := prepareContainer(cfg, driverConfig)
	// se.cachedir = d.config.IISCache
	// se.logger = d.logger

	// if err := se.startContainer(cfg); err != nil {
	// 	return nil, nil, fmt.Errorf("unable to start container: %v", err)
	// }
	// d.logger.Info("IIS task deployed", "driver_cfg", hclog.Fmt("%+v", se.cmd.Args))

	// h := &taskHandle{
	// 	syexec:     se,
	// 	pid:        se.containerPid,
	// 	taskConfig: cfg,
	// 	procState:  drivers.TaskStateRunning,
	// 	startedAt:  time.Now().Round(time.Millisecond),
	// 	logger:     d.logger,
	// }

	// driverState := TaskState{
	// 	ContainerName: driverConfig.Image,
	// 	PID:           se.containerPid,
	// 	TaskConfig:    cfg,
	// 	StartedAt:     h.startedAt,
	// }

	// if err := handle.SetDriverState(&driverState); err != nil {
	// 	d.logger.Error("failed to start task, error setting driver state", "error", err)
	// 	return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	// }

	// if err := handle.SetDriverState(&driverState); err != nil {
	// 	d.logger.Error("failed to start task, error setting driver state", "error", err)
	// 	return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	// }

	// d.tasks.Set(cfg.ID, h)

	// go h.run()

	// return handle, nil, nil
	return nil, nil, nil
}

// WaitTask watis for task completion
func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	// handle, ok := d.tasks.Get(taskID)
	// if !ok {
	// 	return nil, drivers.ErrTaskNotFound
	// }

	// ch := make(chan *drivers.ExitResult)
	// go d.handleWait(ctx, handle, ch)

	// return ch, nil
	return nil, nil
}

// func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
// 	defer close(ch)

// 	ticker := time.NewTicker(1 * time.Second)
// 	defer ticker.Stop()

// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		case <-d.ctx.Done():
// 			return
// 		case <-ticker.C:
// 			s := handle.TaskStatus()
// 			if s.State == drivers.TaskStateExited {
// 				ch <- handle.exitResult
// 			}
// 		}
// 	}
// }

// StopTask shutdown a tasked based on its taskID
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	// handle, ok := d.tasks.Get(taskID)
	// if !ok {
	// 	return drivers.ErrTaskNotFound
	// }

	// if err := handle.shutdown(timeout); err != nil {
	// 	return fmt.Errorf("executor Shutdown failed: %v", err)
	// }

	return nil
}

// DestroyTask delete task
func (d *Driver) DestroyTask(taskID string, force bool) error {
	// handle, ok := d.tasks.Get(taskID)
	// if !ok {
	// 	return drivers.ErrTaskNotFound
	// }

	// if handle.IsRunning() && !force {
	// 	return fmt.Errorf("cannot destroy running task")
	// }

	// d.tasks.Delete(taskID)
	return nil
}

// InspectTask retrieves task info
func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	// handle, ok := d.tasks.Get(taskID)
	// if !ok {
	// 	return nil, drivers.ErrTaskNotFound
	// }

	// return handle.TaskStatus(), nil
	return nil, nil
}

// TaskStats get task stats
func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	// handle, ok := d.tasks.Get(taskID)
	// if !ok {
	// 	return nil, drivers.ErrTaskNotFound
	// }

	// return handle.stats(ctx, interval)
	return nil, nil
}

// TaskEvents return a chan *drivers.TaskEvent
func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// SignalTask send a specific signal to a taskID
func (d *Driver) SignalTask(taskID string, signal string) error {
	return fmt.Errorf("IIS driver does not support signals")
}

// ExecTask calls a exec cmd over a running task
func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, fmt.Errorf("IIS driver does not support exec") //TODO
}
