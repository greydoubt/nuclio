/*
Copyright 2023 The Nuclio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtime

import (
	"fmt"
	"os"

	"github.com/nuclio/nuclio/pkg/common/status"
	"github.com/nuclio/nuclio/pkg/processor/controlcommunication"
	"github.com/nuclio/nuclio/pkg/processor/databinding"

	"github.com/nuclio/errors"
	"github.com/nuclio/logger"
	"github.com/nuclio/nuclio-sdk-go"
)

// Runtime receives an event from a worker and passes it to a specific runtime like Golang, Python, et
type Runtime interface {

	// ProcessEvent receives the event and processes it at the specific runtime
	ProcessEvent(event nuclio.Event, functionLogger logger.Logger) (interface{}, error)

	// GetFunctionLogger returns the function logger
	GetFunctionLogger() logger.Logger

	// GetStatistics returns statistics gathered by the runtime
	GetStatistics() *Statistics

	// GetConfiguration returns the runtime configuration
	GetConfiguration() *Configuration

	// SetStatus sets the runtime's reported status
	SetStatus(newStatus status.Status)

	// GetStatus returns the runtime's reported status
	GetStatus() status.Status

	// Start starts the runtime, or does nothing if the runtime does not require starting (e.g. Go and shell runtimes)
	Start() error

	// Stop stops the runtime
	Stop() error

	// Restart restarts the runtime
	Restart() error

	// SupportsRestart return true if the runtime supports restart
	SupportsRestart() bool

	// Drain signals to the runtime process to drain its accumulated events and waits for it to finish
	Drain() error

	// GetControlMessageBroker returns the control message broker
	GetControlMessageBroker() controlcommunication.ControlMessageBroker
}

// AbstractRuntime is the base for all runtimes
type AbstractRuntime struct {
	Logger               logger.Logger
	FunctionLogger       logger.Logger
	Context              *nuclio.Context
	Statistics           Statistics
	ControlMessageBroker controlcommunication.ControlMessageBroker
	databindings         map[string]databinding.DataBinding
	configuration        *Configuration
	status               status.Status
}

// NewAbstractRuntime creates a new abstract runtime
func NewAbstractRuntime(logger logger.Logger, configuration *Configuration) (*AbstractRuntime, error) {
	var err error

	newAbstractRuntime := AbstractRuntime{
		Logger:         logger,
		FunctionLogger: configuration.FunctionLogger,
		configuration:  configuration,
	}

	// set some environment variables
	if err = os.Setenv("NUCLIO_HANDLER", configuration.Spec.Handler); err != nil {
		return nil, errors.Wrap(err, "Failed to set handler env")
	}

	// create data bindings and start them (connecting to the actual data sources)
	newAbstractRuntime.databindings, err = newAbstractRuntime.createAndStartDataBindings(logger, configuration)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create data bindings")
	}

	newAbstractRuntime.Context, err = newAbstractRuntime.createContext(newAbstractRuntime.FunctionLogger,
		configuration,
		newAbstractRuntime.databindings)

	if err != nil {
		return nil, errors.Wrap(err, "Failed to create context")
	}

	// set the initial status
	newAbstractRuntime.status = status.Initializing

	return &newAbstractRuntime, nil
}

// GetFunctionLogger returns the function logger
func (ar *AbstractRuntime) GetFunctionLogger() logger.Logger {
	return ar.FunctionLogger
}

// GetConfiguration returns the runtime configuration
func (ar *AbstractRuntime) GetConfiguration() *Configuration {
	return ar.configuration
}

// GetStatistics returns statistics gathered by the runtime
func (ar *AbstractRuntime) GetStatistics() *Statistics {
	return &ar.Statistics
}

// SetStatus sets the runtime's reported status
func (ar *AbstractRuntime) SetStatus(newStatus status.Status) {
	ar.status = newStatus
}

// GetStatus returns the runtime's reported status
func (ar *AbstractRuntime) GetStatus() status.Status {
	return ar.status
}

// Start starts the runtime, or does nothing if the runtime does not require starting (e.g. Go and shell runtimes)
func (ar *AbstractRuntime) Start() error {
	return nil
}

// Restart restarts the runtime
func (ar *AbstractRuntime) Restart() error {
	runtimeName := ar.GetConfiguration().Spec.Runtime
	return errors.Errorf("Runtime %s does not support restart", runtimeName)
}

// SupportsRestart returns true if the runtime supports restart
func (ar *AbstractRuntime) SupportsRestart() bool {
	return false
}

// SupportsControlCommunication returns true if the runtime supports control communication
func (ar *AbstractRuntime) SupportsControlCommunication() bool {
	return false
}

func (ar *AbstractRuntime) GetEnvFromConfiguration() []string {
	return []string{
		fmt.Sprintf("NUCLIO_FUNCTION_NAME=%s", ar.configuration.Meta.Name),
		fmt.Sprintf("NUCLIO_FUNCTION_DESCRIPTION=%s", ar.configuration.Spec.Description),
		fmt.Sprintf("NUCLIO_FUNCTION_VERSION=%d", ar.configuration.Spec.Version),
		fmt.Sprintf("NUCLIO_FUNCTION_HANDLER=%s", ar.configuration.Spec.Handler),
	}
}

// GetControlMessageBroker returns the control message broker
func (ar *AbstractRuntime) GetControlMessageBroker() controlcommunication.ControlMessageBroker {
	return ar.ControlMessageBroker
}

func (ar *AbstractRuntime) createAndStartDataBindings(parentLogger logger.Logger,
	configuration *Configuration) (map[string]databinding.DataBinding, error) {

	databindings := map[string]databinding.DataBinding{}

	// create data bindings through the data binding registry
	// TODO: this should be in parallel
	for dataBindingName, dataBindingConfiguration := range configuration.Spec.DataBindings {

		// There was an error in the initial implementation of databinding where "kind" was mistaken for "class". This
		// patch makes it so that if the user declared "kind" (as he should) it will use that to determine the kind
		// of databinding. If not, check the "class" field. This patch will be in until all examples / demos are
		// migrated
		kind := dataBindingConfiguration.Kind
		if kind == "" {
			kind = dataBindingConfiguration.Class
		}

		databindingInstance, err := databinding.RegistrySingleton.NewDataBinding(parentLogger,
			kind,
			dataBindingConfiguration.Name,
			&dataBindingConfiguration)

		if err != nil {
			return nil, errors.Wrap(err, "Failed to create data binding")
		}

		if err := databindingInstance.Start(); err != nil {
			return nil, errors.Wrap(err, "Failed to start data binding")
		}

		databindings[dataBindingName] = databindingInstance
	}

	return databindings, nil
}

func (ar *AbstractRuntime) createContext(parentLogger logger.Logger,
	configuration *Configuration,
	databindings map[string]databinding.DataBinding) (*nuclio.Context, error) {
	var err error

	newContext := &nuclio.Context{
		Logger:          parentLogger,
		DataBinding:     map[string]nuclio.DataBinding{},
		WorkerID:        configuration.WorkerID,
		FunctionName:    configuration.Meta.Name,
		FunctionVersion: configuration.Spec.Version,
		TriggerKind:     configuration.TriggerKind,
		TriggerName:     configuration.TriggerName,
	}

	if newContext.Platform, err = nuclio.NewPlatform(parentLogger,
		ar.configuration.PlatformConfig.Kind,
		ar.configuration.Meta.Namespace,
	); err != nil {
		return nil, errors.Wrap(err, "Failed to initialize Platform")
	}

	// iterate through data bindings and get the context object - the thing users will actuall
	// work with in the handlers
	for databindingName, databindingInstance := range databindings {
		newContext.DataBinding[databindingName], err = databindingInstance.GetContextObject()
		if err != nil {
			return nil, errors.Wrap(err, "Failed to get databinding context object")
		}
	}

	return newContext, nil
}

// Stop stops the runtime
func (ar *AbstractRuntime) Stop() error {
	ar.SetStatus(status.Stopped)
	return nil
}

func (ar *AbstractRuntime) Drain() error {
	return nil
}
